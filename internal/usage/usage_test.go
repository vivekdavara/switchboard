package usage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/redistest"
)

func TestTheUsageOfAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Usage
		ok   bool
	}{
		{`{"id":"x","usage":{"prompt_tokens":5,"completion_tokens":32,"total_tokens":37}}`, Usage{5, 32}, true},
		{`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, Usage{}, true},
		{`{"usage":{"prompt_tokens":1000000000,"completion_tokens":1}}`, Usage{1_000_000_000, 1}, true},
		{`{"usage":{"prompt_tokens":1000000001,"completion_tokens":1}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":1,"completion_tokens":1000000001}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":1,"completion_tokens":-1}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":-1,"completion_tokens":1}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":1.5,"completion_tokens":1}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":"5","completion_tokens":1}}`, Usage{}, false},
		{`{"usage":{"prompt_tokens":5}}`, Usage{}, false},
		{`{"usage":{"completion_tokens":5}}`, Usage{}, false},
		{`{"usage":null}`, Usage{}, false},
		{`{"usage":[]}`, Usage{}, false},
		{`{"id":"x"}`, Usage{}, false},
		{`not json`, Usage{}, false},
	} {
		got, ok := FromAnswer([]byte(tc.body))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %+v %v, want %+v %v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTheUsageOfAChunk(t *testing.T) {
	for _, tc := range []struct {
		data string
		want Usage
		ok   bool
	}{
		{`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":32,"total_tokens":37}}`, Usage{5, 32}, true},
		{`{"choices":[{"index":0,"delta":{"content":" word"}}],"usage":null}`, Usage{}, false},
		{`{"choices":[{"index":0,"delta":{"content":" \"usage\""}}]}`, Usage{}, false},
		{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
			Usage{2, 3}, true},
	} {
		got, ok := FromChunk([]byte(tc.data))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %+v %v, want %+v %v", tc.data, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCostsAreExactAndWrittenExactly(t *testing.T) {
	gpt4oMini := config.Price{Input: 150_000, Output: 600_000} // $0.15 and $0.60 per million
	for _, tc := range []struct {
		u    Usage
		p    config.Price
		pico int64
		usd  string
	}{
		{Usage{5, 32}, gpt4oMini, 19_950_000, "0.00001995"},
		{Usage{1_000_000, 1_000_000}, gpt4oMini, 750_000_000_000, "0.75"},
		{Usage{0, 0}, gpt4oMini, 0, "0"},
		{Usage{1, 0}, config.Price{Input: 1}, 1, "0.000000000001"},
		{Usage{MaxTokens, MaxTokens}, config.Price{Input: 1_000_000_000, Output: 1_000_000_000},
			2_000_000_000_000_000_000, "2000000"},
		{Usage{3, 7}, config.Price{Input: 2_500_000, Output: 10_000_000}, 77_500_000, "0.0000775"},
	} {
		if got := Cost(tc.u, tc.p); got != tc.pico {
			t.Errorf("%+v at %+v: %d, want %d", tc.u, tc.p, got, tc.pico)
		}
		if got := USD(tc.pico); got != tc.usd {
			t.Errorf("USD(%d) = %s, want %s", tc.pico, got, tc.usd)
		}
	}
	if got := USD(-1_500_000_000_000); got != "-1.5" {
		t.Errorf("USD(-1.5 dollars) = %s", got)
	}
}

func TestTotalsAddUpPerDayClientAndModel(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	totals := NewTotals(rdb, prefix)
	ctx := context.Background()
	day := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
	next := day.Add(2 * time.Minute)
	for _, add := range []struct {
		at            time.Time
		client, model string
		e             Entry
	}{
		{day, "alice", "fast", Entry{Completions: 1, Tokens: Usage{5, 32}, Cost: 19_950_000}},
		{day, "alice", "fast", Entry{Completions: 1, Tokens: Usage{7, 10}, Cost: 7_050_000}},
		{day, "alice", "fast", Entry{CacheHits: 1}},
		{day, "alice", "org/model:v2", Entry{Completions: 1, Tokens: Usage{1, 2}, Unpriced: 1}},
		{day, "bob", "fast", Entry{Completions: 1, NoUsage: 1}},
		{next, "alice", "fast", Entry{Completions: 1, Tokens: Usage{1, 1}, Cost: 750_000}},
	} {
		if err := totals.Add(ctx, add.at, add.client, add.model, add.e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := totals.Report(ctx, "2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"2026-10-20", "alice", "fast", Entry{Completions: 2, CacheHits: 1, Tokens: Usage{12, 42}, Cost: 27_000_000}},
		{"2026-10-20", "alice", "org/model:v2", Entry{Completions: 1, Tokens: Usage{1, 2}, Unpriced: 1}},
		{"2026-10-20", "bob", "fast", Entry{Completions: 1, NoUsage: 1}},
	}
	if len(rows) != len(want) {
		t.Fatalf("%+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, rows[i], want[i])
		}
	}
	if rows, _ := totals.Report(ctx, "2026-10-21"); len(rows) != 1 || rows[0].Tokens != (Usage{1, 1}) {
		t.Errorf("the next day (UTC): %+v", rows)
	}
	ttl := rdb.TTL(ctx, totals.key("2026-10-20", "alice", "fast")).Val()
	if ttl < Retention-time.Minute || ttl > Retention {
		t.Errorf("TTL %s, want %s", ttl, Retention)
	}
}

func TestAPrefixIsMatchedAsItself(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	ctx := context.Background()
	star, plain := NewTotals(rdb, prefix+"a*:"), NewTotals(rdb, prefix+"ab:")
	star.Add(ctx, time.Now(), "c", "m", Entry{Completions: 1})
	plain.Add(ctx, time.Now(), "c", "m", Entry{Completions: 2})
	rows, err := star.Report(ctx, Day(time.Now()))
	if err != nil || len(rows) != 1 || rows[0].Completions != 1 {
		t.Errorf("a prefix with a * reads only its own keys: %+v %v", rows, err)
	}
}

func TestAReportSkipsKeysItCantSplitAndRefusesFieldsItCantRead(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	totals := NewTotals(rdb, prefix)
	ctx := context.Background()
	rdb.HSet(ctx, prefix+"usage:2026-10-20:no-model", "completions", 1) // no ":" after the client
	totals.Add(ctx, time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC), "c", "m", Entry{Completions: 3})
	rows, err := totals.Report(ctx, "2026-10-20")
	if err != nil || len(rows) != 1 || rows[0].Client != "c" || rows[0].Completions != 3 {
		t.Errorf("%+v %v", rows, err)
	}
	rdb.HSet(ctx, prefix+"usage:2026-10-20:c:m", "prompt_tokens", "lots")
	if _, err := totals.Report(ctx, "2026-10-20"); err == nil || !strings.Contains(err.Error(), "prompt_tokens") {
		t.Errorf("a field that isn't a number: %v", err)
	}
}

// names records the commands of every pipeline a client sends.
type names struct {
	mu   sync.Mutex
	seen [][]string
}

func (n *names) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (n *names) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (n *names) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		var got []string
		for _, c := range cmds {
			got = append(got, c.Name())
		}
		n.mu.Lock()
		n.seen = append(n.seen, got)
		n.mu.Unlock()
		return next(ctx, cmds)
	}
}

func TestAnEntryIsAddedInOneTransaction(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	hook := &names{}
	rdb.AddHook(hook)
	NewTotals(rdb, prefix).Add(context.Background(), time.Now(), "c", "m",
		Entry{Completions: 1, Tokens: Usage{1, 2}, Cost: 3})
	if len(hook.seen) != 1 || strings.Join(hook.seen[0], " ") !=
		"multi hincrby hincrby hincrby hincrby expire exec" {
		t.Errorf("%q: one MULTI, the fields that changed, the expiry", hook.seen)
	}
}

// twice is a Redis whose SCAN returns every key two times, as SCAN may (a rehash during the scan).
type twice struct {
	redis.Cmdable
}

func (t twice) Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd {
	keys := t.Cmdable.Keys(ctx, match).Val()
	return redis.NewScanCmdResult(append(keys, keys...), 0, nil)
}

func TestAKeyScannedTwiceIsCountedOnce(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	NewTotals(rdb, prefix).Add(context.Background(), at, "c", "m", Entry{Completions: 2, Cost: 5})
	rows, err := NewTotals(twice{rdb}, prefix).Report(context.Background(), "2026-10-20")
	if err != nil || len(rows) != 1 || rows[0].Completions != 2 || rows[0].Cost != 5 {
		t.Errorf("%+v %v", rows, err)
	}
}
