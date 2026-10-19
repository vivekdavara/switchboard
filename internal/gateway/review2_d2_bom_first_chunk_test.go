// Found by the second day-2 review (at ff95f4f): the event reader doesn't strip the byte order mark the SSE spec allows
// at the start of a stream ("stream = [ bom ] *event"), so a BOM-prefixed first chunk isn't a data event; since 10ff60e
// a one-chunk stream that starts with a BOM is refused as "[DONE] before its first chunk" (502, a breaker failure).
package gateway

import "testing"

func TestReview2D2AStreamThatStartsWithAByteOrderMarkIsAnAnswer(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	up := newProvider(t, streaming([]string{bom + chunk, "data: [DONE]\n\n"}, 0, false))
	srv, g, _ := gatewayFor(t, `"circuit_breaker":{"failures":1}`, []string{at(up, "")}, "m=p0/x")
	resp, events, err := readStream(t, srv, askStream)
	if err != nil || resp.StatusCode != 200 || len(events) != 2 {
		t.Errorf("%d, %d events, %v; want the provider's one chunk, then [DONE]", resp.StatusCode, len(events), err)
	}
	if got := g.breakers["p0"].State().String(); got != "closed" {
		t.Errorf("p0's breaker is %s after a whole answer", got)
	}
}
