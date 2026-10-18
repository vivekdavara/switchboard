package mock

// Found by the day-1 review (at 07b8b14): two promises in mock.go that the code did not keep.
//
//  1. Close's comment: "Requests that come later are still served; Close is for shutting down." After Close, p.stop
//     is closed for good, so every later request that waits out any latency at all takes the `case <-p.stop` branch
//     at once: it is counted as abandoned and its connection is dropped. The comment now says so, and this test
//     pins what it says.
//  2. draw's comment: "raising the rate turns more answers into failures without reshuffling the rest". The hang
//     band was [ErrorRate, ErrorRate+HangRate), so it moved when ErrorRate rose: with a hang rate set, a request
//     that was answered at the lower error rate could hang at the higher one (15 of 200 with seed 7). Hangs now
//     take the top of [0, 1).
//
// Run: go test -race -count=1 -run 'TestReview' -v ./internal/mock

import (
	"strings"
	"testing"
	"time"
)

func TestReviewRequestsAfterCloseAreDropped(t *testing.T) {
	cfg := fast()
	cfg.TTFT = 5 * time.Millisecond // any latency at all
	p, srv := start(t, cfg)
	p.Close()
	resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(hello+`}`))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a request after Close got %s, want its connection dropped", resp.Status)
	}
	if s := p.Stats(); s.Abandoned != 1 || s.Answered != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestReviewARaisedErrorRateOnlyTurnsAnswersIntoFailures(t *testing.T) {
	low, high := fatesAt(t, 0.1, 0.1, 200), fatesAt(t, 0.4, 0.1, 200)
	bad := 0
	for i := range low {
		// Allowed: the same fate, or a failure at the higher rate.
		if low[i] != high[i] && high[i] != "fail" {
			if bad < 5 {
				t.Errorf("request %d: %s at error rate 0.1, %s at 0.4", i, low[i], high[i])
			}
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d requests changed fate other than into a failure", bad, len(low))
	}
}
