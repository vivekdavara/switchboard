package gateway

// Bug: a stream that breaks (or carries the provider's error event) after its usage chunk is logged with that
// usage (prompt_tokens, completion_tokens: x.hasUsage is set as the chunk passes), but it is no completion, so
// account never runs, x.counted stays false, and observe adds no tokens. The log line and the metrics then disagree
// on the tokens, which is exactly the invariant agreeWithTheLog says holds "whatever the requests" (DESIGN.md: "the
// counters are the log line's facts, so the two can't disagree ... the tokens and the cost"). A provider's last
// chunk is the usage one, so a break just before [DONE] is the ordinary case.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const reviewD4UsageChunk = `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n"

const reviewD4ContentChunk = `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x",` +
	`"choices":[{"index":0,"delta":{"content":"4"}}]}` + "\n\n"

func TestReviewD4AStreamBrokenAfterItsUsageAgreesWithTheLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		cut    bool
	}{
		{"broken after the usage chunk", []string{reviewD4ContentChunk, reviewD4UsageChunk}, true},
		{"an error event after the usage chunk", []string{reviewD4ContentChunk, reviewD4UsageChunk,
			`data: {"error":{"message":"engine died"}}` + "\n\n", "data: [DONE]\n\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProvider(t, streaming(tc.events, 0, tc.cut))
			srv, g, logs := gatewayFor(t, mini, []string{at(up, "")}, "fast=p0/mock-small")
			body := strings.Replace(ask, `}]}`, `}],"stream":true,"stream_options":{"include_usage":true}}`, 1)
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body) // a broken stream ends with an error: the gateway drops the connection
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"prompt_tokens":11`) {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			lines := logLines(t, logs, "chat completion")
			if len(lines) != 1 || lines[0]["code"] == nil {
				t.Fatalf("%v", lines)
			}
			agreeWithTheLog(t, scrape(t, g), lines)
		})
	}
}
