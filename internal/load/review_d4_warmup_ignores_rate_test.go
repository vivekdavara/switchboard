package load

// Bug (the generator against chaos.sh's timing): the warmup ignores Rate. It is sent in batches of Concurrency at
// once, so chaos.sh's -rate 200 -warmup 200 -c 1024 is one burst of 200 concurrent requests that ends after about one
// answer (~54 ms), not "about a second at 200 a second" as chaos.sh assumes when it sleeps 11 s to act "at 10 s" of the
// timeline. Each fault therefore lands ~0.9 s late on the timeline README "Results (day 4)" prints (its hung-phase
// excerpt shows it: 29 of second 10's requests waited 2 s, i.e. the stop came at ~10.85 s, not 10 s), and an open
// loop's warmup is a burst the measured run then starts behind.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReviewD4AnOpenLoopsWarmupKeepsTheRate(t *testing.T) {
	var warmupEnded time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	began := time.Now()
	res, err := Run(context.Background(), Options{URL: srv.URL, Requests: 1, Warmup: 50, Concurrency: 64, Rate: 100,
		Body: func(i int) []byte {
			if i >= 0 && warmupEnded.IsZero() {
				warmupEnded = time.Now()
			}
			return body(i)
		}})
	if err != nil || len(res.Samples) != 1 {
		t.Fatal(err, res)
	}
	// 50 warmup requests at 100 a second: about half a second.
	if took := warmupEnded.Sub(began); took < 400*time.Millisecond {
		t.Errorf("the warmup of 50 at 100/s took %s: sent at once, not at the rate", took)
	}
}
