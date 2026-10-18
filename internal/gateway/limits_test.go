package gateway

// Tests for mechanisms the day-1 review showed no test noticed when it removed them: the request limit at exactly
// its boundary, the 408 for a body slower than the server's read timeout, and the provider connections kept idle.

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

func TestTheRequestLimitIsExact(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, `"max_request_bytes":300`, []string{at(up, "")}, "fast=p0/mock-small")
	padded := func(size int) string { // a valid request of exactly size bytes
		head := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"pad":"`
		return head + strings.Repeat("x", size-len(head)-2) + `"}`
	}
	if b := padded(300); len(b) != 300 {
		t.Fatal(len(b))
	}
	if resp, b := send(t, srv, padded(300)); resp.StatusCode != 200 {
		t.Errorf("exactly the limit: %d %s", resp.StatusCode, b)
	}
	resp, b := send(t, srv, padded(301))
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 413 || code != "request_too_large" {
		t.Errorf("one byte over: %d %s", resp.StatusCode, b)
	}
}

func TestABodySlowerThanTheReadTimeoutIs408(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"providers":[{"name":"p0","base_url":%q}],`+
		`"models":[{"name":"fast","provider":"p0","upstream_model":"mock-small"}]}`, up.URL+"/v1")), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(New(cfg, nil, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	// The headers promise the whole request; only its first ten bytes ever come.
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\n\r\n%s", len(ask), ask[:10])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 408 || code != "request_timeout" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("the provider got %d requests", n)
	}
}

// burst sends n requests at once through srv and waits for all of them.
func burst(t *testing.T, srv *httptest.Server, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(ask))
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

func TestTheGatewayKeepsItsProviderConnections(t *testing.T) {
	for _, tc := range []struct {
		idle       int // the transport's idle connections per provider; 0 for NewTransport's own
		reuseAbove int // the second burst must open at most this many connections...
		openBelow  int // ...or at least this many
	}{
		{idle: 0, reuseAbove: 2},
		{idle: 2, openBelow: 16},
	} {
		t.Run("idle="+strconv.Itoa(tc.idle), func(t *testing.T) {
			var opened atomic.Int32
			m := mockProvider(t, "mock-a")
			up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(20 * time.Millisecond) // keep the 32 requests in flight together
				m.ServeHTTP(w, r)
			}))
			up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					opened.Add(1)
				}
			}
			up.Start()
			t.Cleanup(up.Close)
			cfg, err := config.Parse([]byte(fmt.Sprintf(`{"providers":[{"name":"p0","base_url":%q}],`+
				`"models":[{"name":"fast","provider":"p0","upstream_model":"mock-small"}]}`, up.URL+"/v1")), nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := NewTransport()
			if tc.idle > 0 {
				tr.MaxIdleConnsPerHost = tc.idle
			}
			srv := httptest.NewServer(New(cfg, tr, slog.New(slog.NewJSONHandler(io.Discard, nil))))
			t.Cleanup(srv.Close)
			srv.Client().Transport.(*http.Transport).MaxIdleConnsPerHost = 64 // the client's side isn't measured

			burst(t, srv, 32)
			first := opened.Load()
			burst(t, srv, 32)
			second := opened.Load() - first
			t.Logf("connections opened: %d for the first 32 requests, %d for the next 32", first, second)
			if tc.reuseAbove > 0 && second > int32(tc.reuseAbove) {
				t.Errorf("the second burst opened %d connections; the pool should have kept the first's", second)
			}
			if tc.openBelow > 0 && second < int32(tc.openBelow) {
				t.Errorf("the second burst opened only %d connections with 2 kept idle", second)
			}
		})
	}
}
