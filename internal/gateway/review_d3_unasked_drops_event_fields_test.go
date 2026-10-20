// Review (day 3): taking "usage": null out of a chunk also throws away the event's other lines.
//
// When the gateway asks for a stream's usage the client didn't ask for, the provider puts "usage": null on every
// chunk, so unasked (accounting.go) rewrites every chunk as one `data: ` line. Everything else in the event goes:
// an `id:` line (which a reconnecting client sends back as Last-Event-ID), an `event:` line (the OpenAI SDKs read a
// named event differently from an unnamed one: Python's Stream wraps it as {"data": ..., "event": ...}), and comment
// lines. DESIGN.md, "Streaming", promises every event "byte for byte"; day 3's comment says only the usage is taken
// out. The client that didn't ask for usage gets a different stream than it would have without the gateway's asking
// (the same provider with stream_usage off relays these events as they came).
package gateway

import (
	"strings"
	"testing"
)

func TestReviewD3TakingUsageOutKeepsTheEventsOtherLines(t *testing.T) {
	ev := "id: 7\nevent: chunk\n: keep-alive note\n" +
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}],"usage":null}` + "\n\n"
	up := newProvider(t, streaming([]string{ev, "data: [DONE]\n\n"}, 0, false))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x") // stream_usage on: the gateway asks
	_, got, err := readStream(t, srv, askStream)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for _, line := range []string{"id: 7\n", "event: chunk\n", ": keep-alive note\n"} {
		if !strings.Contains(got[0].raw, line) {
			t.Errorf("the line %q of the event is gone: the client got %q", line, got[0].raw)
		}
	}
}
