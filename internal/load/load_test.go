package load

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func body(int) []byte { return []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`) }

func TestAClosedLoopSendsTheWarmupThenTheMeasuredRequests(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("X-Switchboard-Provider", "p")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 200, Warmup: 20,
		Concurrency: 8, Header: http.Header{"Authorization": {"Bearer k"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := Summarize(res.Samples, res.Elapsed)
	if seen.Load() != 220 || s.Requests != 200 || s.Successful != 200 || s.Statuses[200] != 200 || len(s.Errors) != 0 {
		t.Fatalf("server saw %d; %+v", seen.Load(), s)
	}
	if s.PerSecond <= 0 || s.P50 <= 0 || s.P50 > s.P99 || s.P99 > s.Max || s.FirstByte <= 0 {
		t.Errorf("%+v", s)
	}
	if tl := Timeline(res.Samples, time.Hour, "X-Switchboard-Provider"); len(tl) != 1 || tl[0].OK != 200 ||
		tl[0].Header["p"] != 200 {
		t.Errorf("%+v", tl)
	}
}

// stallingAt answers at once, except the n-th request, which it holds for d.
func stallingAt(n int64, d time.Duration) *httptest.Server {
	var count atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == n {
			time.Sleep(d)
		}
		w.Write([]byte(`{}`))
	}))
}

// slow counts the samples slower than d.
func slow(samples []Sample, d time.Duration) int {
	n := 0
	for _, s := range samples {
		if s.Latency > d {
			n++
		}
	}
	return n
}

func TestAnOpenLoopCountsTheWaitAStallCausesAndAClosedLoopDoesnt(t *testing.T) {
	// One worker, a request due every 10 ms, and the 31st held for 300 ms: the requests due during the stall wait
	// behind it, and the open loop measures them from when they were due.
	srv := stallingAt(31, 300*time.Millisecond)
	defer srv.Close()
	open, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 100, Concurrency: 1, Rate: 100})
	if err != nil {
		t.Fatal(err)
	}
	if n := slow(open.Samples, 50*time.Millisecond); n < 20 {
		t.Errorf("open loop: %d requests slower than 50 ms, want the ~25 due during the stall", n)
	}
	srv2 := stallingAt(31, 300*time.Millisecond)
	defer srv2.Close()
	closed, err := Run(context.Background(), Options{URL: srv2.URL, Body: body, Requests: 100, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := slow(closed.Samples, 50*time.Millisecond); n != 1 {
		t.Errorf("closed loop: %d requests slower than 50 ms, want only the stalled one", n)
	}
}

func TestAnOpenLoopKeepsItsRate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 50, Concurrency: 4, Rate: 500})
	if err != nil {
		t.Fatal(err)
	}
	// 50 requests, 2 ms apart: the last is due at 98 ms.
	if last := res.Samples[49].Due; last < 98*time.Millisecond || last > 150*time.Millisecond {
		t.Errorf("the last request was due at %s, want 98 ms", last)
	}
	if res.Elapsed < 98*time.Millisecond {
		t.Errorf("elapsed %s", res.Elapsed)
	}
}

func TestTransportErrorsAreCountedByKind(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	res, err := Run(context.Background(), Options{URL: "http://" + addr, Body: body, Requests: 5, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if s := Summarize(res.Samples, res.Elapsed); s.Errors["connection refused"] != 5 || s.Successful != 0 ||
		s.P50 != 0 || s.PerSecond != 0 {
		t.Errorf("%+v", s)
	}
	if tl := Timeline(res.Samples, time.Second, ""); len(tl) != 1 || tl[0].Failed != 5 {
		t.Errorf("%+v", tl)
	}
}

func TestATimeoutEndsARequest(t *testing.T) {
	srv := stallingAt(1, time.Second)
	defer srv.Close()
	res, _ := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 1, Concurrency: 1,
		Timeout: 50 * time.Millisecond})
	if s := res.Samples[0]; s.Err != "context deadline exceeded" || s.Latency > 500*time.Millisecond {
		t.Errorf("%+v", s)
	}
}

func TestPercentilesAreNearestRank(t *testing.T) {
	var hundred []time.Duration
	for i := 1; i <= 100; i++ {
		hundred = append(hundred, time.Duration(i))
	}
	for _, tc := range []struct {
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{hundred, 50, 50}, {hundred, 90, 90}, {hundred, 99, 99}, {hundred, 99.9, 100}, {hundred, 100, 100},
		{hundred, 0.5, 1}, {hundred[:3], 50, 2}, {hundred[:2], 50, 1}, {hundred[:1], 99.9, 1}, {nil, 50, 0},
	} {
		if got := Percentile(tc.sorted, tc.p); got != tc.want {
			t.Errorf("p%g of %d values: %d, want %d", tc.p, len(tc.sorted), got, tc.want)
		}
	}
}

func TestATimelineStepsByDueTime(t *testing.T) {
	h := func(p string) http.Header { return http.Header{"X-Switchboard-Provider": {p}} }
	samples := []Sample{
		{Due: 100 * time.Millisecond, Latency: 5, Status: 200, Header: h("a")},
		{Due: 900 * time.Millisecond, Latency: 7, Status: 502, Header: h("b")},
		{Due: 2500 * time.Millisecond, Latency: 9, Status: 200, Header: h("a")},
		{Due: 2600 * time.Millisecond, Err: "connection refused"},
	}
	tl := Timeline(samples, time.Second, "X-Switchboard-Provider")
	if len(tl) != 3 || tl[0].Requests != 2 || tl[0].OK != 1 || tl[0].Failed != 1 || tl[0].P50 != 5 || tl[0].P99 != 7 ||
		tl[1].Requests != 0 || tl[2].From != 2*time.Second || tl[2].OK != 1 || tl[2].Failed != 1 ||
		tl[0].Header["b"] != 1 || tl[2].Header["a"] != 1 {
		t.Errorf("%+v", tl)
	}
}

func TestBadOptionsAreRefused(t *testing.T) {
	for _, o := range []Options{{}, {URL: "http://h", Body: body, Requests: 1}, {URL: "http://h", Body: body,
		Concurrency: 1}, {URL: "http://h", Body: body, Requests: 1, Concurrency: 1, Rate: -1}} {
		if _, err := Run(context.Background(), o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestAnOpenLoopsTimelineStartsAfterItsWarmup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	// 20 warmup requests and 30 measured at 200 a second: the warmup takes 100 ms, and the measured requests are due
	// from 0 to 145 ms of the run's own clock.
	began := time.Now()
	res, err := Run(context.Background(), Options{URL: srv.URL, Body: body, Requests: 30, Warmup: 20, Concurrency: 4,
		Rate: 200})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples[0].Due != 0 || res.Samples[29].Due != 145*time.Millisecond {
		t.Errorf("due at %s and %s, want 0 and 145 ms", res.Samples[0].Due, res.Samples[29].Due)
	}
	if took := time.Since(began); took < 245*time.Millisecond {
		t.Errorf("the run took %s; 50 requests at 200 a second take at least 245 ms", took)
	}
}

func TestAStoppedRunKeepsOnlyTheRequestsItSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, Options{URL: srv.URL, Body: body, Requests: 1000, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Samples); n == 0 || n >= 100 {
		t.Fatalf("%d samples from 100 ms of 2 workers at 10 ms a request", n)
	}
	for i, s := range res.Samples {
		if s.Latency <= 0 && s.Err == "" {
			t.Errorf("sample %d was never sent: %+v", i, s)
		}
		if s.Err != "" && s.Err != "context deadline exceeded" {
			t.Errorf("sample %d: %q, not shortened to its kind", i, s.Err)
		}
	}
	if res.Elapsed <= 0 || res.Elapsed > time.Second {
		t.Errorf("elapsed %s", res.Elapsed)
	}
	if got := errorKind(context.Canceled); got != "context canceled" {
		t.Errorf("%q", got)
	}
}
