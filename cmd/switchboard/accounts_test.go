package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/redistest"
	"github.com/vivekdavara/switchboard/internal/usage"
)

var keygenOutput = regexp.MustCompile(`^key        (sb-[A-Za-z0-9_-]{43})\nkey_sha256 ([0-9a-f]{64})\n$`)

func TestKeygenPrintsAKeyAndTheHashTheConfigHolds(t *testing.T) {
	keys := map[string]bool{}
	for range 3 {
		var out bytes.Buffer
		if code := run(context.Background(), []string{"keygen"}, noEnv, &out, io.Discard, nil); code != 0 {
			t.Fatalf("exit %d", code)
		}
		m := keygenOutput.FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("%q", out.String())
		}
		sum := sha256.Sum256([]byte(m[1]))
		if hex.EncodeToString(sum[:]) != m[2] {
			t.Errorf("%s isn't the SHA-256 of %s", m[2], m[1])
		}
		cfg, err := config.Parse([]byte(`{"clients":[{"name":"c","key_sha256":"`+m[2]+`"}],`+
			`"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]}`), noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := cfg.Client(m[1]); !ok || c.Name != "c" {
			t.Errorf("the configuration doesn't know the key it was given the hash of")
		}
		keys[m[1]] = true
	}
	if len(keys) != 3 {
		t.Errorf("three keys, %d different", len(keys))
	}
}

func TestCheckPrintsDay3(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"check", "-config", "../../configs/clients.example.json"}, noEnv,
		&out, io.Discard, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"model fast (also gpt-4o-mini) -> mock-a as mock-small (weight 1); no fallback; cached for 10m0s\n",
		`redis 127.0.0.1:6382, prefix "switchboard:", timeout 100ms, rate-limited requests when it is down: allow` + "\n",
		"client alice: 60 requests a minute, 10 at once\nclient bob: no rate limit\n",
		"price mock-a mock-large: $2.5 in, $10 out per million tokens\n" +
			"price mock-a mock-small: $0.15 in, $0.6 out per million tokens\n" +
			"price mock-b mock-large: $2.5 in, $10 out per million tokens\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}

func TestUsagePrintsADaysTotals(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	totals := usage.NewTotals(rdb, prefix)
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	totals.Add(ctx, at, "alice", "fast", usage.Entry{Completions: 2, CacheHits: 1,
		Tokens: usage.Usage{Prompt: 10, Completion: 64}, Cost: 39_900_000})
	totals.Add(ctx, at, "bob", "smart", usage.Entry{Completions: 1, Tokens: usage.Usage{Prompt: 5, Completion: 32},
		Cost: 332_500_000})
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"redis":{"addr":%q,"prefix":%q},"providers":[{"name":"p",`+
		`"base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]}`, redistest.Addr(t), prefix)), 0o600)
	var out, errs bytes.Buffer
	if code := run(ctx, []string{"usage", "-config", path, "-day", "2026-10-20"}, noEnv, &out, &errs, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	want := `2026-10-20 (UTC), 2 client and model pairs
  client  model  completions  cache hits  prompt tokens  completion tokens  cost (USD)  unpriced  no usage
   alice   fast            2           1             10                 64   0.0000399         0         0
     bob  smart            1           0              5                 32   0.0003325         0         0
total cost 0.0003724 USD
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	out.Reset()
	run(ctx, []string{"usage", "-config", path, "-day", "2026-10-21"}, noEnv, &out, io.Discard, nil)
	if !strings.HasPrefix(out.String(), "2026-10-21 (UTC), 0 client and model pairs\n") {
		t.Errorf("another day: %s", out.String())
	}
}
