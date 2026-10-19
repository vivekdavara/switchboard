package breaker

import (
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

// BenchmarkAllowAndSuccess is a closed breaker's cost per attempt.
func BenchmarkAllowAndSuccess(b *testing.B) {
	br := New(config.Breaker{Failures: 5, Cooldown: 10 * time.Second}, nil)
	b.ReportAllocs()
	for range b.N {
		t, _, _ := br.Allow()
		t.Success()
	}
}
