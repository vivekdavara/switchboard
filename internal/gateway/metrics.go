package gateway

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/vivekdavara/switchboard/internal/breaker"
	"github.com/vivekdavara/switchboard/internal/config"
)

// metrics are the log line's facts as Prometheus counters (DESIGN.md, "Metrics (day 4)"): each chat completion is
// observed once, from its exchange, when its log line is written, so the two can't disagree. Every label's values
// come from the configuration (model, provider, upstream model and client names) or from a fixed set (statuses,
// codes, outcomes), never from what a client sends: a client naming a million models would otherwise make a million
// series. A model the gateway doesn't serve is counted with model="".
type metrics struct {
	reg *prometheus.Registry

	requests  *prometheus.CounterVec   // model, status, code
	duration  *prometheus.HistogramVec // model, streamed
	inFlight  prometheus.Gauge
	refused   *prometheus.CounterVec   // reason: before any handler (host, client key)
	attempts  *prometheus.CounterVec   // provider, outcome
	upstream  *prometheus.HistogramVec // provider, outcome
	skips     *prometheus.CounterVec   // provider, reason
	budget    prometheus.Counter       // retries the retry budget refused
	rateLimit *prometheus.CounterVec   // client, result
	cache     *prometheus.CounterVec   // model, result
	stored    *prometheus.CounterVec   // model
	redisErrs *prometheus.CounterVec   // step
	counted   *prometheus.CounterVec   // client, provider, upstream_model: completions added to the totals
	tokens    *prometheus.CounterVec   // client, provider, upstream_model, kind
	uncounted *prometheus.CounterVec   // client, provider, upstream_model, kind: reported, not counted
	cost      *costs
}

// durationBuckets run from a cache hit (a fraction of a millisecond) to a long stream (minutes), in seconds.
var durationBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

func newMetrics(breakers map[string]*breaker.Breaker) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_requests_total",
			Help: "Chat completions answered, by model (its configured name), status and the gateway's error code."},
			[]string{"model", "status", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchboard_request_duration_seconds",
			Help:    "Time from a chat completion's arrival to its log line: the whole answer, or the whole stream.",
			Buckets: durationBuckets}, []string{"model", "streamed"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "switchboard_requests_in_flight",
			Help: "Chat completions being answered now."}),
		refused: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_refused_total",
			Help: "Requests to any path refused before their handler: host_not_allowed, missing_api_key, " +
				"invalid_api_key."}, []string{"reason"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_attempts_total",
			Help: "Requests sent to a provider, by what came of them: answered, failed, unanswered, client_closed."},
			[]string{"provider", "outcome"}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchboard_attempt_duration_seconds",
			Help: "Time from sending an attempt to its outcome (for a stream, its end).", Buckets: durationBuckets},
			[]string{"provider", "outcome"}),
		skips: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_skips_total",
			Help: "Targets passed over without an attempt: circuit_open, unanswered_before."},
			[]string{"provider", "reason"}),
		budget: prometheus.NewCounter(prometheus.CounterOpts{Name: "switchboard_retry_budget_refusals_total",
			Help: "Fallbacks the retry budget refused."}),
		rateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_rate_limit_total",
			Help: "Rate-limit checks, by client and result: allowed, refused, unavailable (Redis couldn't say)."},
			[]string{"client", "result"}),
		cache: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_cache_total",
			Help: "Cache lookups of models with a cache: hit, miss, bypass, refresh, unavailable."},
			[]string{"model", "result"}),
		stored: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_cache_stored_total",
			Help: "Answers kept in the cache."}, []string{"model"}),
		redisErrs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_redis_errors_total",
			Help: "Redis steps that failed (rate_limit, cache, totals); each failed request once per step."},
			[]string{"step"}),
		counted: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_completions_total",
			Help: "Completions counted for the usage totals: answered, by client and the provider's model."},
			[]string{"client", "provider", "upstream_model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_tokens_total",
			Help: "Tokens of counted completions, as the provider reported them, by kind: prompt, completion."},
			[]string{"client", "provider", "upstream_model", "kind"}),
		uncounted: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchboard_uncounted_tokens_total",
			Help: "Tokens a provider reported for an answer that wasn't counted (a stream that broke or ended in an " +
				"error after its usage chunk, a client that left, an attempt that failed before a fallback): the " +
				"provider may still bill them."},
			[]string{"client", "provider", "upstream_model", "kind"}),
		cost: newCosts(),
	}
	m.reg.MustRegister(m.requests, m.duration, m.inFlight, m.refused, m.attempts, m.upstream, m.skips, m.budget,
		m.rateLimit, m.cache, m.stored, m.redisErrs, m.counted, m.tokens, m.uncounted, m.cost,
		&breakers_{breakers},
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// observe counts one chat completion, from the same fields its log line has.
func (m *metrics) observe(x *exchange) {
	status := strconv.Itoa(x.status)
	m.requests.WithLabelValues(x.route, status, x.code).Inc()
	m.duration.WithLabelValues(x.route, strconv.FormatBool(x.streamed)).Observe(time.Since(x.began).Seconds())
	for _, a := range x.tried {
		if a.Skipped != "" {
			m.skips.WithLabelValues(a.Provider, a.Skipped).Inc()
			continue
		}
		outcome := a.Outcome
		if a.Code == "client_closed" {
			outcome = "client_closed"
		}
		m.attempts.WithLabelValues(a.Provider, outcome).Inc()
		m.upstream.WithLabelValues(a.Provider, outcome).Observe(a.Ms / 1000)
		if a.PromptTokens != 0 || a.CompletionTokens != 0 { // an earlier attempt's usage: never counted
			m.uncounted.WithLabelValues(clientLabel(x), a.Provider, a.UpstreamModel, "prompt").Add(float64(a.PromptTokens))
			m.uncounted.WithLabelValues(clientLabel(x), a.Provider, a.UpstreamModel, "completion").
				Add(float64(a.CompletionTokens))
		}
	}
	if x.budgetSpent {
		m.budget.Inc()
	}
	client := clientLabel(x)
	if x.limited {
		result := x.rateLimit
		if result == "" {
			result = "allowed"
		}
		m.rateLimit.WithLabelValues(client, result).Inc()
	}
	if x.cache != "" {
		m.cache.WithLabelValues(x.route, x.cache).Inc()
	}
	if x.cacheStored {
		m.stored.WithLabelValues(x.route).Inc()
	}
	if x.redisErr != nil {
		m.redisErrs.WithLabelValues("rate_limit").Inc()
	}
	if x.cacheErr != nil {
		m.redisErrs.WithLabelValues("cache").Inc()
	}
	if x.totalsErr != nil {
		m.redisErrs.WithLabelValues("totals").Inc()
	}
	if x.counted {
		m.counted.WithLabelValues(client, x.provider, x.upstreamModel).Inc()
		if x.hasUsage {
			m.tokens.WithLabelValues(client, x.provider, x.upstreamModel, "prompt").Add(float64(x.usage.Prompt))
			m.tokens.WithLabelValues(client, x.provider, x.upstreamModel, "completion").
				Add(float64(x.usage.Completion))
		}
		if x.priced {
			m.cost.add(client, x.provider, x.upstreamModel, x.cost)
		}
	} else if x.hasUsage { // the log line has these tokens too, marked uncounted (found in the day-4 review)
		m.uncounted.WithLabelValues(client, x.provider, x.upstreamModel, "prompt").Add(float64(x.usage.Prompt))
		m.uncounted.WithLabelValues(client, x.provider, x.upstreamModel, "completion").Add(float64(x.usage.Completion))
	}
}

// clientLabel is the client the request is counted for: Anonymous when the file names no clients.
func clientLabel(x *exchange) string {
	if x.client == "" {
		return config.Anonymous
	}
	return x.client
}

// handler serves the metrics in Prometheus's text format (and OpenMetrics, if the scraper asks for it).
func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg, EnableOpenMetrics: true})
}

// costs is switchboard_cost_dollars_total, kept as exact integer picodollars and turned into dollars only when
// scraped: adding each request's cost to a float counter would round at every request, and the error would grow
// with the total (DESIGN.md, "Integer picodollars, not floats"). The scrape's one conversion is exact to 15
// significant digits; the exact totals are in Redis.
type costs struct {
	desc *prometheus.Desc
	mu   sync.Mutex
	pd   map[[3]string]int64 // client, provider, upstream model -> picodollars
}

func newCosts() *costs {
	return &costs{desc: prometheus.NewDesc("switchboard_cost_dollars_total",
		"Cost of counted completions in US dollars, at the configured prices.",
		[]string{"client", "provider", "upstream_model"}, nil), pd: map[[3]string]int64{}}
}

func (c *costs) add(client, provider, model string, pd int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pd[[3]string{client, provider, model}] += pd
}

func (c *costs) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *costs) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, pd := range c.pd {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, float64(pd)/1e12, k[0], k[1], k[2])
	}
}

// breakers_ reads every provider's breaker when scraped: switchboard_breaker_state, 1 for the state it is in and 0
// for the other two (so a graph of state="open" is the time it was open), and switchboard_breaker_opened_total.
type breakers_ struct{ b map[string]*breaker.Breaker }

var (
	breakerState = prometheus.NewDesc("switchboard_breaker_state",
		"A provider's circuit breaker: 1 for its state now (closed, open, half-open), 0 for the others.",
		[]string{"provider", "state"}, nil)
	breakerOpened = prometheus.NewDesc("switchboard_breaker_opened_total",
		"Times a provider's breaker opened (from closed, or again from half-open after a failed probe).",
		[]string{"provider"}, nil)
)

func (b *breakers_) Describe(ch chan<- *prometheus.Desc) {
	ch <- breakerState
	ch <- breakerOpened
}

func (b *breakers_) Collect(ch chan<- prometheus.Metric) {
	for name, br := range b.b {
		now := br.State()
		for _, s := range []breaker.State{breaker.Closed, breaker.Open, breaker.HalfOpen} {
			v := 0.0
			if s == now {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(breakerState, prometheus.GaugeValue, v, name, s.String())
		}
		ch <- prometheus.MustNewConstMetric(breakerOpened, prometheus.CounterValue, float64(br.Opened()), name)
	}
}
