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
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: workers, Warmup: workers,
		Concurrency: workers})
	if err != nil {
		t.Fatal(err)
	}
	if s := Summarize(res.Samples, res.Elapsed); s.Successful != workers {
		t.Fatalf("%+v", s)
	}
	// With the bug, the idle limit of 100 closes 100 of the warmup's connections and the measured requests dial them
	// again: 300 dials, in every run. Without it, a few hundred more than 200 can still happen, and did in CI (207
	// and 221 on 2026-10-22): a worker's next request can reach the transport before its last connection is back in
	// the idle pool, and the transport then dials as well as waiting. With one measured request per worker, 30 runs
	// here with GOMAXPROCS=2 under -race on a busy machine dialled 200 in 25 and 240 to 272 in 5, and the bug 300 in
	// 10 of 10. So this bound only catches the bug's scale; TestTheDefaultClientKeepsAConnectionPerWorker pins the
	// cause.
	if n := dialed.Load(); n >= workers+workers/2 {
		t.Errorf("%d connections opened for %d workers: the warmup's connections weren't kept", n, workers)
	}
}

func TestTheDefaultClientKeepsAConnectionPerWorker(t *testing.T) {
	for _, c := range []int{1, 100, 101, 1024} {
		tr := newClient(c).Transport.(*http.Transport)
		if tr.MaxIdleConnsPerHost != c || tr.MaxIdleConns != 0 || tr.MaxConnsPerHost != 0 {
			t.Errorf("%d workers: %d idle per host, %d idle in all, %d per host", c, tr.MaxIdleConnsPerHost,
				tr.MaxIdleConns, tr.MaxConnsPerHost)
		}
	}
}
