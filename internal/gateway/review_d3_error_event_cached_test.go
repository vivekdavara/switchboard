// Review (day 3): a stream in which the provider sent an error event mid-way is cached and counted as a completion.
//
// DESIGN.md, "What day 2 leaves for them to respect": a cache may store only an answered 200, never a failure or a
// cut stream; caching.go says "never an error or a broken stream". But chat stores every stream that reaches
// data: [DONE] (gateway.go, the res.written branch), without looking at what came before it. vLLM's OpenAI server
// does exactly this on an engine error: it yields `data: {"error": {...}}` and then `data: [DONE]`. The first client
// rightly sees the error (the SDK raises it); every identical request for the next ttl is then served the same error
// from the cache (X-Switchboard-Cache: hit, the provider not asked). The totals also count it as a completion (logged,
// not asserted).
package gateway

import (
	"strings"
	"testing"
)

func TestReviewD3AStreamWithAnErrorEventIsNotCached(t *testing.T) {
	redis, totals := redisTotals(t)
	errorEvent := `data: {"error":{"message":"the engine failed mid-stream","type":"server_error",` +
		`"code":"server_error"}}` + "\n\n"
	up := newProvider(t, streaming([]string{chunk, errorEvent, "data: [DONE]\n\n"}, 0, false))
	srv, _, _ := gatewayFor(t, redis, []string{at(up, `"stream_usage":false`)},
		`{"name":"m","provider":"p0","upstream_model":"x","cache":{"ttl":"1m"}}`)

	_, first, _ := readStream(t, srv, askStream)
	sawError := false
	for _, ev := range first {
		sawError = sawError || strings.Contains(ev.raw, "the engine failed mid-stream")
	}
	if !sawError {
		t.Fatalf("the first client didn't get the provider's error event: %v", first)
	}

	resp, second, _ := readStream(t, srv, askStream)
	if got := resp.Header.Get("X-Switchboard-Cache"); got == "hit" {
		t.Errorf("a stream with the provider's error event in it was cached: the second request is a %s, "+
			"served %d events from the cache, the error among them", got, len(second))
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("the provider was asked %d times, not 2: the failure was answered from the cache", n)
	}
	// Not asserted (the totals' own comment counts "a stream that ended with [DONE]"), but shown: the failure is a
	// completion, and the hit a cache hit.
	t.Logf("totals: %+v", today(t, totals))
}
