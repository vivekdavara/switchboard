package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/gateway"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// keygen prints a new client key and the SHA-256 the configuration holds for it. The key is 32 random bytes, so
// nobody can guess it and its hash is safe to check in; it is printed once and kept nowhere.
func keygen(stdout io.Writer) int {
	var b [32]byte
	rand.Read(b[:]) // never fails (crypto/rand panics rather than return an error since Go 1.24)
	key := "sb-" + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(key))
	fmt.Fprintf(stdout, "key        %s\nkey_sha256 %s\n", key, hex.EncodeToString(sum[:]))
	return 0
}

// report prints a day's totals from the configuration's Redis: per client and model, completions, cache hits,
// tokens and cost.
func report(ctx context.Context, cfg *config.Config, day string, stdout, stderr io.Writer) int {
	// The command line first: a bad -day is a usage error (2) whatever the file says (it was 1 for a file with no
	// Redis until the day-5 review).
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		fmt.Fprintf(stderr, "switchboard: -day %q is not a date such as 2026-10-20\n", day)
		return 2
	}
	if cfg.Redis == nil {
		fmt.Fprintln(stderr, "switchboard: the configuration names no redis, so there are no totals")
		return 1
	}
	rdb := gateway.NewRedis(cfg.Redis)
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // a report reads many keys: not one command's timeout
	defer cancel()
	rows, err := usage.NewTotals(rdb, cfg.Redis.Prefix).Report(ctx, day)
	if err != nil {
		fmt.Fprintln(stderr, "switchboard: reading the totals:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s (UTC), %d client and model pairs\n", day, len(rows))
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "client\tmodel\tcompletions\tcache hits\tprompt tokens\tcompletion tokens\tcost (USD)\tunpriced\t"+
		"no usage\t")
	var total int64
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%d\t%d\t\n", r.Client, r.Model, r.Completions, r.CacheHits,
			r.Tokens.Prompt, r.Tokens.Completion, usage.USD(r.Cost), r.Unpriced, r.NoUsage)
		total += r.Cost
	}
	tw.Flush()
	fmt.Fprintf(stdout, "total cost %s USD\n", usage.USD(total))
	return 0
}
