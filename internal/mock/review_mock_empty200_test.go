package mock

import (
	"bufio"
	"io"
	"net"
	"net/http"
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
	_, srv := start(t, fast())
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"+
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
}
