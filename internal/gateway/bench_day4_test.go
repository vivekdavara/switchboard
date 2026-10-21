package gateway

// Benchmarks for README "Results (day 4)": what the metrics and the log line cost a request, measured on the two
// calls the request's end makes (observe and logExchange) with a typical exchange, apart from any HTTP. Run with
// `make bench`.

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/breaker"
	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// day4Exchanges are an answered request (a client, one attempt, tokens and a cost) and a fallback (a skip, a failed
// attempt, then an answer).
func day4Exchanges() map[string]*exchange {
	answered := &exchange{id: "0123456789abcdef", began: time.Now(), status: 200, model: "fast", route: "fast",
		provider: "mock-a", upstreamModel: "mock-small", upstreamSent: true, upstreamStatus: 200,
		upstreamTook: 52 * time.Millisecond, requestBytes: 81, responseBytes: 446, client: "alice", limited: true,
		usage: usage.Usage{Prompt: 5, Completion: 32}, hasUsage: true, cost: 19_950_000, priced: true, counted: true}
	answered.attempts = []attemptLog{{Provider: "mock-a", UpstreamModel: "mock-small", Status: 200, Ms: 52,
		Outcome: "answered"}}
	answered.tried = answered.attempts
	fallback := *answered
	fallback.model, fallback.route = "smart", "smart"
	fallback.tried = []attemptLog{{Provider: "mock-c", UpstreamModel: "big", Skipped: "circuit_open"},
		{Provider: "mock-b", UpstreamModel: "big", Status: 500, Ms: 3, Outcome: "failed", Code: "upstream_error"},
		answered.attempts[0]}
	fallback.attempts = fallback.tried[1:]
	return map[string]*exchange{"answered": answered, "fallback": &fallback}
}

func BenchmarkObserve(b *testing.B) {
	for name, x := range day4Exchanges() {
		b.Run(name, func(b *testing.B) {
			m := newMetrics(map[string]*breaker.Breaker{"mock-a": breaker.New(config.DefaultBreaker, nil)})
			b.ReportAllocs()
			for b.Loop() {
				m.observe(x)
			}
		})
	}
}

func BenchmarkLogLine(b *testing.B) {
	for name, x := range day4Exchanges() {
		b.Run(name, func(b *testing.B) {
			g := &Gateway{log: slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics: newMetrics(nil)}
			b.ReportAllocs()
			for b.Loop() {
				g.logExchange(x) // the line, then observe
			}
		})
	}
}
