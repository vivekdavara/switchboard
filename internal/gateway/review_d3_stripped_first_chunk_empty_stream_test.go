// Review (day 3): taking the usage out can turn a stream into the "[DONE] with nothing before it" 200 that day 2
// refuses to commit (an edge, not an ordinary path: it takes a provider whose first data event is a usage-only chunk).
//
// relay decides whether the first data event is an answer (a JSON chunk, not an error, not [DONE]) and commits on it,
// and only then asks unasked what to send; unasked drops a usage-only chunk. So a stream whose first chunk is the
// usage commits a 200, sends nothing for that event, and if [DONE] comes next the client gets exactly the stream
// DESIGN.md, "Streaming", says the gateway never commits ("A [DONE]-only stream committed an empty 200 until the
// day-2 review found it"). The breaker is told the provider succeeded, the totals count a completion, and with a
// cache it is stored and served again.
package gateway

import (
	"testing"
)

func TestReviewD3AStreamWhoseOnlyChunkIsTakenOutIsNotAnEmpty200(t *testing.T) {
	events := []string{
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":0}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	up := newProvider(t, streaming(events, 0, false))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x") // stream_usage on: the gateway asks
	resp, got, _ := readStream(t, srv, askStream)
	if resp.StatusCode == 200 && len(got) == 1 && got[0].raw == "data: [DONE]\n\n" {
		t.Errorf("the client got 200 and a stream of only data: [DONE]: %v", got)
	}
}
