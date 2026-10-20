package gateway

// Benchmarks for README "Results (day 3)": what client keys, the rate limit, the totals and the cache add to a
// request. As on day 1, requests go one at a time to the mock with no simulated latency, so the times are the HTTP
// hops, the gateway's own work and its Redis round trips (to the local Redis on loopback). Run with `make bench`.

import (
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
	"github.com/vivekdavara/switchboard/internal/redistest"
)

// day3Setups are the configurations compared, each a top-level fragment and the model's cache; "plain" is day 2's.
var day3Setups = []struct {
	name, top, cache string
	key, vary        bool // send a client key; make every request different (so a cache always misses)
}{
	{name: "plain"},
	{name: "key", top: "clients", key: true},
	{name: "key+totals", top: "redis,clients", key: true},
	{name: "key+limit+totals", top: "redis,limited", key: true},
	{name: "cache-miss", top: "redis,clients", cache: `{"ttl":"1m"}`, key: true, vary: true},
	{name: "cache-hit", top: "redis,clients", cache: `{"ttl":"1m"}`, key: true},
}

func BenchmarkDay3(b *testing.B) {
	for _, setup := range day3Setups {
		b.Run(setup.name, func(b *testing.B) {
			mcfg := mock.Defaults()
			mcfg.Name, mcfg.TTFT, mcfg.PerToken, mcfg.Models = "mock-a", 0, 0, nil
			m, err := mock.New(mcfg)
			if err != nil {
				b.Fatal(err)
			}
			up := httptest.NewServer(m)
			b.Cleanup(up.Close)
			var top []string
			for _, part := range strings.Split(setup.top, ",") {
				switch part {
				case "redis":
					top = append(top, fmt.Sprintf(`"redis":{"addr":%q,"prefix":%q}`, redistest.Addr(b),
						redistest.Prefix(b)))
				case "clients":
					top = append(top, clients("alice"))
				case "limited": // a limit never reached: the round trip is the cost
					top = append(top, clients("alice:1000000:1000000"))
				}
			}
			top = append(top, mini)
			cache := ""
			if setup.cache != "" {
				cache = `,"cache":` + setup.cache
			}
			cfg, err := config.Parse([]byte(`{`+strings.Join(top, ",")+`,"providers":[{"name":"p0","base_url":`+
				fmt.Sprintf("%q", up.URL+"/v1")+`}],"models":[{"name":"fast","provider":"p0",`+
				`"upstream_model":"mock-small"`+cache+`}]}`), nil)
			if err != nil {
				b.Fatal(err)
			}
			g := New(cfg, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			b.Cleanup(func() { g.Close() })
			gw := httptest.NewServer(g)
			b.Cleanup(gw.Close)
			ct := http.DefaultTransport.(*http.Transport).Clone()
			b.Cleanup(ct.CloseIdleConnections)
			client := &http.Client{Transport: ct}
			n := 0
			post := func() {
				body := bodies["small"]
				if setup.vary {
					n++
					body = strings.Replace(body, "two plus two", fmt.Sprintf("two plus %d", n), 1)
				}
				req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if setup.key {
					req.Header.Set("Authorization", "Bearer sb-alice-key")
				}
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					b.Fatalf("status %d", resp.StatusCode)
				}
				if setup.cache != "" && !setup.vary && n > 0 && resp.Header.Get("X-Switchboard-Cache") != "hit" {
					b.Fatalf("not a hit: %v", resp.Header)
				}
			}
			for range 100 { // warm the connections, Redis's script cache, and (cache-hit) the entry
				post()
			}
			n = max(n, 1)
			took := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for range b.N {
				began := time.Now()
				post()
				took = append(took, time.Since(began))
			}
			b.StopTimer()
			slices.Sort(took)
			b.ReportMetric(float64(took[len(took)/2].Microseconds()), "p50-µs")
			b.ReportMetric(float64(took[len(took)*99/100].Microseconds()), "p99-µs")
		})
	}
}

// BenchmarkUnasked is the cost of taking the usage out of one chunk the gateway asked it of (OpenAI's "usage": null
// on every chunk), and of passing one chunk with no usage in it.
func BenchmarkUnasked(b *testing.B) {
	withNull := sseEvent{data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":" word"},"logprobs":null,"finish_reason":null}],"usage":null}`)}
	withNull.raw = append(append([]byte("data: "), withNull.data...), "\n\n"...)
	without := sseEvent{data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":" word"},"logprobs":null,"finish_reason":null}]}`)}
	without.raw = append(append([]byte("data: "), without.data...), "\n\n"...)
	for name, ev := range map[string]sseEvent{"usage-null": withNull, "no-usage": without} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if unasked(ev) == nil {
					b.Fatal("dropped")
				}
			}
		})
	}
}
