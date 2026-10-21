package load

// Bug: Percentile computes the nearest rank as math.Ceil(p / 100 * float64(n)) in floating point. 99.9/100*n is
// slightly above the integer it should be whenever n is a multiple of 1000 (99.9/100*1000 = 999.0000000000001), so
// the ceiling goes one rank too far: p99.9 of 1000 values is the maximum, not the 999th value. Every run in README
// "Results (day 4)" has such a count (5,000, 30,000, 9,000, 8,000 measured requests), so every p99.9 there is the
// next sample up from the nearest-rank one. (TestPercentilesAreNearestRank checks p99.9 only for 100 values, where
// the ceiling is 100 either way.)

import (
	"testing"
	"time"
)

func TestReviewD4P999OfAThousandIsTheNinetyNinthHundredAndNinetyNinth(t *testing.T) {
	for _, n := range []int{1000, 5000, 8000, 9000, 30000} {
		sorted := make([]time.Duration, n)
		for i := range sorted {
			sorted[i] = time.Duration(i + 1)
		}
		want := time.Duration(n * 999 / 1000) // ceil(0.999 n), exact for these n
		if got := Percentile(sorted, 99.9); got != want {
			t.Errorf("p99.9 of %d values 1..%d: %d, want %d (nearest rank)", n, n, got, want)
		}
	}
}
