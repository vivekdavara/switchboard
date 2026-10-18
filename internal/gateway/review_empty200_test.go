package gateway

// Found by the day-1 review (at 07b8b14): the gateway's "client went away" paths (chat's default branch after a
// body read error, and failUpstream's 499 branch) wrote nothing and returned, on the assumption that nobody was
// listening. But when a handler writes nothing, net/http writes "200 OK" with an empty body itself
// (response.finishRequest). Two ordinary ways the client is still there:
//
//   - a malformed chunked body: the body read fails with "invalid byte in chunk length", which is neither a
//     MaxBytesError nor a timeout, so it was logged as a 499 and the client, still connected, got an empty 200;
//   - a client that half-closes its side after sending the whole request (shutdown(SHUT_WR)): net/http's
//     background read sees EOF and cancels the request context, the upstream request fails at once, and the
//     client, still reading, got an empty 200. The provider was never asked.
//
// Now an unreadable body gets a 400 (invalid_body), and a request whose client is taken for gone has its connection
// dropped (http.ErrAbortHandler), logged as 499.
//
// Run: go test -race -count=1 -run TestReviewNoEmpty200 -v ./internal/gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rawExchange sends raw on a new connection to srv, optionally half-closes, and returns the response (nil if the
// connection was dropped).
func rawExchange(t *testing.T, srv *httptest.Server, raw string, halfClose bool) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	if halfClose {
		conn.(*net.TCPConn).CloseWrite()
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err.Error()
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestReviewNoEmpty200(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")

	t.Run("a malformed chunked body", func(t *testing.T) {
		raw := "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" +
			"Transfer-Encoding: chunked\r\n\r\nzz\r\n" + ask + "\r\n0\r\n\r\n"
		resp, body := rawExchange(t, srv, raw, false)
		if resp == nil {
			t.Fatalf("no response: %s", body)
		}
		if _, code, _, _ := errorOfOrEmpty([]byte(body)); resp.StatusCode != 400 || code != "invalid_body" {
			t.Errorf("got %d %q for a malformed body, want 400 invalid_body\nlog: %s", resp.StatusCode, body, logs)
		}
	})

	t.Run("a client that half-closes after its request", func(t *testing.T) {
		raw := "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" +
			"Connection: close\r\nContent-Length: " + strconv.Itoa(len(ask)) + "\r\n\r\n" + ask
		resp, body := rawExchange(t, srv, raw, true)
		if resp != nil {
			t.Errorf("got %d %q (Content-Length %q), want the connection dropped; the provider got %d requests\n"+
				"log: %s", resp.StatusCode, body, resp.Header.Get("Content-Length"), len(up.requests()), logs)
		}
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"status":499`); {
			if time.Now().After(deadline) {
				t.Fatalf("no 499 in the log:\n%s", logs)
			}
			time.Sleep(time.Millisecond)
		}
	})
}
