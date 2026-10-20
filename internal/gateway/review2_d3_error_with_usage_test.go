// Review 2 (day 3): an error event that carries a usage object is taken for the usage the gateway asked for, and
// dropped, when the gateway asked for the usage the client didn't.
//
// unasked (accounting.go, unaskedParsed) drops any chunk whose usage is an object and that has no choices: it never
// looks for "error". Two of the fixes now rely on it:
//   - before the commit (stream.go, the new `x.stripUsage && ... && unasked(ev) == nil` skip), a first event
//     {"error":{...},"usage":{...}} is skipped as "the usage the gateway asked for", so the attempt is no longer the
//     provider's refusal ("answered its stream with an error: <message>", failed, as the old code had it) but "ended
//     its stream before its first chunk" (unanswered: the provider is then marked unsure), and the provider's message
//     is lost;
//   - after the commit, the mid-stream check marks the stream failed, but out() still drops the event, so the client
//     never gets the error the comment in relay says it "gets as it came": its SDK sees a clean [DONE].
package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

const errorWithUsage = `data: {"error":{"message":"the engine failed","type":"server_error"},` +
	`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}` + "\n\n"

func TestReview2D3AFirstErrorEventWithAUsageIsTheProvidersRefusal(t *testing.T) {
	p0 := newProvider(t, streaming([]string{errorWithUsage, "data: [DONE]\n\n"}, 0, false))
	srv, _, _ := gatewayFor(t, "", []string{at(p0, "")}, "m=p0/x") // stream_usage on: the gateway asks
	resp, b := send(t, srv, askStream)
	if resp.StatusCode != 502 || !strings.Contains(string(b), "the engine failed") {
		t.Errorf("the provider's error is lost: %d %s", resp.StatusCode, b)
	}
}

// As unanswered rather than failed, the provider is marked unsure, so the model's other target on the same provider
// (another upstream model) is skipped as "unanswered_before" instead of tried.
func TestReview2D3AFirstErrorEventWithAUsageLetsTheSameProviderBeTriedAgain(t *testing.T) {
	content := `data: {"id":"d","object":"chat.completion.chunk","created":1,"model":"y",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	var mu sync.Mutex
	n := 0
	p0 := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			streaming([]string{errorWithUsage, "data: [DONE]\n\n"}, 0, false).ServeHTTP(w, r)
			return
		}
		streaming([]string{content, "data: [DONE]\n\n"}, 0, false).ServeHTTP(w, r)
	}))
	srv, _, logs := gatewayFor(t, "", []string{at(p0, "")}, inOrder("m", "p0/x", "p0/y"))
	resp, _ := send(t, srv, askStream)
	x := lastExchange(t, logs)
	if resp.StatusCode != 200 || len(x.Tried) != 2 || x.Tried[0].Outcome != "failed" {
		t.Errorf("the provider's error as its first event is not a refusal (failed), so p0/y wasn't tried: %d %+v",
			resp.StatusCode, x.Tried)
	}
}

func TestReview2D3AMidStreamErrorEventWithAUsageReachesTheClient(t *testing.T) {
	content := `data: {"id":"d","object":"chat.completion.chunk","created":1,"model":"x",` +
		`"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"
	p0 := newProvider(t, streaming([]string{content, errorWithUsage, "data: [DONE]\n\n"}, 0, false))
	srv, _, logs := gatewayFor(t, "", []string{at(p0, "")}, "m=p0/x") // stream_usage on: the gateway asks
	_, events, _ := readStream(t, srv, askStream)
	sawError := false
	for _, ev := range events {
		sawError = sawError || strings.Contains(ev.raw, "the engine failed")
	}
	if !sawError {
		t.Errorf("the provider's error event never reached the client, which got a clean stream: %v", events)
	}
	if x := lastExchange(t, logs); x.Code != "upstream_error" {
		t.Errorf("%+v", x)
	}
}
