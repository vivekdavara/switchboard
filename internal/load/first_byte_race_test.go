package load

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The first byte's time is written by the transport's own goroutine (httptrace's GotFirstResponseByte runs in its
// read loop), and a request whose deadline fires just as the answer starts can return from Do before that write. The
// release check's make check caught it once, under the race detector, in TestAStoppedRunKeepsOnlyTheRequestsItSent:
// send then read the time unsynchronized. This test aims every request at that moment (answers due as the timeout
// fires) so that `go test -race` sees it; it failed on the code before the fix.
func TestAFirstByteAtTheDeadlineIsNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 4000, Concurrency: 16,
		Timeout: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range res.Samples {
		if s.FirstByte < 0 || (s.FirstByte > 0 && s.Err == "" && s.FirstByte > s.Latency) {
			t.Errorf("sample %d: first byte %s, latency %s", i, s.FirstByte, s.Latency)
		}
	}
}
