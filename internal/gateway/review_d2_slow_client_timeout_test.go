// Found by the day-2 review (at b3b5e1f): relay resets the provider's timer only after it has written an event to the
// client, so the time spent blocked writing to a slow client counts against the provider's timeout. A provider that
// sent its whole stream at once is cancelled, the client's stream ends in upstream_timeout, and the provider's breaker
// is told it failed.
package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// smallWriteBuffers gives every accepted connection a small send buffer, so a client that pauses holds the gateway's
// writes up after a few kilobytes instead of a few megabytes.
type smallWriteBuffers struct{ net.Listener }

func (l smallWriteBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(16 << 10)
	}
	return c, err
}

func TestReviewD2ASlowClientIsNotTheProvidersTimeout(t *testing.T) {
	big := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[],"pad":"` +
		strings.Repeat("x", 4000) + "\"}\n\n"
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		// The whole answer at once: the provider never makes the gateway wait.
		for _, e := range append(append([]string{chunk}, repeat(big, 2000)...), "data: [DONE]\n\n") {
			if _, err := io.WriteString(w, e); err != nil || rc.Flush() != nil {
				return
			}
		}
	}))
	_, g, logs := gatewayFor(t, `"max_response_bytes":67108864,"circuit_breaker":{"failures":1,"cooldown":"30s"}`,
		[]string{at(up, `"timeout":"300ms"`)}, "m=p0/x")
	srv := httptest.NewUnstartedServer(g)
	srv.Listener = smallWriteBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()

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
	time.Sleep(time.Second) // the client is busy for a moment: longer than the provider's timeout
	b, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasSuffix(string(b), "data: [DONE]\n\n") {
		tail := string(b[max(0, len(b)-300):])
		t.Errorf("the stream ended with %v after %d bytes: ...%s", err, len(b), tail)
	}
	if got := g.breakers["p0"].State().String(); got != "closed" {
		t.Errorf("the provider's breaker is %s: a slow client was counted as the provider's failure", got)
	}
	if x := lastExchange(t, logs); x.Code != "" {
		t.Errorf("logged as %q", x.Code)
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
