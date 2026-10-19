// Found by the second day-2 review (at ff95f4f): relay reports a stream's ticket as a success at its first event,
// which resets the breaker's failures in a row, so the Breaker.Fail of its later break is never more than the first
// in a row: a provider that breaks every stream after its first chunk never opens its breaker (failures >= 2).
package gateway

import (
	"testing"
)

func TestReview2D2StreamsThatAllBreakAfterTheirFirstChunkOpenTheBreaker(t *testing.T) {
	// Every stream: one chunk, then the connection drops (upstream_unavailable, a failure after the commit).
	up := newProvider(t, streaming([]string{chunk}, 0, true))
	srv, g, _ := gatewayFor(t, `"circuit_breaker":{"failures":2,"cooldown":"1m"}`, []string{at(up, "")}, "m=p0/x")
	for range 4 {
		readStream(t, srv, askStream)
	}
	if got := g.breakers["p0"].State().String(); got != "open" {
		t.Errorf("after 4 streams in a row that broke, p0's breaker is %s; failures 2 should have opened it", got)
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("p0 was sent %d of the 4 requests; with failures 2, the breaker should have skipped it after 2", n)
	}

	// The same provider failing plain requests opens it after 2, as it should: only the streamed path is blind.
	plain := newProvider(t, answering(500, `{}`))
	srv, g, _ = gatewayFor(t, `"circuit_breaker":{"failures":2,"cooldown":"1m"}`, []string{at(plain, "")}, "m=p0/x")
	for range 4 {
		send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	}
	if got := g.breakers["p0"].State().String(); got != "open" || len(plain.requests()) != 2 {
		t.Fatalf("plain: breaker %s after %d requests reached p0", got, len(plain.requests()))
	}
}
