// Package usage reads the tokens a provider says an answer used, prices them, and keeps daily totals per client and
// model in Redis (DESIGN.md, "Accounting").
//
// Money is whole picodollars (10^-12 USD) in an int64, never a float: a price of $0.15 per million tokens is 150,000
// picodollars a token, and a cost is tokens times that, exactly. The bounds make overflow impossible: a price is at
// most $1,000 per million (10^9 picodollars a token) and a count at most 10^9 tokens, so each product is at most
// 10^18, and the sum of two is below the int64 limit of 9.2 × 10^18.
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vivekdavara/switchboard/internal/config"
)

// MaxTokens bounds a count the gateway believes: a provider's usage above it is taken as no usage at all.
const MaxTokens = 1_000_000_000

// Usage is the tokens one answer used, as the provider reported them.
type Usage struct {
	Prompt, Completion int64
}

// usageOf reads a "usage" object: both counts must be there, whole, and from 0 to MaxTokens.
func usageOf(raw json.RawMessage) (Usage, bool) {
	var u struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil || u.Prompt == nil || u.Completion == nil {
		return Usage{}, false
	}
	if *u.Prompt < 0 || *u.Prompt > MaxTokens || *u.Completion < 0 || *u.Completion > MaxTokens {
		return Usage{}, false
	}
	return Usage{*u.Prompt, *u.Completion}, true
}

// FromAnswer reads the usage of a completion answered in one piece: its top-level "usage" object.
func FromAnswer(body []byte) (Usage, bool) {
	var a struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &a) != nil || len(a.Usage) == 0 {
		return Usage{}, false
	}
	return usageOf(a.Usage)
}

// FromChunk reads the usage a stream's chunk carries, if any: OpenAI sends it in a last chunk of its own (choices
// empty) when stream_options.include_usage asks; other providers send it with the last choices.
func FromChunk(data []byte) (Usage, bool) {
	if !bytes.Contains(data, []byte(`"usage"`)) { // the cheap test first: most chunks have none
		return Usage{}, false
	}
	return FromAnswer(data)
}

// Cost is what u cost at price p, in picodollars.
func Cost(u Usage, p config.Price) int64 {
	return u.Prompt*p.Input + u.Completion*p.Output
}

// USD writes picodollars as dollars, exactly: as many decimal places as it takes, at most 12.
func USD(pico int64) string {
	sign := ""
	if pico < 0 {
		sign, pico = "-", -pico
	}
	whole, frac := pico/1_000_000_000_000, pico%1_000_000_000_000
	if frac == 0 {
		return sign + strconv.FormatInt(whole, 10)
	}
	return sign + strconv.FormatInt(whole, 10) + "." +
		strings.TrimRight(fmt.Sprintf("%012d", frac), "0")
}

// Retention is how long a day's totals are kept after the last request that added to them.
const Retention = 90 * 24 * time.Hour

// Entry is what one request adds to its client's totals for its model on its day.
type Entry struct {
	Completions int64 // answers a provider gave (a 200, or a stream that ended with [DONE])
	CacheHits   int64 // answers the cache gave: no tokens spent
	Tokens      Usage
	Cost        int64 // picodollars
	Unpriced    int64 // completions with a usage but no price for their provider's model
	NoUsage     int64 // completions without a usage
}

// Totals keeps the daily totals in Redis, one hash per day, client and model.
type Totals struct {
	rdb    redis.Cmdable
	prefix string
}

// NewTotals keeps totals in rdb, under prefix.
func NewTotals(rdb redis.Cmdable, prefix string) *Totals {
	return &Totals{rdb: rdb, prefix: prefix}
}

// key is the hash of a day (UTC, as 2026-10-20), a client and a model. Client names can't hold ':', and a day has a
// fixed form, so a key splits back into its parts even when the model's name has colons of its own.
func (t *Totals) key(day, client, model string) string {
	return t.prefix + "usage:" + day + ":" + client + ":" + model
}

// Day is the day a time is counted on: its date in UTC.
func Day(at time.Time) string { return at.UTC().Format(time.DateOnly) }

// fieldsOf are a hash's fields, in the order a report shows them.
var fieldsOf = []string{"completions", "cache_hits", "prompt_tokens", "completion_tokens", "cost_picousd", "unpriced",
	"no_usage"}

// Add adds an entry to the totals, in one round trip: a MULTI, so no report sees a request half added. (A MULTI isn't
// all or nothing: a field that isn't a number would fail its own HINCRBY and not the others'. Only Redis's own
// tools could make one.)
func (t *Totals) Add(ctx context.Context, at time.Time, client, model string, e Entry) error {
	key := t.key(Day(at), client, model)
	_, err := t.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for i, v := range []int64{e.Completions, e.CacheHits, e.Tokens.Prompt, e.Tokens.Completion, e.Cost, e.Unpriced,
			e.NoUsage} {
			if v != 0 {
				p.HIncrBy(ctx, key, fieldsOf[i], v)
			}
		}
		p.Expire(ctx, key, Retention)
		return nil
	})
	return err
}

// Row is one client's totals for one model on one day.
type Row struct {
	Day, Client, Model string
	Entry
}

// Report returns a day's totals, by client and then model.
func (t *Totals) Report(ctx context.Context, day string) ([]Row, error) {
	var rows []Row
	head := t.prefix + "usage:" + day + ":"
	seen := map[string]bool{}
	iter := t.rdb.Scan(ctx, 0, escapeGlob(head)+"*", 1000).Iterator()
	for iter.Next(ctx) {
		// SCAN may return a key more than once (Redis's documentation says so: a rehash during the scan); a key read
		// twice would be counted twice (a suspicion of the day-3 review).
		if seen[iter.Val()] {
			continue
		}
		seen[iter.Val()] = true
		client, model, ok := strings.Cut(strings.TrimPrefix(iter.Val(), head), ":")
		if !ok {
			continue
		}
		h, err := t.rdb.HGetAll(ctx, iter.Val()).Result()
		if err != nil {
			return nil, err
		}
		r := Row{Day: day, Client: client, Model: model}
		for i, into := range []*int64{&r.Completions, &r.CacheHits, &r.Tokens.Prompt, &r.Tokens.Completion, &r.Cost,
			&r.Unpriced, &r.NoUsage} {
			if v, ok := h[fieldsOf[i]]; ok {
				if *into, err = strconv.ParseInt(v, 10, 64); err != nil {
					return nil, fmt.Errorf("%s %s: %w", iter.Val(), fieldsOf[i], err)
				}
			}
		}
		rows = append(rows, r)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Client != rows[j].Client {
			return rows[i].Client < rows[j].Client
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, nil
}

// escapeGlob escapes the characters SCAN's MATCH reads as a pattern, so a prefix is matched as itself.
func escapeGlob(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
