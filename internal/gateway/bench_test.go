package gateway

// Benchmarks for README "Results" (day 1): what the gateway adds to a request, and what keeping idle connections
// to a provider saves. The provider is the mock with no simulated latency, so the times are the HTTP hops and the
// gateway's own work. Run them with `make bench`.

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/mock"
)

// benchSetup is a mock provider and a gateway in front of it, each on its own loopback port.
type benchSetup struct {
	direct, gateway string        // base URLs
	newConns        *atomic.Int64 // connections the provider accepted
	client          *http.Client
}

func newBenchSetup(b *testing.B, idlePerHost int) *benchSetup {
	b.Helper()
	mcfg := mock.Defaults()
	mcfg.Name, mcfg.TTFT, mcfg.PerToken, mcfg.Models = "mock-a", 0, 0, nil
	m, err := mock.New(mcfg)
	if err != nil {
		b.Fatal(err)
	}
	newConns := &atomic.Int64{}
	up := httptest.NewUnstartedServer(m)
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	up.Start()
	b.Cleanup(up.Close)

	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"providers":[{"name":"mock-a","base_url":%q}],`+
		`"models":[{"name":"fast","provider":"mock-a","upstream_model":"mock-small"}]}`, up.URL+"/v1")), nil)
	if err != nil {
		b.Fatal(err)
	}
	t := NewTransport()
	t.MaxIdleConnsPerHost = idlePerHost
	gw := httptest.NewServer(New(cfg, t, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	b.Cleanup(gw.Close)

	// The benchmark's own client keeps plenty of idle connections, so only the gateway's pool is being varied.
	ct := http.DefaultTransport.(*http.Transport).Clone()
	ct.MaxIdleConns, ct.MaxIdleConnsPerHost = 1024, 1024
	b.Cleanup(ct.CloseIdleConnections)
	return &benchSetup{direct: up.URL, gateway: gw.URL, newConns: newConns, client: &http.Client{Transport: ct}}
}

// bodies are the requests measured: one short message, and a 20-message conversation of about 4 KB.
var bodies = map[string]string{
	"small": `{"model":"fast","messages":[{"role":"user","content":"What is two plus two?"}]}`,
	"chat":  conversation(20),
}

func conversation(n int) string {
	var msgs []string
	for i := range n {
		role := []string{"user", "assistant"}[i%2]
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":%q}`, role,
			strings.Repeat(fmt.Sprintf("turn %d says something about the weather. ", i), 4)))
	}
	return `{"model":"fast","temperature":0.7,"messages":[` + strings.Join(msgs, ",") + `]}`
}

func (s *benchSetup) post(base, body string) error {
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// BenchmarkOverhead sends requests one at a time, straight to the provider ("direct") and through the gateway, and
// reports the mean (ns/op) and the median and 99th percentile of each request's time. The gateway's overhead is the
// difference: its parsing and rewriting plus a second loopback HTTP hop.
func BenchmarkOverhead(b *testing.B) {
	for _, shape := range []string{"small", "chat"} {
		for _, via := range []string{"direct", "gateway"} {
			b.Run(via+"/"+shape, func(b *testing.B) {
				s := newBenchSetup(b, 256)
				base := s.direct
				if via == "gateway" {
					base = s.gateway
				}
				body := bodies[shape]
				if via == "direct" {
					body = strings.Replace(body, `"model":"fast"`, `"model":"mock-small"`, 1)
				}
				for range 100 { // warm the connections
					if err := s.post(base, body); err != nil {
						b.Fatal(err)
					}
				}
				took := make([]time.Duration, 0, b.N)
				b.ResetTimer()
				for range b.N {
					began := time.Now()
					if err := s.post(base, body); err != nil {
						b.Fatal(err)
					}
					took = append(took, time.Since(began))
				}
				b.StopTimer()
				slices.Sort(took)
				b.ReportMetric(float64(took[len(took)/2].Microseconds()), "p50-µs")
				b.ReportMetric(float64(took[len(took)*99/100].Microseconds()), "p99-µs")
			})
		}
	}
}

// BenchmarkPool sends requests from 32 goroutines through the gateway, whose transport keeps 2 (Go's default) or
// 256 idle connections per provider, and reports the provider connections opened per request.
func BenchmarkPool(b *testing.B) {
	for _, idle := range []int{2, 256} {
		b.Run(fmt.Sprintf("idle=%d", idle), func(b *testing.B) {
			s := newBenchSetup(b, idle)
			start := s.newConns.Load()
			b.SetParallelism(max(1, 32/runtime.GOMAXPROCS(0)))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := s.post(s.gateway, bodies["small"]); err != nil {
						b.Error(err) // not Fatal: this isn't the benchmark's goroutine
						return
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(s.newConns.Load()-start)/float64(b.N), "conns/op")
		})
	}
}
