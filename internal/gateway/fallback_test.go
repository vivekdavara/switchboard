package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// inOrder is a model whose targets ("provider/upstream") are tried in the order given: the first has weight 1 and
// the rest are standbys, which come after it in the config's order.
func inOrder(name string, targets ...string) string {
	var ts []string
	for i, t := range targets {
		prov, up, _ := strings.Cut(t, "/")
		weight := 0
		if i == 0 {
			weight = 1
		}
		ts = append(ts, fmt.Sprintf(`{"provider":%q,"upstream_model":%q,"weight":%d}`, prov, up, weight))
	}
	return fmt.Sprintf(`{"name":%q,"targets":[%s]}`, name, strings.Join(ts, ","))
}

// exchangeLog is the part of a "chat completion" log line these tests read.
type exchangeLog struct {
	Status   int
	Provider string
	Code     string
	Attempts int
	Tried    []attemptLog
	Spent    bool `json:"retry_budget_spent"`
}

// lastExchange decodes the last "chat completion" line of the log.
func lastExchange(t *testing.T, logs *syncBuffer) exchangeLog {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], `"msg":"chat completion"`) {
			var x exchangeLog
			if err := json.Unmarshal([]byte(lines[i]), &x); err != nil {
				t.Fatalf("%v: %s", err, lines[i])
			}
			return x
		}
	}
	t.Fatalf("no chat completion in the log:\n%s", logs)
	return exchangeLog{}
}

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// breaksOff answers 200 with a Content-Length it never fills, and closes the connection.
var breaksOff = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"id\":"))
		conn.Close()
	}
})

func TestEachKindOfFailureFallsBackOrNot(t *testing.T) {
	jsonErr := `{"error":{"message":"no","type":"invalid_request_error"}}`
	for _, tc := range []struct {
		name     string
		first    http.Handler // nil: nothing listens
		fallback bool
		outcome  string // of the first attempt, when it falls back
		status   int    // what the client gets, when it doesn't
	}{
		{"500", answering(500, `{"error":{"message":"oops","type":"server_error"}}`), true, "failed", 0},
		{"502 without JSON", answering(502, "bad gateway"), true, "failed", 0},
		{"503", answering(503, `{}`), true, "failed", 0},
		{"529, overloaded", answering(529, `{}`), true, "failed", 0},
		{"429", answering(429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, "Retry-After", "7"),
			true, "failed", 0},
		{"401", answering(401, jsonErr), true, "failed", 0},
		{"403", answering(403, jsonErr), true, "failed", 0},
		{"404", answering(404, jsonErr), true, "failed", 0},
		{"a redirect", answering(302, "", "Location", "http://elsewhere.example/"), true, "failed", 0},
		{"418", answering(418, jsonErr), true, "failed", 0},
		{"nothing listening", nil, true, "failed", 0},
		{"200 without JSON", answering(200, "<html>"), true, "unanswered", 0},
		{"a broken answer", breaksOff, true, "unanswered", 0},
		{"a timeout", waiting(make(chan struct{}, 1)), true, "unanswered", 0},
		{"400", answering(400, jsonErr), false, "", 400},
		{"413", answering(413, jsonErr), false, "", 413},
		{"422", answering(422, jsonErr), false, "", 422},
		{"400 without JSON", answering(400, "nope"), false, "", 400},
		{"an answer over the limit", answering(200, `{"x":"`+strings.Repeat("x", 3000)+`"}`), false, "", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := fmt.Sprintf(`"base_url":"http://%s/v1","timeout":"300ms"`, closedAddr(t))
			if tc.first != nil {
				first = at(newProvider(t, tc.first), `"timeout":"300ms"`)
			}
			second := newProvider(t, mockProvider(t, "mock-b"))
			srv, _, logs := gatewayFor(t, `"max_response_bytes":2000`, []string{first, at(second, "")},
				inOrder("m", "p0/mock-small", "p1/mock-small"))
			resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
			x := lastExchange(t, logs)
			if !tc.fallback {
				if resp.StatusCode != tc.status || len(second.requests()) != 0 ||
					resp.Header.Get("X-Switchboard-Attempts") != "1" || x.Attempts != 1 {
					t.Errorf("%d %v %s; the fallback got %d requests", resp.StatusCode, resp.Header, b,
						len(second.requests()))
				}
				return
			}
			if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p1" ||
				resp.Header.Get("X-Switchboard-Attempts") != "2" || resp.Header.Get("Retry-After") != "" {
				t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, b)
			}
			if len(second.requests()) != 1 || x.Attempts != 2 || len(x.Tried) != 2 ||
				x.Tried[0].Provider != "p0" || x.Tried[0].Outcome != tc.outcome ||
				x.Tried[1].Provider != "p1" || x.Tried[1].Outcome != "answered" || x.Provider != "p1" {
				t.Errorf("log %+v; the fallback got %d requests", x, len(second.requests()))
			}
		})
	}
}

func TestAnUnansweredProviderIsNotAskedAgainButAFailedOneIs(t *testing.T) {
	// p0 serves two upstream models. A timeout on the first means p0 may still be working on the request: its
	// other target is skipped, and p1 gets the request. A 500 says p0 didn't do it: its other target is tried.
	slow := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(string(mustRead(r)), `"model":"a"`) {
			<-r.Context().Done()
			return
		}
		mockProvider(t, "p0").ServeHTTP(w, r)
	}))
	second := newProvider(t, mockProvider(t, "mock-b"))
	srv, _, logs := gatewayFor(t, "", []string{at(slow, `"timeout":"200ms"`), at(second, "")},
		inOrder("m", "p0/a", "p0/mock-small", "p1/mock-small"))
	resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	x := lastExchange(t, logs)
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p1" || len(slow.requests()) != 1 {
		t.Fatalf("%d %v %s; p0 got %d", resp.StatusCode, resp.Header, b, len(slow.requests()))
	}
	if len(x.Tried) != 3 || x.Tried[0].Outcome != "unanswered" || x.Tried[0].Code != "upstream_timeout" ||
		x.Tried[1].Skipped != "unanswered_before" || x.Tried[1].UpstreamModel != "mock-small" ||
		x.Tried[2].Outcome != "answered" || x.Attempts != 2 {
		t.Errorf("%+v", x.Tried)
	}

	failing := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(string(mustRead(r)), `"model":"a"`) {
			answering(500, `{}`).ServeHTTP(w, r)
			return
		}
		mockProvider(t, "p0").ServeHTTP(w, r)
	}))
	srv, _, logs = gatewayFor(t, "", []string{at(failing, ""), at(second, "")},
		inOrder("m", "p0/a", "p0/mock-small", "p1/mock-small"))
	resp, b = send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p0" || len(failing.requests()) != 2 {
		t.Errorf("%d %v %s; p0 got %d", resp.StatusCode, resp.Header, b, len(failing.requests()))
	}
	if x := lastExchange(t, logs); len(x.Tried) != 2 || x.Tried[0].Outcome != "failed" ||
		x.Tried[1].UpstreamModel != "mock-small" {
		t.Errorf("%+v", x.Tried)
	}
}

// mustRead reads a request's body and puts it back, for a handler that hands the request on.
func mustRead(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

func TestWhenEveryTargetFailsTheClientGetsTheLastAnswer(t *testing.T) {
	limited := func(after string) http.Handler {
		return answering(429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, "Retry-After", after)
	}
	for _, tc := range []struct {
		name          string
		first, second http.Handler
		status        int
		code          string
		retryAfter    string
	}{
		// The first 429's Retry-After isn't left on a different answer.
		{"429 then 500", limited("7"), answering(500, `{}`), 502, "upstream_error", ""},
		{"500 then 429", answering(500, `{}`), limited("3"), 429, "", "3"},
		{"429 then 429", limited("7"), limited("3"), 429, "", "3"},
		{"500 then a timeout", answering(500, `{}`), waiting(make(chan struct{}, 1)), 504, "upstream_timeout", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newProvider(t, tc.first), newProvider(t, tc.second)
			srv, _, logs := gatewayFor(t, "", []string{at(a, ""), at(b, `"timeout":"200ms"`)},
				inOrder("m", "p0/x", "p1/x"))
			resp, body := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
			if resp.StatusCode != tc.status || resp.Header.Get("Retry-After") != tc.retryAfter ||
				resp.Header.Get("X-Switchboard-Provider") != "p1" || resp.Header.Get("X-Switchboard-Attempts") != "2" {
				t.Errorf("%d %v %s", resp.StatusCode, resp.Header, body)
			}
			if _, code, _, _ := errorOf(t, body); tc.code != "" && code != tc.code {
				t.Errorf("code %q, want %q", code, tc.code)
			}
			if x := lastExchange(t, logs); x.Status != tc.status || x.Attempts != 2 {
				t.Errorf("%+v", x)
			}
		})
	}
}

func TestMaxAttemptsAndTheRetryBudgetLimitFallback(t *testing.T) {
	down := fmt.Sprintf(`"base_url":"http://%s/v1"`, closedAddr(t))
	second := newProvider(t, mockProvider(t, "mock-b"))
	model := `{"name":"m","max_attempts":1,"targets":[{"provider":"p0","upstream_model":"mock-small"},` +
		`{"provider":"p1","upstream_model":"mock-small","weight":0}]}`
	srv, _, _ := gatewayFor(t, "", []string{down, at(second, "")}, model)
	if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 502 ||
		len(second.requests()) != 0 {
		t.Errorf("max_attempts 1 fell back: %d %s", resp.StatusCode, b)
	}

	srv, _, logs := gatewayFor(t, `"retry_budget":{"ratio":0,"min_per_second":0}`, []string{down, at(second, "")},
		inOrder("m", "p0/mock-small", "p1/mock-small"))
	resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if x := lastExchange(t, logs); resp.StatusCode != 502 || len(second.requests()) != 0 || !x.Spent ||
		x.Attempts != 1 {
		t.Errorf("an empty budget fell back: %d %s %+v", resp.StatusCode, b, x)
	}

	// A ratio of 0.5 with no floor: every second request may fall back.
	srv, _, _ = gatewayFor(t, `"retry_budget":{"ratio":0.5,"min_per_second":0}`, []string{down, at(second, "")},
		inOrder("m", "p0/mock-small", "p1/mock-small"))
	var statuses []int
	for range 6 {
		resp, _ := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
		statuses = append(statuses, resp.StatusCode)
	}
	if fmt.Sprint(statuses) != "[502 200 502 200 502 200]" {
		t.Errorf("statuses %v", statuses)
	}
}

func TestAnOpenBreakerSkipsItsProvider(t *testing.T) {
	failing := newProvider(t, answering(500, `{}`))
	second := newProvider(t, mockProvider(t, "mock-b"))
	srv, g, logs := gatewayFor(t, `"circuit_breaker":{"failures":2,"cooldown":"30s"}`,
		[]string{at(failing, ""), at(second, "")}, inOrder("m", "p0/mock-small", "p1/mock-small"), "only=p0/x")
	for range 2 {
		if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	if got := g.breakers["p0"].State().String(); got != "open" || len(failing.requests()) != 2 {
		t.Fatalf("p0's breaker is %s after %d failures", got, len(failing.requests()))
	}
	// Now p0 is skipped: the request goes straight to p1, and that is its first attempt.
	resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	x := lastExchange(t, logs)
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Attempts") != "1" || len(failing.requests()) != 2 ||
		len(x.Tried) != 2 || x.Tried[0].Skipped != "circuit_open" || x.Tried[1].Outcome != "answered" {
		t.Errorf("%d %v %s %+v", resp.StatusCode, resp.Header, b, x)
	}
	// A model with no other provider: 503 at once, with when to come back.
	resp, b = send(t, srv, `{"model":"only","messages":[{"role":"user","content":"x"}]}`)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "circuit_open" ||
		(resp.Header.Get("Retry-After") != "30" && resp.Header.Get("Retry-After") != "29") ||
		len(failing.requests()) != 2 {
		t.Errorf("%d %v %s", resp.StatusCode, resp.Header, b)
	}
	if x := lastExchange(t, logs); x.Status != 503 || x.Attempts != 0 || len(x.Tried) != 1 {
		t.Errorf("%+v", x)
	}
	// The other provider's breaker is its own.
	if got := g.breakers["p1"].State().String(); got != "closed" {
		t.Errorf("p1's breaker is %s", got)
	}
}

func TestA404DoesNotTripTheProvidersBreaker(t *testing.T) {
	unknown := newProvider(t, answering(404, `{"error":{"message":"no such model","type":"invalid_request_error"}}`))
	srv, g, _ := gatewayFor(t, `"circuit_breaker":{"failures":1,"cooldown":"30s"}`, []string{at(unknown, "")},
		"m=p0/x")
	for range 3 {
		if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 502 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	if got := g.breakers["p0"].State().String(); got != "closed" || len(unknown.requests()) != 3 {
		t.Errorf("breaker %s after %d 404s", got, len(unknown.requests()))
	}
}

func TestTheFirstAttemptFollowsTheWeights(t *testing.T) {
	a := newProvider(t, mockProvider(t, "mock-a"))
	b := newProvider(t, mockProvider(t, "mock-b"))
	srv, g, _ := gatewayFor(t, "", []string{at(a, ""), at(b, "")},
		`{"name":"m","targets":[{"provider":"p0","upstream_model":"mock-small","weight":1},`+
			`{"provider":"p1","upstream_model":"mock-small","weight":3}]}`)
	for point, want := range []string{"p0", "p1", "p1", "p1"} {
		first := true
		g.intn = func(n int) int {
			if !first {
				return 0 // the fallback order: the one left
			}
			first = false
			if n != 4 {
				t.Errorf("the first draw is over %d, want the total weight 4", n)
			}
			return point
		}
		resp, body := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
		if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != want {
			t.Errorf("point %d: %d %v %s", point, resp.StatusCode, resp.Header, body)
		}
	}
}

func TestAClientThatLeavesEndsTheFallback(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	hold := newProvider(t, waiting(cancelled))
	second := newProvider(t, mockProvider(t, "mock-b"))
	srv, g, logs := gatewayFor(t, "", []string{at(hold, ""), at(second, "")}, inOrder("m", "p0/x", "p1/mock-small"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("the request finished")
	}
	<-cancelled
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"status":499`); {
		if time.Now().After(deadline) {
			t.Fatalf("no 499 in the log:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
	if len(second.requests()) != 0 {
		t.Errorf("the fallback was asked for a client that had left")
	}
	// A client leaving says nothing about p0: its breaker counts no failure.
	if g.breakers["p0"].State().String() != "closed" {
		t.Error("p0's breaker moved")
	}
}

func TestAShutdownEndsTheFallback(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	hold := newProvider(t, waiting(cancelled))
	second := newProvider(t, mockProvider(t, "mock-b"))
	_, g, logs := gatewayFor(t, "", []string{at(hold, ""), at(second, "")}, inOrder("fast", "p0/x", "p1/mock-small"))
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	go func() {
		for deadline := time.Now().Add(10 * time.Second); len(hold.requests()) == 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		stop(ErrShutdown)
	}()
	resp, b := send(t, srv, ask)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "shutting_down" ||
		len(second.requests()) != 0 {
		t.Errorf("%d %s; the fallback got %d", resp.StatusCode, b, len(second.requests()))
	}
	// Not even tried: an attempt on a cancelled context never leaves the gateway, but it would still spend the
	// retry budget, and the breaker's probe if it were the one.
	if x := lastExchange(t, logs); x.Attempts != 1 || len(x.Tried) != 0 {
		t.Errorf("%+v", x)
	}
	<-cancelled
}

func TestAProbeTheBudgetRefusesIsHandedBack(t *testing.T) {
	// p1's breaker is half-open, so the fallback to it would be its probe; the empty budget refuses the retry. The
	// probe must be handed back, or p1 would be "probing" for ever and skipped by every later request.
	failing := newProvider(t, answering(500, `{}`))
	flaky := newProvider(t, answering(500, `{}`))
	srv, g, _ := gatewayFor(t, `"retry_budget":{"ratio":0,"min_per_second":0},`+
		`"circuit_breaker":{"failures":1,"cooldown":"20ms"}`, []string{at(failing, ""), at(flaky, "")},
		inOrder("m", "p0/x", "p1/x"), "only=p1/x")
	send(t, srv, `{"model":"only","messages":[{"role":"user","content":"x"}]}`) // p1 fails once: open
	time.Sleep(30 * time.Millisecond)
	if got := g.breakers["p1"].State().String(); got != "half-open" {
		t.Fatalf("p1 is %s", got)
	}
	if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 502 ||
		len(flaky.requests()) != 1 {
		t.Fatalf("%d %s; p1 got %d", resp.StatusCode, b, len(flaky.requests()))
	}
	// The probe is still free: the next request to p1 is let through.
	resp, b := send(t, srv, `{"model":"only","messages":[{"role":"user","content":"x"}]}`)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 502 || code != "upstream_error" || len(flaky.requests()) != 2 {
		t.Errorf("%d %s; p1 got %d requests", resp.StatusCode, b, len(flaky.requests()))
	}
}
