package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestEachOutcomeTellsTheBreakerWhatItShould makes each kind of answer the probe of a half-open breaker, which
// shows all three verdicts apart: a success closes it, a failure opens it again, and nothing learnt leaves it
// half-open with the probe handed back, so the next request probes. (The day-2 review could change the verdict of
// most rows, or drop the hand-back of a neutral probe, with every test passing.)
func TestEachOutcomeTellsTheBreakerWhatItShould(t *testing.T) {
	stream := func(events ...string) http.Handler { return streaming(events, 0, false) }
	for _, tc := range []struct {
		name  string
		probe http.Handler
		after string // the breaker's state after the probe
	}{
		{"a completion", answering(200, `{"id":"x","object":"chat.completion","choices":[]}`), "closed"},
		{"400", answering(400, `{"error":{"message":"no","type":"invalid_request_error"}}`), "closed"},
		{"413", answering(413, `{"error":{"message":"no","type":"invalid_request_error"}}`), "closed"},
		{"422", answering(422, `{"error":{"message":"no","type":"invalid_request_error"}}`), "closed"},
		{"429", answering(429, `{"error":{"message":"no","type":"rate_limit_error"}}`), "open"},
		{"401", answering(401, `{}`), "open"},
		{"403", answering(403, `{}`), "open"},
		{"404", answering(404, `{}`), "half-open"},
		{"500", answering(500, `{}`), "open"},
		{"502", answering(502, `{}`), "open"},
		{"a redirect", answering(302, ""), "open"},
		{"a 200 without JSON", answering(200, "<html>"), "open"},
		{"an answer over the limit", answering(200, `{"x":"`+strings.Repeat("x", 3000)+`"}`), "half-open"},
		{"a timeout", waiting(make(chan struct{}, 1)), "open"},
		{"a broken answer", breaksOff, "open"},
		{"a whole stream", stream(chunk, "data: [DONE]\n\n"), "closed"},
		{"a stream that breaks after its first event", stream(chunk), "open"},
		{"a stream over the limit before its first event", stream("data: " + strings.Repeat("x", 3000) + "\n\n"),
			"half-open"},
		{"a stream with an error first", stream(`data: {"error":{"message":"no"}}` + "\n\n"), "open"},
		{"a stream that ends before its first event", stream(), "open"},
		{"a stream answered with JSON", answering(200, `{"id":"x","object":"chat.completion","choices":[]}`), "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch n.Add(1) {
				case 1: // opens the breaker
					answering(500, `{}`).ServeHTTP(w, r)
				case 2: // the probe
					tc.probe.ServeHTTP(w, r)
				default:
					answering(200, `{"id":"y","object":"chat.completion","choices":[]}`).ServeHTTP(w, r)
				}
			}))
			srv, g, _ := gatewayFor(t, `"max_response_bytes":2000,"circuit_breaker":{"failures":1,"cooldown":"50ms"}`,
				[]string{at(up, `"timeout":"200ms"`)}, "m=p0/x")
			plain := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
			send(t, srv, plain)
			time.Sleep(70 * time.Millisecond)
			if got := g.breakers["p0"].State().String(); got != "half-open" {
				t.Fatalf("before the probe: %s", got)
			}
			body := plain
			if strings.HasPrefix(tc.name, "a stream") || tc.name == "a whole stream" {
				body = askStream
			}
			readStream(t, srv, body) // reads a plain answer too, as one unended "event"
			if got := g.breakers["p0"].State().String(); got != tc.after {
				t.Fatalf("after the probe: %s, want %s", got, tc.after)
			}
			if tc.after == "half-open" { // nothing learnt: the probe was handed back, and the next request probes
				if resp, b := send(t, srv, plain); resp.StatusCode != 200 || n.Load() != 3 {
					t.Errorf("the next request: %d %s, provider asked %d times", resp.StatusCode, b, n.Load())
				}
			}
		})
	}
}

func TestRetryAfterIsTheEarliestProbeAndAtLeastASecond(t *testing.T) {
	failing := newProvider(t, answering(500, `{}`))
	other := newProvider(t, answering(500, `{}`))
	srv, _, _ := gatewayFor(t, `"circuit_breaker":{"failures":1,"cooldown":"30s"}`,
		[]string{at(failing, ""), at(other, "")}, "a=p0/x", "b=p1/x", inOrder("both", "p0/x", "p1/x"))
	send(t, srv, `{"model":"a","messages":[{"role":"user","content":"x"}]}`) // p0 opens now
	time.Sleep(1100 * time.Millisecond)
	send(t, srv, `{"model":"b","messages":[{"role":"user","content":"x"}]}`) // p1 opens 1.1 s later
	resp, b := send(t, srv, `{"model":"both","messages":[{"role":"user","content":"x"}]}`)
	// p0's probe comes first, in 28.9 s: 29, not p1's 30.
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "29" || !strings.Contains(string(b), "retry after 29s") {
		t.Errorf("%d Retry-After %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}

	// A probe out: its breaker can only say "now", and the header says 1, as the message does.
	release := make(chan struct{})
	var n atomic.Int32
	held := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			answering(500, `{}`).ServeHTTP(w, r)
			return
		}
		<-release
		answering(500, `{}`).ServeHTTP(w, r)
	}))
	srv, _, _ = gatewayFor(t, `"circuit_breaker":{"failures":1,"cooldown":"20ms"}`, []string{at(held, "")}, "m=p0/x")
	plain := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
	send(t, srv, plain)
	time.Sleep(40 * time.Millisecond)
	done := make(chan struct{})
	go func() { defer close(done); send(t, srv, plain) }() // the probe, held
	for deadline := time.Now().Add(5 * time.Second); n.Load() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the probe never arrived")
		}
		time.Sleep(time.Millisecond)
	}
	resp, b = send(t, srv, plain)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" || !strings.Contains(string(b), "retry after 1s") {
		t.Errorf("%d Retry-After %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	close(release)
	<-done
}

func TestAStallBetweenOneAndTwoTimeoutsIsCaught(t *testing.T) {
	// Two chunks, 500 ms of nothing, then the rest: past the 300 ms timeout, so cut, though within twice it. (After
	// the second chunk, so the wait is one the timeout was restarted for by a data event.)
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunk+chunk)
		http.NewResponseController(w).Flush()
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, chunk+"data: [DONE]\n\n")
	}))
	srv, _, _ := gatewayFor(t, "", []string{at(up, `"timeout":"300ms"`)}, "m=p0/x")
	_, events, err := readStream(t, srv, askStream)
	if err == nil || len(events) != 3 {
		t.Fatalf("%d events, %v", len(events), err)
	}
	if code := lastEventError(t, events); code != "upstream_timeout" {
		t.Errorf("code %q", code)
	}
}

func TestAProbeThatFindsNobodyReopensTheBreaker(t *testing.T) {
	up := newProvider(t, answering(500, `{}`))
	srv, g, _ := gatewayFor(t, `"circuit_breaker":{"failures":1,"cooldown":"50ms"}`, []string{at(up, "")}, "m=p0/x")
	send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	up.Close() // the provider is gone: the probe's connection is refused
	time.Sleep(70 * time.Millisecond)
	if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 502 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got := g.breakers["p0"].State().String(); got != "open" {
		t.Errorf("after an unreachable probe: %s", got)
	}
}
