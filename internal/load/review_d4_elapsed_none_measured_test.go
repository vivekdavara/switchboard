package load

// Bug: Result.Elapsed is last.Sub(began), where last is the latest end of a measured request. If ctx ends before any
// measured request is sent (a Ctrl-C during the warmup), last is the zero time and Elapsed is about -292 years
// (time.Duration's minimum), which loadgen then prints as the run's duration. With no measured request it should be 0.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReviewD4ARunStoppedBeforeItsFirstMeasuredRequestHasNoNegativeElapsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, rate := range []float64{0, 100} {
		res, err := Run(ctx, Options{URL: srv.URL, Body: body, Requests: 10, Warmup: 2, Concurrency: 2, Rate: rate})
		if err != nil {
			t.Fatal(err)
		}
		if res.Elapsed < 0 {
			t.Errorf("rate %g: %d samples, elapsed %s", rate, len(res.Samples), res.Elapsed)
		}
	}
}
