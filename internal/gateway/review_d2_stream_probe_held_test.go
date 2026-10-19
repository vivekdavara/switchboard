// Found by the day-2 review (at b3b5e1f): a half-open breaker's probe ticket is reported only when try returns, and a
// streamed probe returns only when its stream ends. A probe whose stream has committed (the provider answered) but
// whose client stops reading holds the ticket for as long as the client keeps its connection open (the server has no
// WriteTimeout): every other request skips the healthy provider as circuit_open, past the provider's own timeout.
package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewD2AStreamedProbeDoesNotHoldTheBreakerForEver(t *testing.T) {
	big := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],"pad":"` +
		strings.Repeat("x", 4000) + "\"}\n\n"
	var n atomic.Int32
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1: // opens the breaker
			answering(500, `{}`).ServeHTTP(w, r)
		case 2: // the probe: a stream, all of it at once
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			rc := http.NewResponseController(w)
			for _, e := range append(append([]string{chunk}, repeat(big, 2000)...), "data: [DONE]\n\n") {
				if _, err := io.WriteString(w, e); err != nil || rc.Flush() != nil {
					return
				}
			}
		default:
			answering(200, `{"id":"x","object":"chat.completion","choices":[]}`).ServeHTTP(w, r)
		}
	}))
	_, g, _ := gatewayFor(t, `"max_response_bytes":67108864,"circuit_breaker":{"failures":1,"cooldown":"50ms"}`,
		[]string{at(up, `"timeout":"300ms"`)}, "m=p0/x")
	srv := httptest.NewUnstartedServer(g)
	srv.Listener = smallWriteBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()

	if resp, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 502 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	time.Sleep(100 * time.Millisecond) // the cooldown is over: the next request is the probe
	if got := g.breakers["p0"].State().String(); got != "half-open" {
		t.Fatalf("p0 is %s", got)
	}

	// The probe: a stream whose client reads the headers (the provider has answered) and then nothing more, keeping
	// its connection open.
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(16 << 10)
		}
		return c, err
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(askStream))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p0" {
		t.Fatalf("the probe: %d %v", resp.StatusCode, resp.Header)
	}

	// Well past the provider's timeout, p0 has answered and is healthy; another request should reach it.
	time.Sleep(time.Second)
	resp2, b := send(t, srv, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if resp2.StatusCode != 200 {
		t.Errorf("a request to the healthy p0 got %d %s (breaker %s)", resp2.StatusCode, b, g.breakers["p0"].State())
	}
}
