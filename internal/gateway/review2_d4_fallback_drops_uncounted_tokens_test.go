package gateway

// Bug (the day-4 review's fix 2, incomplete): switchboard_uncounted_tokens_total and the log's `uncounted: true` hold
// "tokens a provider reported for an answer that wasn't counted ... the provider may still bill them", but only for
// the last attempt. try() resets x.usage and x.hasUsage for each attempt, so an attempt that reported its usage and
// then failed before any chunk the client would see (here: the usage chunk the gateway asked for, then [DONE], so
// "ended its stream before its first chunk", unanswered: the provider may have done and billed the work) leaves its
// tokens nowhere if a fallback follows, while the same attempt alone is logged and counted as uncounted. Whether p0's
// 5,000 prompt tokens show depends on whether another target was configured after it.

import (
	"testing"
)

func TestReview2D4AFailedAttemptsReportedTokensAreUncountedEvenIfAFallbackFollows(t *testing.T) {
	usageOnly := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
		`"usage":{"prompt_tokens":5000,"completion_tokens":7000,"total_tokens":12000}}` + "\n\n"
	content := `data: {"id":"d","object":"chat.completion.chunk","created":1,"model":"x",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	for _, tc := range []struct {
		name    string
		targets []string
	}{
		{"p0 alone", []string{"p0/x"}},
		{"p0, then p1", []string{"p0/x", "p1/x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p0 := newProvider(t, streaming([]string{usageOnly, "data: [DONE]\n\n"}, 0, false)) // stream_usage on: asked
			p1 := newProvider(t, streaming([]string{content, "data: [DONE]\n\n"}, 0, false))
			srv, g, logs := gatewayFor(t, "", []string{at(p0, ""), at(p1, `"stream_usage":false`)},
				inOrder("m", tc.targets...))
			resp, _, _ := readStream(t, srv, askStream)
			lines := logLines(t, logs, "chat completion")
			agreeWithTheLog(t, scrape(t, g), lines) // passes either way: the log and the metrics both drop them
			got := 0.0
			for k, v := range scrape(t, g) {
				if k == `switchboard_uncounted_tokens_total{client="anonymous",kind="prompt",provider="p0",upstream_model="x"}` {
					got = v
				}
			}
			if got != 5000 {
				t.Errorf("status %d: p0 reported 5000 prompt tokens and its answer wasn't counted; uncounted: %g; line %v",
					resp.StatusCode, got, lines[len(lines)-1])
			}
		})
	}
}
