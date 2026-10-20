package ratelimit

import (
	"context"
	"testing"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/redistest"
)

// BenchmarkTake is one request taken from a bucket: one EVALSHA round trip to the local Redis, on loopback.
func BenchmarkTake(b *testing.B) {
	rdb, prefix := redistest.Client(b)
	l := New(rdb, prefix)
	limit := config.RateLimit{PerMinute: 1_000_000, Burst: 1_000_000}
	ctx := context.Background()
	for range 100 {
		l.Take(ctx, "c", limit)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := l.Take(ctx, "c", limit); err != nil {
			b.Fatal(err)
		}
	}
}
