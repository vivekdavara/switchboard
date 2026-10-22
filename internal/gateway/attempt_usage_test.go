package gateway

import (
	"fmt"
	"strings"
	"testing"
)

// usageChunk is a stream's usage-only chunk.
func usageChunk(prompt, completion int) string {
	return fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n", prompt, completion,
		prompt+completion)
}

// TestEveryFailedAttemptsUsageIsUncountedUnderItsOwnLabels drives the paths the day-4 third review probed, where an
// attempt reports its usage and then ends before any chunk the client would see (unanswered: maybe billed): a skip
// between it and the next attempt, two such attempts, one reporting only completion tokens, a budget refusal after
// it, and a client's key. Each attempt's tokens must be uncounted under its own provider, upstream model and client
// (the review's surviving mutants: tokens put on a skip's entry, the first attempt's overwritten by the second's, a
// completion-only usage dropped, the line's upstream model or no client used as the label).
func TestEveryFailedAttemptsUsageIsUncountedUnderItsOwnLabels(t *testing.T) {
	type key struct{ client, provider, model, kind string }
	for _, tc := range []struct {
		name    string
		extra   string
		who     string
		targets []string
		want    map[key]float64
	}{
		{"a skip between", "", "", []string{"p0/x", "p0/y", "p1/v"}, map[key]float64{
			{"anonymous", "p0", "x", "prompt"}: 5000, {"anonymous", "p0", "x", "completion"}: 7000}},
		{"two failures, then an answer", "", "", []string{"p0/x", "p2/w", "p1/v"}, map[key]float64{
			{"anonymous", "p0", "x", "prompt"}: 5000, {"anonymous", "p0", "x", "completion"}: 7000,
			{"anonymous", "p2", "w", "prompt"}: 300, {"anonymous", "p2", "w", "completion"}: 400}},
		{"only completion tokens", "", "", []string{"p3/u", "p1/v"}, map[key]float64{
			{"anonymous", "p3", "u", "prompt"}: 0, {"anonymous", "p3", "u", "completion"}: 900}},
		{"two failures, nothing more", "", "", []string{"p0/x", "p2/w"}, map[key]float64{
			{"anonymous", "p0", "x", "prompt"}: 5000, {"anonymous", "p0", "x", "completion"}: 7000,
			{"anonymous", "p2", "w", "prompt"}: 300, {"anonymous", "p2", "w", "completion"}: 400}},
		{"the budget refuses", `"retry_budget":{"ratio":0,"min_per_second":0}`, "", []string{"p0/x", "p1/v"},
			map[key]float64{{"anonymous", "p0", "x", "prompt"}: 5000, {"anonymous", "p0", "x", "completion"}: 7000}},
		{"a client's", clients("alice"), "alice", []string{"p0/x", "p1/v"}, map[key]float64{
			{"alice", "p0", "x", "prompt"}: 5000, {"alice", "p0", "x", "completion"}: 7000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p0 := newProvider(t, streaming([]string{usageChunk(5000, 7000), "data: [DONE]\n\n"}, 0, false))
			p1 := newProvider(t, streaming([]string{
				`data: {"id":"d","object":"chat.completion.chunk","created":1,"model":"x",` +
					`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n",
				"data: [DONE]\n\n"}, 0, false))
			p2 := newProvider(t, streaming([]string{usageChunk(300, 400), "data: [DONE]\n\n"}, 0, false))
			p3 := newProvider(t, streaming([]string{usageChunk(0, 900), "data: [DONE]\n\n"}, 0, false))
			srv, g, logs := gatewayFor(t, tc.extra, []string{at(p0, ""), at(p1, `"stream_usage":false`), at(p2, ""),
				at(p3, "")}, inOrder("m", tc.targets...))
			var header []string
			if tc.who != "" {
				header = as(tc.who)
			}
			send(t, srv, askStream, header...)
			samples := scrape(t, g)
			agreeWithTheLog(t, samples, logLines(t, logs, "chat completion"))
			total := 0.0
			for k, want := range tc.want {
				name := fmt.Sprintf(`switchboard_uncounted_tokens_total{client=%q,kind=%q,provider=%q,upstream_model=%q}`,
					k.client, k.kind, k.provider, k.model)
				if got := samples[name]; got != want {
					t.Errorf("%s = %g, want %g", name, got, want)
				}
				total += want
			}
			if got := sum(samples, "switchboard_uncounted_tokens_total"); got != total {
				t.Errorf("%g uncounted tokens in all, want %g (some under other labels?)", got, total)
			}
			if strings.Contains(logs.String(), `"skipped":"unanswered_before","upstream_model":"y","prompt_tokens"`) {
				t.Errorf("a skip carries tokens: %s", logs)
			}
		})
	}
}
