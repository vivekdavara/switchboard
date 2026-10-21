package gateway

// Bug: a cache lookup that fails (x.cache "unavailable", x.cacheErr set) is followed by keeping the answer, and store
// assigns x.cacheErr = err unconditionally. When the write succeeds, the lookup's error is overwritten with nil: the
// log line has cache "unavailable" but no cache_err, is logged at INFO (day 4's rule: "a chat completion's line is
// WARN ... when a Redis step failed"), and switchboard_redis_errors_total{step="cache"} isn't counted ("once per
// failing request and step"). TestRedisFailuresAreCountedPerStep passes only because there both steps fail. Here
// the GET fails deterministically (WRONGTYPE: the key holds a hash) and the SET that replaces it succeeds; a lookup
// that times out on a slow Redis and a write that doesn't is the same path.

import (
	"context"
	"fmt"
	"testing"

	"github.com/vivekdavara/switchboard/internal/cache"
	"github.com/vivekdavara/switchboard/internal/openai"
	"github.com/vivekdavara/switchboard/internal/redistest"
)

func TestReviewD4AFailedCacheLookupStaysAFailureWhenTheAnswerIsKept(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"prefix":%q}`, redistest.Addr(t), prefix),
		[]string{at(up, "")}, cached("fast", `{"ttl":"1m"}`))

	req, e := openai.ParseChatRequest([]byte(ask))
	if e != nil {
		t.Fatal(e)
	}
	key := prefix + "cache:" + cache.Key(clientName(nil), "fast", req.WithModel("fast"))
	if err := rdb.HSet(context.Background(), key, "not", "a string").Err(); err != nil {
		t.Fatal(err)
	}

	if resp, b := send(t, srv, ask); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	lines := logLines(t, logs, "chat completion")
	if len(lines) != 1 || lines[0]["cache"] != "unavailable" {
		t.Fatalf("the lookup didn't fail as set up: %v", lines)
	}
	if lines[0]["cache_err"] == nil || lines[0]["level"] != "WARN" {
		t.Errorf("a failed lookup logged without its error, at %v: %v", lines[0]["level"], lines[0])
	}
	want(t, scrape(t, g), map[string]float64{
		`switchboard_cache_total{model="fast",result="unavailable"}`: 1,
		`switchboard_redis_errors_total{step="cache"}`:               1,
	})
}
