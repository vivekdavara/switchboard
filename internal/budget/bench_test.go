package budget

import (
	"testing"

	"github.com/vivekdavara/switchboard/internal/config"
)

// BenchmarkRequestAndRetry is the budget's cost for a request that falls back once.
func BenchmarkRequestAndRetry(b *testing.B) {
	bud := New(config.RetryBudget{Ratio: 1, MinPerSecond: 1}, nil)
	b.ReportAllocs()
	for range b.N {
		bud.Request()
		bud.Retry()
	}
}
