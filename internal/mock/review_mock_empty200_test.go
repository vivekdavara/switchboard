package mock

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Found by the day-1 review (3): the mock had the gateway's empty-200 bug too. chatCompletions treated every body
// read error that isn't a MaxBytesError as "the client went away mid-body" and returned without writing, so
// net/http answered a malformed chunked body, from a client still connected, with "200 OK" and an empty body. It
// now answers 400 invalid_body.
//
// Run: go test -race -count=1 -run TestReviewAMalformedBodyIsNotAnEmpty200 -v ./internal/mock
func TestReviewAMalformedBodyIsNotAnEmpty200(t *testing.T) {
	p, srv := start(t, fast())
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\n\r\nzz\r\n"+hello+"}\r\n0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, code, _ := apiError(t, b); resp.StatusCode != 400 || code != "invalid_body" {
		t.Errorf("a malformed chunked body got %d %q, want 400 invalid_body", resp.StatusCode, b)
	}
	if s := p.Stats(); s.Requests != 1 || s.Refused != 1 {
		t.Errorf("stats %+v, want it counted as refused", s)
	}
}

// Added after the second review, which turned the panic in the latency wait back into a return and saw every test
// pass: a client that half-closes after a complete request is reported as gone by net/http while it still reads, so
// a handler that returns without writing would hand it an empty 200. While waiting out its latency or hanging, the
// mock drops the connection instead.
func TestAHalfClosedClientNeverGetsAnEmpty200(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"waiting out its latency", func(c *Config) { c.TTFT = time.Minute }},
		{"hanging", func(c *Config) { c.HangRate = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fast()
			tc.change(&cfg)
			p, srv := start(t, cfg)
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			body := hello + "}"
			io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\n"+
				"Content-Type: application/json\r\nConnection: close\r\nContent-Length: "+
				strconv.Itoa(len(body))+"\r\n\r\n"+body)
			conn.(*net.TCPConn).CloseWrite()
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("got %d %q, want the connection dropped", resp.StatusCode, b)
			}
			if s := p.Stats(); s.Answered != 0 || s.Requests != 1 {
				t.Errorf("stats %+v", s)
			}
		})
	}
}
