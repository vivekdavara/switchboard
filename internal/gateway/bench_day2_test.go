package gateway

// Benchmarks for README "Results (day 2)": what the gateway adds to a streamed answer, and what a circuit breaker
// saves while a provider is down. Run them with `make bench`.

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/mock"
)

// readEvents posts a streamed request and reads it to the end, returning when the first event arrived, when the
// stream ended, and how many events it had.
func readEvents(client *http.Client, base, body string) (first, total time.Duration, events int, err error) {
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	began := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadSlice('\n')
		if err == io.EOF {
			return first, time.Since(began), events, nil
		}
		if err != nil {
			return 0, 0, 0, err
		}
		if len(line) == 1 { // the blank line ending an event
			if events++; events == 1 {
				first = time.Since(began)
			}
		}
	}
}

// BenchmarkStream streams a 32-word answer (34 chunks and [DONE]) from the mock with no simulated latency, straight
// and through the gateway, one at a time. It reports the median time to the first event and to the end; the
// gateway's cost per event is the difference in the second, less the first's, over 35 events.
func BenchmarkStream(b *testing.B) {
	for _, via := range []string{"direct", "gateway"} {
		b.Run(via, func(b *testing.B) {
			s := newBenchSetup(b, 256)
			base, body := s.gateway, `{"model":"fast","messages":[{"role":"user","content":"Tell me"}],"stream":true}`
			if via == "direct" {
				base, body = s.direct, strings.Replace(body, `"fast"`, `"mock-small"`, 1)
			}
			for range 50 {
				if _, _, n, err := readEvents(s.client, base, body); err != nil || n != 35 {
					b.Fatalf("%d events, %v", n, err)
				}
			}
			firsts, totals := make([]time.Duration, 0, b.N), make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for range b.N {
				first, total, _, err := readEvents(s.client, base, body)
				if err != nil {
					b.Fatal(err)
				}
				firsts, totals = append(firsts, first), append(totals, total)
			}
			b.StopTimer()
			slices.Sort(firsts)
			slices.Sort(totals)
			b.ReportMetric(float64(firsts[len(firsts)/2].Microseconds()), "first-p50-µs")
			b.ReportMetric(float64(totals[len(totals)/2].Microseconds()), "total-p50-µs")
			b.ReportMetric(float64(totals[len(totals)*99/100].Microseconds()), "total-p99-µs")
		})
	}
}

// BenchmarkDeadProvider sends requests one at a time to a model whose first target is down, either refusing
// connections or accepting them and never answering (it hangs until the provider's 10 ms timeout), with a standby
// that answers. With breakers off every request waits for the dead provider first; with them on (5 failures, 10 s
// cooldown) the dead provider is skipped after its fifth failure, and probed once per cooldown.
func BenchmarkDeadProvider(b *testing.B) {
	for _, dead := range []string{"refusing", "hanging"} {
		for _, failures := range []int{0, 5} {
			name := fmt.Sprintf("%s/breaker=off", dead)
			if failures > 0 {
				name = fmt.Sprintf("%s/breaker=on", dead)
			}
			b.Run(name, func(b *testing.B) {
				deadURL := "http://" + unusedAddr(b) + "/v1"
				if dead == "hanging" {
					hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						// Read the body first: net/http notices a client that hangs up only once the body is
						// read, so without this the handler outlives the gateway's cancel and Close waits for ever.
						io.Copy(io.Discard, r.Body)
						<-r.Context().Done()
					}))
					b.Cleanup(hang.Close)
					deadURL = hang.URL + "/v1"
				}
				mcfg := mock.Defaults()
				mcfg.Name, mcfg.TTFT, mcfg.PerToken, mcfg.Models = "mock-b", 0, 0, nil
				m, err := mock.New(mcfg)
				if err != nil {
					b.Fatal(err)
				}
				standby := httptest.NewServer(m)
				b.Cleanup(standby.Close)
				cfg, err := config.Parse([]byte(fmt.Sprintf(`{
					"retry_budget": {"ratio": 1, "min_per_second": 1000},
					"circuit_breaker": {"failures": %d, "cooldown": "10s"},
					"providers": [{"name": "dead", "base_url": %q, "timeout": "10ms"},
					              {"name": "standby", "base_url": %q}],
					"models": [{"name": "m", "targets": [{"provider": "dead", "upstream_model": "x"},
					            {"provider": "standby", "upstream_model": "mock-small", "weight": 0}]}]}`,
					failures, deadURL, standby.URL+"/v1")), nil)
				if err != nil {
					b.Fatal(err)
				}
				gw := httptest.NewServer(New(cfg, nil, slog.New(slog.NewJSONHandler(io.Discard, nil))))
				b.Cleanup(gw.Close)
				s := &benchSetup{gateway: gw.URL, client: gw.Client()}
				body := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
				took := make([]time.Duration, 0, b.N)
				b.ResetTimer()
				for range b.N {
					began := time.Now()
					if err := s.post(s.gateway, body); err != nil {
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

// unusedAddr is a loopback address nothing listens on.
func unusedAddr(b *testing.B) string {
	b.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	return addr
}
