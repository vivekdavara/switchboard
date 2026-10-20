// Review (day 3): taking the usage out of a stream drops whole chunks that hold no usage at all.
//
// unasked (accounting.go) says it drops "a chunk that is only the usage (no choices, or an empty list of them, as
// OpenAI sends it last)". The code drops any chunk that has a "usage" member and no non-empty choices: it never looks
// at the usage's value or at the chunk's other members. With include_usage asked for, a provider puts "usage": null
// on every chunk, so a chunk with an empty choices list and something else in it (Azure OpenAI's first chunk, which
// carries prompt_filter_results and "choices": []) is dropped whole, though there was no usage in it to take out.
// The client, who didn't ask for usage, loses the chunk; it would have had it without the gateway's asking.
// (Here it is also the stream's first data event, so the stream is committed and its first event is never sent.)
package gateway

import (
	"strings"
	"testing"
)

func TestReviewD3AChunkWithNullUsageAndNoChoicesIsNotDropped(t *testing.T) {
	filter := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
		`"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false}}}],` +
		`"usage":null}` + "\n\n"
	events := []string{
		filter,
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}],"usage":null}` + "\n\n",
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	up := newProvider(t, streaming(events, 0, false))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x") // stream_usage on: the gateway asks
	_, got, err := readStream(t, srv, askStream)
	if err != nil {
		t.Fatal(err)
	}
	filtered := false
	for _, ev := range got {
		if strings.Contains(ev.raw, "usage") {
			t.Errorf("usage left in: %q", ev.raw)
		}
		filtered = filtered || strings.Contains(ev.raw, "prompt_filter_results")
	}
	if !filtered {
		t.Errorf("the chunk with prompt_filter_results (and \"usage\": null, no usage) was dropped; the client got "+
			"%d events: %v", len(got), got)
	}
}
