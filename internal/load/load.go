// Package load sends chat completions to a URL and measures what came back (day 4's load and chaos runs; DESIGN.md,
// "Load and chaos (day 4)").
//
// Two ways to send. Closed loop (Rate 0): Concurrency workers each send their next request as soon as their last one
// is answered, so the throughput is what the server sustains, and each latency runs from its request's send. Open
// loop (Rate > 0): requests are due at a fixed rate whatever the server does, and each latency runs from when the
// request was due, not when a free worker sent it. A closed loop measuring a server that stalls for a second sends
// nothing during that second, so it records one slow request where real clients would have seen a second's worth
// (coordinated omission); the open loop records all of them.
package load

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options are a run's settings.
type Options struct {
	URL         string
	Body        func(i int) []byte // request i's body (a warmup request's i is negative)
	Header      http.Header        // sent with every request (Content-Type is set already)
	Requests    int                // how many to measure
	Warmup      int                // sent before them and not measured: connections opened, code paths warm
	Concurrency int                // at most this many in flight
	Rate        float64            // requests a second; 0 for a closed loop
	Interval    time.Duration      // the timeline's step; 0 for no timeline
	Timeout     time.Duration      // a request's limit; 0 for none
	Client      *http.Client       // nil for one with a connection per worker kept open
}

// Sample is one measured request.
type Sample struct {
	Due       time.Duration // since the run began: when it was due (open loop) or sent (closed loop)
	Latency   time.Duration // from Due to the end of the answer
	FirstByte time.Duration // from Due to the answer's first byte; 0 if none came
	Status    int           // 0 if no answer came (Err says why)
	Err       string
	Header    http.Header
}

// Result is a run's measurements, in the order the requests were due.
type Result struct {
	Samples []Sample
	Elapsed time.Duration // from the first measured request's due time to the last one's end
}

// Run sends the warmup and then the measured requests, and returns the measured ones' samples. It stops early if ctx
// ends, with the samples so far.
func Run(ctx context.Context, o Options) (Result, error) {
	if o.URL == "" || o.Body == nil || o.Requests < 1 || o.Concurrency < 1 || o.Warmup < 0 || o.Rate < 0 {
		return Result{}, fmt.Errorf("load: a URL, a body, at least one request and one worker are needed")
	}
	client := o.Client
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// A connection per worker, kept: Go's default of 2 idle per host would open a new connection per request,
		// and the default transport's 100 idle in all dropped the rest above 100 workers (found in the day-4 review).
		t.MaxIdleConnsPerHost = o.Concurrency
		t.MaxIdleConns = 0 // no limit in all
		t.MaxConnsPerHost = 0
		client = &http.Client{Transport: t}
	}
	if o.Rate == 0 { // a closed loop's warmup: Concurrency at a time, as the loop itself sends
		for i := 0; i < o.Warmup; {
			var wg sync.WaitGroup
			for w := 0; w < o.Concurrency && i < o.Warmup; w, i = w+1, i+1 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					send(ctx, client, o, o.Body(-1-i), time.Now(), time.Now())
				}(i)
			}
			wg.Wait()
		}
	}

	samples := make([]Sample, o.Requests)
	type job struct {
		i   int // a warmup request's is negative
		due time.Time
	}
	var began time.Time
	jobs := make(chan job, o.Warmup+o.Requests)
	if o.Rate == 0 {
		began = time.Now()
		for i := range o.Requests {
			jobs <- job{i: i} // due when a worker takes it
		}
		close(jobs)
	} else {
		// An open loop's warmup keeps the rate too, so the measured requests begin in the steady state, and the
		// timeline's 0 s is the warmup's end (chaos.sh times its faults from it). Sent at once, as the closed loop's
		// is, it was a burst the run then started behind (found in the day-4 review).
		step := time.Duration(float64(time.Second) / o.Rate)
		start := time.Now()
		began = start.Add(time.Duration(o.Warmup) * step)
		go func() {
			defer close(jobs)
			for k := range o.Warmup + o.Requests {
				due := start.Add(time.Duration(k) * step)
				if d := time.Until(due); d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						return
					}
				}
				jobs <- job{k - o.Warmup, due}
			}
		}()
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var last time.Time
	for range o.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				if j.due.IsZero() {
					j.due = time.Now()
				}
				if j.i < 0 {
					send(ctx, client, o, o.Body(j.i), j.due, began) // warmup: not measured
					continue
				}
				s, end := send(ctx, client, o, o.Body(j.i), j.due, began)
				samples[j.i] = s
				mu.Lock()
				if end.After(last) {
					last = end
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	res := Result{Samples: samples, Elapsed: last.Sub(began)}
	if ctx.Err() != nil {
		n := 0
		for _, s := range samples {
			if s.Latency > 0 || s.Err != "" {
				samples[n] = s
				n++
			}
		}
		res.Samples = samples[:n]
	}
	return res, nil
}

// send sends one request, due at due, and reads the whole answer.
func send(ctx context.Context, client *http.Client, o Options, body []byte, due, began time.Time) (Sample, time.Time) {
	s := Sample{Due: due.Sub(began)}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	var first time.Time
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Now() }})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL, bytes.NewReader(body))
	if err != nil {
		s.Err = err.Error()
		return s, time.Now()
	}
	for k, v := range o.Header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		s.Status, s.Header = resp.StatusCode, resp.Header
	}
	end := time.Now()
	s.Latency = end.Sub(due)
	if !first.IsZero() {
		s.FirstByte = first.Sub(due)
	}
	if err != nil {
		s.Err = errorKind(err)
	}
	return s, end
}

// errorKind shortens a transport error to what it was, without the addresses, so equal failures count together.
func errorKind(err error) string {
	msg := err.Error()
	for _, kind := range []string{"connection refused", "connection reset", "context deadline exceeded", "EOF",
		"broken pipe", "timeout"} {
		if strings.Contains(msg, kind) {
			return kind
		}
	}
	return msg
}

// Summary is what a set of samples comes to.
type Summary struct {
	Requests   int
	PerSecond  float64 // answered requests (any status) a second of the elapsed time
	Statuses   map[int]int
	Errors     map[string]int
	P50, P90   time.Duration // latency percentiles of the answered requests (nearest rank)
	P99, P999  time.Duration
	Max        time.Duration
	FirstByte  time.Duration // its median
	Successful int           // status 200
}

// Summarize sums up samples over elapsed.
func Summarize(samples []Sample, elapsed time.Duration) Summary {
	s := Summary{Requests: len(samples), Statuses: map[int]int{}, Errors: map[string]int{}}
	var lat, fb []time.Duration
	for _, x := range samples {
		if x.Err != "" {
			s.Errors[x.Err]++
			continue
		}
		s.Statuses[x.Status]++
		if x.Status == http.StatusOK {
			s.Successful++
		}
		lat = append(lat, x.Latency)
		if x.FirstByte > 0 {
			fb = append(fb, x.FirstByte)
		}
	}
	if elapsed > 0 {
		s.PerSecond = float64(len(lat)) / elapsed.Seconds()
	}
	slices.Sort(lat)
	slices.Sort(fb)
	s.P50, s.P90, s.P99, s.P999 = Percentile(lat, 50), Percentile(lat, 90), Percentile(lat, 99), Percentile(lat, 99.9)
	if len(lat) > 0 {
		s.Max = lat[len(lat)-1]
	}
	s.FirstByte = Percentile(fb, 50)
	return s
}

// Percentile is the nearest-rank percentile p (0 < p <= 100, to four decimal places) of sorted: the smallest value
// that at least p percent of them are at most. 0 for none. The rank is computed in integers: in floating point,
// 99.9/100*1000 is 999.0000000000001, whose ceiling took the 1000th of 1000 for p99.9 (found in the day-4 review).
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	const scale = 1_000_000 // p in millionths of the whole
	millionths := int64(math.Round(p * scale / 100))
	n := int64(len(sorted))
	rank := (millionths*n + scale - 1) / scale // ceil(p/100 * n)
	return sorted[min(max(rank, 1), n)-1]
}

// Step is one interval of a timeline.
type Step struct {
	From     time.Duration // the interval's start, since the run began, by due time
	Requests int
	OK       int // status 200
	Failed   int // any other status, or no answer
	P50, P99 time.Duration
	Header   map[string]int // the count of each value of the timeline's header (say, X-Switchboard-Provider)
}

// Timeline puts samples into steps of interval by due time, counting the values of header in each.
func Timeline(samples []Sample, interval time.Duration, header string) []Step {
	if interval <= 0 || len(samples) == 0 {
		return nil
	}
	var steps []Step
	var lat [][]time.Duration
	for _, x := range samples {
		i := int(x.Due / interval)
		for len(steps) <= i {
			steps = append(steps, Step{From: time.Duration(len(steps)) * interval, Header: map[string]int{}})
			lat = append(lat, nil)
		}
		st := &steps[i]
		st.Requests++
		if x.Err == "" && x.Status == http.StatusOK {
			st.OK++
		} else {
			st.Failed++
		}
		if x.Err == "" {
			lat[i] = append(lat[i], x.Latency)
		}
		if header != "" && x.Header != nil {
			st.Header[x.Header.Get(header)]++
		}
	}
	for i := range steps {
		slices.Sort(lat[i])
		steps[i].P50, steps[i].P99 = Percentile(lat[i], 50), Percentile(lat[i], 99)
	}
	return steps
}
