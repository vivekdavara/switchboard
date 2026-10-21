package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/vivekdavara/switchboard/internal/redistest"
)

// scrape returns every sample of g's metrics as `name{label="value",...}` (labels sorted by name; a histogram as its
// _count and _sum) -> value.
func scrape(t *testing.T, g *Gateway) map[string]float64 {
	t.Helper()
	families, err := g.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
			}
			key := func(suffix string) string { return f.GetName() + suffix + "{" + strings.Join(labels, ",") + "}" }
			switch f.GetType() {
			case dto.MetricType_COUNTER:
				out[key("")] = m.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				out[key("")] = m.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				out[key("_count")] = float64(m.GetHistogram().GetSampleCount())
				out[key("_sum")] = m.GetHistogram().GetSampleSum()
			}
		}
	}
	return out
}

// sum adds up every sample of the named metric.
func sum(samples map[string]float64, name string) float64 {
	total := 0.0
	for k, v := range samples {
		if strings.HasPrefix(k, name+"{") {
			total += v
		}
	}
	return total
}

// want checks samples against the expected values, naming every one that differs.
func want(t *testing.T, samples map[string]float64, expected map[string]float64) {
	t.Helper()
	var keys []string
	for k := range expected {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got := samples[k]; got != expected[k] {
			t.Errorf("%s = %g, want %g", k, got, expected[k])
		}
	}
}

// agreeWithTheLog checks the invariants that hold between the metrics and the log's chat completion lines whatever
// the requests were: one request per line, by status and code; their attempts; the tokens and cost of the counted
// ones, and the tokens of the uncounted ones (a usage the provider reported for an answer that then broke).
func agreeWithTheLog(t *testing.T, samples map[string]float64, lines []map[string]any) {
	t.Helper()
	byStatus := map[string]float64{}
	var attempts, prompt, completion, cost, uncountedPrompt, uncountedCompletion float64
	for _, l := range lines {
		code, _ := l["code"].(string)
		byStatus[fmt.Sprintf("%v %s", l["status"], code)]++
		if a, ok := l["attempts"].(float64); ok {
			attempts += a
		}
		tried, _ := l["tried"].([]any)
		for _, a := range tried { // an earlier attempt's usage, reported before it failed
			a := a.(map[string]any)
			if p, ok := a["prompt_tokens"].(float64); ok {
				uncountedPrompt += p
			}
			if c, ok := a["completion_tokens"].(float64); ok {
				uncountedCompletion += c
			}
		}
		if p, ok := l["prompt_tokens"].(float64); ok && l["uncounted"] == true {
			uncountedPrompt += p
			uncountedCompletion += l["completion_tokens"].(float64)
		} else if ok {
			prompt += p
			completion += l["completion_tokens"].(float64)
		}
		if c, ok := l["cost_usd"].(string); ok {
			var f float64
			fmt.Sscan(c, &f)
			cost += f
		}
	}
	got := map[string]float64{}
	for k, v := range samples {
		if !strings.HasPrefix(k, "switchboard_requests_total{") {
			continue
		}
		var code, status string
		for _, l := range strings.Split(strings.Trim(strings.TrimPrefix(k, "switchboard_requests_total"), "{}"), ",") {
			name, value, _ := strings.Cut(l, "=")
			switch name {
			case "code":
				code = strings.Trim(value, `"`)
			case "status":
				status = strings.Trim(value, `"`)
			}
		}
		got[status+" "+code] += v
	}
	for k, v := range byStatus {
		if got[k] != v {
			t.Errorf("requests with status and code %q: %g in the metrics, %g in the log", k, got[k], v)
		}
	}
	if len(got) != len(byStatus) {
		t.Errorf("statuses and codes: %v in the metrics, %v in the log", got, byStatus)
	}
	if a := sum(samples, "switchboard_attempts_total"); a != attempts {
		t.Errorf("attempts: %g in the metrics, %g in the log", a, attempts)
	}
	if p := sumKind(samples, "prompt", ""); p != prompt {
		t.Errorf("prompt tokens: %g in the metrics, %g in the log", p, prompt)
	}
	if c := sumKind(samples, "completion", ""); c != completion {
		t.Errorf("completion tokens: %g in the metrics, %g in the log", c, completion)
	}
	for kind, want := range map[string]float64{"prompt": uncountedPrompt, "completion": uncountedCompletion} {
		got := 0.0
		for k, v := range samples {
			if strings.HasPrefix(k, "switchboard_uncounted_tokens_total{") && strings.Contains(k, `kind="`+kind+`"`) {
				got += v
			}
		}
		if got != want {
			t.Errorf("uncounted %s tokens: %g in the metrics, %g in the log", kind, got, want)
		}
	}
	if c := sum(samples, "switchboard_cost_dollars_total"); fmt.Sprintf("%.9f", c) != fmt.Sprintf("%.9f", cost) {
		t.Errorf("cost: %.12f in the metrics, %.12f in the log", c, cost)
	}
}

// sumKind adds up switchboard_tokens_total of one kind, for one client ("" for all).
func sumKind(samples map[string]float64, kind, client string) float64 {
	total := 0.0
	for k, v := range samples {
		if strings.HasPrefix(k, "switchboard_tokens_total{") && strings.Contains(k, `kind="`+kind+`"`) &&
			(client == "" || strings.Contains(k, `client="`+client+`"`)) {
			total += v
		}
	}
	return total
}

func TestTheMetricsCountTheLogLinesFacts(t *testing.T) {
	good := newProvider(t, mockProvider(t, "mock-a"))
	bad := newProvider(t, answering(500, `{"error":{"message":"down","type":"server_error"}}`))
	srv, g, logs := gatewayFor(t, `"circuit_breaker":{"failures":2,"cooldown":"1m"},`+
		`"retry_budget":{"ratio":1,"min_per_second":0},`+mini,
		[]string{at(good, ""), at(bad, "")}, "fast=p0/mock-small",
		`{"name":"smart","targets":[{"provider":"p1","upstream_model":"big","weight":1},`+
			`{"provider":"p0","upstream_model":"mock-small","weight":0}]}`)

	for i, body := range []string{
		ask,                                      // answered by p0
		strings.Replace(ask, "fast", "smart", 1), // p1 fails, p0 answers
		strings.Replace(ask, "fast", "smart", 1), // p1 fails again: its breaker opens; p0 answers
		strings.Replace(ask, "fast", "smart", 1), // p1 skipped, circuit open; p0 answers
		strings.Replace(ask, "fast", "gpt-9", 1), // 404
		`{"model": "fast", "messages": [`,        // 400
		strings.Replace(ask, `}]}`, `}],"stream":true}`, 1),
	} {
		if resp, b := send(t, srv, body); (i < 4 || i == 6) && resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, b)
		}
	}
	samples := scrape(t, g)
	want(t, samples, map[string]float64{
		`switchboard_requests_total{code="",model="fast",status="200"}`:                               1 + 1,
		`switchboard_requests_total{code="",model="smart",status="200"}`:                              3,
		`switchboard_requests_total{code="model_not_found",model="",status="404"}`:                    1,
		`switchboard_requests_total{code="invalid_json",model="",status="400"}`:                       1,
		`switchboard_request_duration_seconds_count{model="fast",streamed="false"}`:                   1,
		`switchboard_request_duration_seconds_count{model="fast",streamed="true"}`:                    1,
		`switchboard_request_duration_seconds_count{model="smart",streamed="false"}`:                  3,
		`switchboard_attempts_total{outcome="answered",provider="p0"}`:                                5,
		`switchboard_attempts_total{outcome="failed",provider="p1"}`:                                  2,
		`switchboard_attempt_duration_seconds_count{outcome="answered",provider="p0"}`:                5,
		`switchboard_skips_total{provider="p1",reason="circuit_open"}`:                                1,
		`switchboard_breaker_state{provider="p1",state="open"}`:                                       1,
		`switchboard_breaker_state{provider="p1",state="closed"}`:                                     0,
		`switchboard_breaker_state{provider="p0",state="closed"}`:                                     1,
		`switchboard_breaker_opened_total{provider="p1"}`:                                             1,
		`switchboard_breaker_opened_total{provider="p0"}`:                                             0,
		`switchboard_retry_budget_refusals_total{}`:                                                   0,
		`switchboard_completions_total{client="anonymous",provider="p0",upstream_model="mock-small"}`: 5,
		`switchboard_requests_in_flight{}`:                                                            0,
	})
	if n := sum(samples, "switchboard_requests_total"); n != 7 {
		t.Errorf("%g requests counted, want 7", n)
	}
	if sum(samples, "switchboard_tokens_total") == 0 || sum(samples, "switchboard_cost_dollars_total") == 0 {
		t.Errorf("no tokens or cost: %v", samples)
	}
	for k := range samples { // no Redis, no clients: none of their series exist
		for _, absent := range []string{"switchboard_rate_limit_total", "switchboard_cache", "switchboard_redis_errors",
			"switchboard_refused_total"} {
			if strings.HasPrefix(k, absent) {
				t.Errorf("%s: %g", k, samples[k])
			}
		}
	}
	agreeWithTheLog(t, samples, logLines(t, logs, "chat completion"))
}

func TestTheRetryBudgetsRefusalsAreCounted(t *testing.T) {
	bad := newProvider(t, answering(500, `{"error":{"message":"down","type":"server_error"}}`))
	good := newProvider(t, mockProvider(t, "mock-b"))
	srv, g, logs := gatewayFor(t, `"retry_budget":{"ratio":0,"min_per_second":0}`, []string{at(bad, ""), at(good, "")},
		`{"name":"fast","targets":[{"provider":"p0","upstream_model":"m","weight":1},`+
			`{"provider":"p1","upstream_model":"m","weight":0}]}`)
	if resp, _ := send(t, srv, ask); resp.StatusCode != 502 {
		t.Fatalf("%d, want the first provider's failure", resp.StatusCode)
	}
	samples := scrape(t, g)
	want(t, samples, map[string]float64{
		`switchboard_retry_budget_refusals_total{}`:                                   1,
		`switchboard_attempts_total{outcome="failed",provider="p0"}`:                  1,
		`switchboard_requests_total{code="upstream_error",model="fast",status="502"}`: 1,
	})
	agreeWithTheLog(t, samples, logLines(t, logs, "chat completion"))
}

func TestClientsRateLimitsAndTheCacheAreCounted(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, logs := gatewayFor(t, testRedis(t, "")+","+clients("alice:60:2", "bob")+","+mini, []string{at(up, "")},
		cached("fast", `{"ttl":"1m"}`))
	for i, c := range []struct {
		who    string
		status int
	}{{"alice", 200}, {"alice", 200}, {"alice", 429}, {"bob", 200}} {
		if resp, b := send(t, srv, ask, as(c.who)...); resp.StatusCode != c.status {
			t.Fatalf("request %d, %s: %d %s", i, c.who, resp.StatusCode, b)
		}
	}
	send(t, srv, ask)                                           // no key
	send(t, srv, ask, "Authorization", "Bearer sb-mallory-key") // a key nobody has
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Host = "attacker.example"
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("%v %v", resp, err)
	}

	samples := scrape(t, g)
	want(t, samples, map[string]float64{
		`switchboard_rate_limit_total{client="alice",result="allowed"}`:                           2,
		`switchboard_rate_limit_total{client="alice",result="refused"}`:                           1,
		`switchboard_requests_total{code="rate_limit_exceeded",model="",status="429"}`:            1,
		`switchboard_requests_total{code="",model="fast",status="200"}`:                           3,
		`switchboard_cache_total{model="fast",result="miss"}`:                                     2, // alice's first, bob's
		`switchboard_cache_total{model="fast",result="hit"}`:                                      1,
		`switchboard_cache_stored_total{model="fast"}`:                                            2,
		`switchboard_attempts_total{outcome="answered",provider="p0"}`:                            2,
		`switchboard_completions_total{client="alice",provider="p0",upstream_model="mock-small"}`: 1,
		`switchboard_completions_total{client="bob",provider="p0",upstream_model="mock-small"}`:   1,
		`switchboard_refused_total{reason="missing_api_key"}`:                                     1,
		`switchboard_refused_total{reason="invalid_api_key"}`:                                     1,
		`switchboard_refused_total{reason="host_not_allowed"}`:                                    1,
	})
	for k, v := range samples {
		if strings.HasPrefix(k, "switchboard_rate_limit_total") && strings.Contains(k, "bob") {
			t.Errorf("bob has no rate limit, but %s = %g", k, v)
		}
		if strings.HasPrefix(k, "switchboard_redis_errors_total") {
			t.Errorf("%s = %g with Redis up", k, v)
		}
	}
	if sumKind(samples, "prompt", "alice") == 0 || sumKind(samples, "prompt", "alice") != sumKind(samples, "prompt", "bob") {
		t.Errorf("alice's and bob's identical requests: %g and %g prompt tokens", sumKind(samples, "prompt", "alice"),
			sumKind(samples, "prompt", "bob"))
	}
	agreeWithTheLog(t, samples, logLines(t, logs, "chat completion"))
}

func TestRedisFailuresAreCountedPerStep(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, _ := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"timeout":"50ms"},`, closedAddr(t))+
		clients("alice:60:5")+","+mini, []string{at(up, "")}, cached("fast", `{"ttl":"1m"}`))
	for range 2 {
		if resp, b := send(t, srv, ask, as("alice")...); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	want(t, scrape(t, g), map[string]float64{
		`switchboard_redis_errors_total{step="rate_limit"}`:                 2,
		`switchboard_redis_errors_total{step="cache"}`:                      2, // the lookup and the keeping: once a request
		`switchboard_redis_errors_total{step="totals"}`:                     2,
		`switchboard_rate_limit_total{client="alice",result="unavailable"}`: 2,
		`switchboard_cache_total{model="fast",result="unavailable"}`:        2,
		`switchboard_cache_stored_total{model="fast"}`:                      0,
		// Counted here even though the totals couldn't be written: the metric is the gateway's own count.
		`switchboard_completions_total{client="alice",provider="p0",upstream_model="mock-small"}`: 2,
	})
}

func TestACostIsExactHoweverManyRequestsAddUpToIt(t *testing.T) {
	c := newCosts()
	f := 0.0
	for range 1_000_000 {
		c.add("alice", "p0", "mock-small", 150_000) // $0.00000015: one token of a $0.15-a-million model
		f += 150_000 / 1e12
	}
	ch := make(chan prometheus.Metric, 1)
	go func() { c.Collect(ch); close(ch) }()
	var m dto.Metric
	if err := (<-ch).Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got != 0.15 {
		t.Errorf("$%.17f, want exactly $0.15", got)
	}
	if f == 0.15 {
		t.Log("a float counter would have been exact too this time")
	} else {
		t.Logf("a float counter would say $%.17f", f)
	}
}

func TestTheMetricsAreServedAsPrometheusText(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, _ := gatewayFor(t, mini, []string{at(up, "")}, "fast=p0/mock-small")
	send(t, srv, ask)
	scrapeAs := func(host, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		g.Metrics().ServeHTTP(rec, req)
		return rec
	}
	if rec := scrapeAs("example.com", "/metrics"); rec.Code != 403 { // httptest's default Host: not allowed
		t.Errorf("a Host the API refuses: %d", rec.Code)
	}
	if rec := scrapeAs("127.0.0.1:9108", "/v1/models"); rec.Code != 404 {
		t.Errorf("another path: %d", rec.Code)
	}
	post := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/metrics", nil)
	req.Host = "127.0.0.1:9108"
	if g.Metrics().ServeHTTP(post, req); post.Code != 405 {
		t.Errorf("another method: %d", post.Code)
	}
	rec := scrapeAs("127.0.0.1:9108", "/metrics")
	b, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	for _, line := range []string{
		`# TYPE switchboard_requests_total counter`,
		`switchboard_requests_total{code="",model="fast",status="200"} 1`,
		`# TYPE switchboard_request_duration_seconds histogram`,
		`switchboard_breaker_state{provider="p0",state="closed"} 1`,
		`# TYPE go_goroutines gauge`,
		`switchboard_refused_total{reason="host_not_allowed"} 1`,
	} {
		if !strings.Contains(string(b), line+"\n") {
			t.Errorf("no %q in:\n%s", line, b)
		}
	}
}

func TestAClientThatLeavesIsCountedAsA499AndAClosedAttempt(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, waiting(cancelled))
	srv, g, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/m")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(ask))
	req.Header.Set("Content-Type", "application/json")
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("the request finished")
	}
	// The request is counted once its handler has written the log line, a moment after the client left.
	for deadline := time.Now().Add(5 * time.Second); sum(scrape(t, g), "switchboard_requests_total") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the request was never counted")
		}
		time.Sleep(time.Millisecond)
	}
	want(t, scrape(t, g), map[string]float64{
		`switchboard_requests_total{code="",model="fast",status="499"}`:     1,
		`switchboard_attempts_total{outcome="client_closed",provider="p0"}`: 1,
		`switchboard_requests_in_flight{}`:                                  0,
	})
}

func TestAnAnswerWhoseRedisStepsFailedIsLoggedAtWarn(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"timeout":"50ms"},`, closedAddr(t))+
		clients("alice:60:5", "bob"), []string{at(up, "")}, cached("fast", `{"ttl":"1m"}`), "plain=p0/mock-small")
	send(t, srv, ask, as("alice")...)                                    // every step fails
	send(t, srv, strings.Replace(ask, "fast", "plain", 1), as("bob")...) // only the totals
	lines := logLines(t, logs, "chat completion")
	if len(lines) != 2 || lines[0]["level"] != "WARN" || lines[0]["status"] != 200.0 || lines[1]["level"] != "WARN" ||
		lines[1]["totals_err"] == nil || lines[1]["rate_limit"] != nil {
		t.Errorf("%v", lines)
	}
}

func TestAFailedRateLimitCheckAloneMakesTheLineWarn(t *testing.T) {
	// Only the rate-limit step fails: the bucket's key holds a hash, so the script's GET is WRONGTYPE; the totals (and
	// everything else) still work. (The day-4 review's mutant M1: only the totals step was pinned at WARN.)
	rdb, prefix := redistest.Client(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"prefix":%q},`, redistest.Addr(t), prefix)+
		clients("alice:60:5"), []string{at(up, "")}, "fast=p0/mock-small")
	if err := rdb.HSet(context.Background(), g.limiter.Key("alice"), "x", "1").Err(); err != nil {
		t.Fatal(err)
	}
	if resp, b := send(t, srv, ask, as("alice")...); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	l := logLines(t, logs, "chat completion")[0]
	if l["level"] != "WARN" || l["rate_limit"] != "unavailable" || l["redis_err"] == nil || l["totals_err"] != nil {
		t.Errorf("%v", l)
	}
	want(t, scrape(t, g), map[string]float64{
		`switchboard_redis_errors_total{step="rate_limit"}`:                 1,
		`switchboard_rate_limit_total{client="alice",result="unavailable"}`: 1,
	})
}

func TestTheInFlightGaugeCountsARequestWhileItRuns(t *testing.T) {
	// The day-4 review's mutant M2: removing the gauge's Inc and Dec passed, since the tests only read 0 afterwards.
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, waiting(cancelled))
	srv, g, _ := gatewayFor(t, "", []string{at(up, `"timeout":"300ms"`)}, "fast=p0/m")
	done := make(chan struct{})
	go func() { defer close(done); send(t, srv, ask) }()
	for deadline := time.Now().Add(5 * time.Second); len(up.requests()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the provider")
		}
		time.Sleep(time.Millisecond)
	}
	if n := scrape(t, g)[`switchboard_requests_in_flight{}`]; n != 1 {
		t.Errorf("%g in flight while one request waits on its provider", n)
	}
	<-done
	<-cancelled
	if n := scrape(t, g)[`switchboard_requests_in_flight{}`]; n != 0 {
		t.Errorf("%g in flight after it was answered", n)
	}
}
