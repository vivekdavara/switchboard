package gateway

// Found by the day-1 review (at 07b8b14): config accepted any positive max_response_bytes, including
// 9223372036854775807 (math.MaxInt64, a natural way to write "no limit"). forward reads the answer through
// io.LimitReader(resp.Body, MaxResponseBytes+1), and MaxInt64+1 overflowed to MinInt64, so the LimitReader read
// nothing: every answer was empty, and every request got 502 "answered 200 without a JSON object".
//
// Now config refuses sizes past config.MaxBytes (1 GiB; the config tests cover the refusal), and the largest size
// it accepts still passes answers.
//
// Run: go test -race -count=1 -run TestReviewTheLargestResponseLimitStillPassesAnswers -v ./internal/gateway

import (
	"fmt"
	"testing"

	"github.com/vivekdavara/switchboard/internal/config"
)

func TestReviewTheLargestResponseLimitStillPassesAnswers(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, fmt.Sprintf(`"max_response_bytes":%d`, config.MaxBytes), []string{at(up, "")},
		"fast=p0/mock-small")
	if resp, b := send(t, srv, ask); resp.StatusCode != 200 {
		t.Errorf("got %d %s, want the provider's 200", resp.StatusCode, b)
	}
}
