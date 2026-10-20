// Review 2 (day 3): the usage of an attempt that failed before its first chunk is counted for the next attempt's
// answer.
//
// relay's pre-commit loop now reads a usage-only chunk the gateway asked for (stream.go, readUsage in the loop before
// the commit) into x.usage and x.hasUsage. Neither is reset per attempt (gateway.go, the loop over targets). So when
// that attempt then fails before any chunk the client would see (here: [DONE] next, "ended its stream before its first
// chunk", unanswered) and the request falls back to a provider whose stream reports no usage (stream_usage off), the
// answer is logged and added to the totals with the first provider's tokens, priced at the second's price, instead of
// as no_usage. Before the fix the usage was read only after the commit, so a failed attempt's never was.
package gateway

import (
	"testing"
)

func TestReview2D3AFailedAttemptsUsageIsNotCountedForTheNext(t *testing.T) {
	usageOnly := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
		`"usage":{"prompt_tokens":5000,"completion_tokens":7000,"total_tokens":12000}}` + "\n\n"
	content := `data: {"id":"d","object":"chat.completion.chunk","created":1,"model":"x",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	p0 := newProvider(t, streaming([]string{usageOnly, "data: [DONE]\n\n"}, 0, false)) // stream_usage on: asked
	p1 := newProvider(t, streaming([]string{content, "data: [DONE]\n\n"}, 0, false))
	redis, totals := redisTotals(t)
	srv, _, logs := gatewayFor(t, redis, []string{at(p0, ""), at(p1, `"stream_usage":false`)},
		inOrder("m", "p0/x", "p1/x"))
	resp, events, err := readStream(t, srv, askStream)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p1" || len(events) != 2 {
		t.Fatalf("%d %v %v %v", resp.StatusCode, resp.Header, events, err)
	}
	lines := logLines(t, logs, "chat completion")
	line := lines[len(lines)-1]
	if _, ok := line["prompt_tokens"]; ok {
		t.Errorf("p1 reported no usage, but the log line has p0's: prompt_tokens %v, completion_tokens %v",
			line["prompt_tokens"], line["completion_tokens"])
	}
	for _, row := range today(t, totals) {
		if row.NoUsage != 1 || row.Tokens.Prompt != 0 || row.Tokens.Completion != 0 {
			t.Errorf("the totals count p1's answer with p0's tokens, not as no_usage: %+v", row)
		}
	}
}
