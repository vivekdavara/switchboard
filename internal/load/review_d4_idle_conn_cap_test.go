package load

// Bug: Run's default client clones http.DefaultTransport and raises MaxIdleConnsPerHost to Concurrency, but leaves
// the clone's MaxIdleConns at DefaultTransport's 100. So with more than 100 workers the pool keeps at most 100 idle
// connections in all: after the warmup (each batch ends with every connection idle at once) all but 100 are closed,
// and the measured requests dial again. DESIGN.md says "each worker keeps its connection" and Options.Warmup says the
// warmup opens the connections; load.sh's slow cases (-c 512) and chaos.sh (-c 1024) run above the cap, and a closed
// loop of 256 (the "waves" trial run in DESIGN.md) re-dials whenever more than 100 answers land together.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewD4EveryWorkerKeepsTheConnectionTheWarmupOpened(t *testing.T) {
	var dialed atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // so a batch is in flight together
		w.Write([]byte(`{}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	const workers = 200
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 2 * workers, Warmup: workers,
		Concurrency: workers})
	if err != nil {
		t.Fatal(err)
	}
	if s := Summarize(res.Samples, res.Elapsed); s.Successful != 2*workers {
		t.Fatalf("%+v", s)
	}
	if n := dialed.Load(); n > workers {
		t.Errorf("%d connections opened for %d workers: the warmup's connections weren't kept", n, workers)
	}
}
