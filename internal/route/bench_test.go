package route

import (
	"math/rand/v2"
	"testing"
)

// BenchmarkOrder is the router's cost per request: ordering a model's targets.
func BenchmarkOrder(b *testing.B) {
	for _, tc := range []struct {
		name    string
		targets []any
	}{
		{"1", []any{"a", 1}},
		{"3+standby", []any{"a", 3, "b", 2, "c", 1, "s", 0}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ts := targets(tc.targets...)
			b.ReportAllocs()
			for range b.N {
				Order(ts, rand.IntN)
			}
		})
	}
}
