// Package config loads the gateway's configuration: where it listens, its limits, the providers it forwards to and
// the models it serves.
//
// The file is JSON, read as strictly as a request: jsonstrict first (a key given twice is an error, not a silent
// override), then every key must be spelled exactly as documented. encoding/json's DisallowUnknownFields isn't
// enough on its own: it still fills a field from a key in any case, so a misspelt "Timeout" would pass as
// "timeout" where a reader expects it to be refused, and two spellings of one key would both be accepted.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vivekdavara/switchboard/internal/jsonstrict"
)

// Defaults for what the file leaves out.
const (
	DefaultListen           = "127.0.0.1:8108"
	DefaultMaxRequestBytes  = 4 << 20
	DefaultMaxResponseBytes = 16 << 20
	DefaultShutdownGrace    = 10 * time.Second
	DefaultTimeout          = 60 * time.Second
	DefaultWeight           = 1
)

// DefaultRetryBudget and DefaultBreaker are what the file's retry_budget and circuit_breaker default to, each field on
// its own.
var (
	DefaultRetryBudget = RetryBudget{Ratio: 0.2, MinPerSecond: 1}
	DefaultBreaker     = Breaker{Failures: 5, Cooldown: 10 * time.Second}
)

// MaxWeight bounds a target's weight, so the sum of a model's weights can't overflow.
const MaxWeight = 1000

// MaxBytes bounds both size limits. The gateway holds a whole request and a whole answer in memory, so a limit past
// this is a mistake, and a limit near math.MaxInt64 overflowed the gateway's limit+1 read (found in the day-1 review:
// max_response_bytes 9223372036854775807 turned every answer into a 502).
const MaxBytes = 1 << 30

// Config is a loaded, checked configuration.
type Config struct {
	Listen           string
	MaxRequestBytes  int64
	MaxResponseBytes int64
	ShutdownGrace    time.Duration
	// DrainDelay is how long a stopping gateway keeps serving with /readyz at 503 before it closes its listener, so
	// a load balancer that polls /readyz stops sending first. 0 (the default) closes at once.
	DrainDelay time.Duration
	// AllowedHosts are the host names (without ports, lower case) a request's Host header may name; "*" allows any.
	// The default is the loopback names: a web page whose own domain is made to resolve to 127.0.0.1 (DNS
	// rebinding) is same-origin with a gateway it reaches that way, so it can post JSON with no preflight, and only
	// its Host header gives it away.
	AllowedHosts []string
	Providers    []*Provider
	Models       []Model
	RetryBudget  RetryBudget
	Breaker      Breaker
	// Redis is where rate limits, cached answers and usage totals live (day 3); nil if the file names none, and then
	// there are no limits, no cache and no totals (the log still has every request's usage).
	Redis *Redis
	// Clients are the holders of keys for the gateway (day 3). With none, the gateway asks for no key, as on days 1
	// and 2, and every request is counted as Anonymous's.
	Clients []*Client
	// Metrics is where the Prometheus metrics are served (day 4); nil if the file names none: the gateway still
	// counts, but nothing serves the counts.
	Metrics *Metrics
	// Log is how the gateway logs (day 4).
	Log Log

	routes        map[string]Model // by name and by alias
	clientsByHash map[[32]byte]*Client
	prices        map[[2]string]Price // by provider and upstream model
}

// RetryBudget bounds the attempts the gateway makes after a request's first, across all requests: in any 10 seconds
// it makes at most Ratio of them per request that made an attempt, plus MinPerSecond of them a second. When a provider fails,
// fallback adds load to the others just when they are busiest; the budget caps that extra load. Ratio 0 and
// MinPerSecond 0 turn fallback off.
type RetryBudget struct {
	Ratio        float64 // from 0 to 1
	MinPerSecond float64 // from 0 to 1000
}

// Breaker is how each provider's circuit breaker behaves: Failures failed attempts in a row open it, and after
// Cooldown one request is let through as a probe, whose result closes it or opens it again. Failures 0 turns the
// breakers off.
type Breaker struct {
	Failures int
	Cooldown time.Duration
}

// Provider is an upstream that speaks the OpenAI API.
type Provider struct {
	Name    string
	BaseURL string // absolute, without a trailing slash: requests go to BaseURL + "/chat/completions"
	Timeout time.Duration
	// APIKey is the value of the environment variable APIKeyEnv names, read at load. It is never in the file and
	// never logged.
	APIKey    string
	APIKeyEnv string
	// StreamUsage says whether the gateway asks this provider for a stream's usage (stream_options.include_usage)
	// when the client didn't, taking the usage out again before the client sees it (DESIGN.md, "Accounting"). On by
	// default; turn it off for a provider that refuses stream_options.
	StreamUsage bool
}

// Model is a model the gateway serves: its name, the other names clients may use for it, and the targets that can
// serve it.
type Model struct {
	Name    string
	Aliases []string
	Targets []Target
	// MaxAttempts is how many attempts one request may make, the first included; 1 means no retries. A target
	// skipped because its breaker is open isn't an attempt, so with 1 the next target is still tried in its place.
	// It is at most len(Targets): a request never tries one target twice (a client's SDK retries on its own, with
	// backoff).
	MaxAttempts int
	// Cache keeps this model's answers in Redis (day 3); nil: no caching.
	Cache *Cache
}

// Target is one way to serve a model: a provider, and the provider's name for the model. A request's first attempt
// goes to a target picked at random in proportion to the weights; a target of weight 0 is a standby, tried only
// after the others.
type Target struct {
	Provider      *Provider
	UpstreamModel string
	Weight        int
}

// Route returns the model a client asked for, by its name or one of its aliases.
func (c *Config) Route(name string) (Model, bool) {
	m, ok := c.routes[name]
	return m, ok
}

// Load reads and checks the file at path, taking provider keys from getenv.
func Load(path string, getenv func(string) string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b, getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

var (
	// Provider names go into a response header, so they keep to characters any header value can hold.
	providerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// conventionalEnvName is how variables are named by convention, and how no provider's key looks.
	conventionalEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// Parse checks a configuration file's contents, taking provider keys from getenv.
func Parse(b []byte, getenv func(string) string) (*Config, error) {
	top, err := jsonstrict.ParseObject(b)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Listen:           DefaultListen,
		MaxRequestBytes:  DefaultMaxRequestBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
		ShutdownGrace:    DefaultShutdownGrace,
		RetryBudget:      DefaultRetryBudget,
		Breaker:          DefaultBreaker,
		routes:           map[string]Model{},
	}
	var grace, drain string
	var hosts []string
	var providers, models []json.RawMessage
	var budget, breaker, redis json.RawMessage
	var clients, prices []json.RawMessage
	var metrics, logging json.RawMessage
	if err := decodeExact("the file", top, fields{
		"listen":             &c.Listen,
		"max_request_bytes":  &c.MaxRequestBytes,
		"max_response_bytes": &c.MaxResponseBytes,
		"shutdown_grace":     &grace,
		"drain_delay":        &drain,
		"allowed_hosts":      &hosts,
		"retry_budget":       &budget,
		"circuit_breaker":    &breaker,
		"providers":          &providers,
		"models":             &models,
		"redis":              &redis,
		"clients":            &clients,
		"prices":             &prices,
		"metrics":            &metrics,
		"log":                &logging,
	}); err != nil {
		return nil, err
	}
	if budget != nil {
		if err := parseRetryBudget(budget, &c.RetryBudget); err != nil {
			return nil, fmt.Errorf("retry_budget: %w", err)
		}
	}
	if breaker != nil {
		if err := parseBreaker(breaker, &c.Breaker); err != nil {
			return nil, fmt.Errorf("circuit_breaker: %w", err)
		}
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nil, fmt.Errorf("listen: %q is not a host:port address", c.Listen)
	}
	for _, size := range []struct {
		name  string
		value int64
	}{{"max_request_bytes", c.MaxRequestBytes}, {"max_response_bytes", c.MaxResponseBytes}} {
		if size.value < 1 || size.value > MaxBytes {
			return nil, fmt.Errorf("%s must be from 1 to %d (1 GiB): the gateway holds a whole body in memory",
				size.name, MaxBytes)
		}
	}
	if grace != "" {
		if c.ShutdownGrace, err = time.ParseDuration(grace); err != nil || c.ShutdownGrace < 0 {
			return nil, fmt.Errorf("shutdown_grace: %q is not a duration such as 10s", grace)
		}
	}
	if drain != "" {
		if c.DrainDelay, err = time.ParseDuration(drain); err != nil || c.DrainDelay < 0 {
			return nil, fmt.Errorf("drain_delay: %q is not a duration such as 5s", drain)
		}
	}
	if c.AllowedHosts, err = allowedHosts(hosts); err != nil {
		return nil, err
	}

	if len(providers) == 0 {
		return nil, errors.New("providers: at least one is required")
	}
	byName := map[string]*Provider{}
	for i, raw := range providers {
		p, err := parseProvider(raw, getenv)
		if err != nil {
			return nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
		if byName[p.Name] != nil {
			return nil, fmt.Errorf("providers[%d]: the name %q is used twice", i, p.Name)
		}
		byName[p.Name] = p
		c.Providers = append(c.Providers, p)
	}

	if len(models) == 0 {
		return nil, errors.New("models: at least one is required")
	}
	for i, raw := range models {
		m, err := parseModel(raw, byName)
		if err == nil {
			// Names and aliases share one namespace: a name a client sends must mean one model.
			for _, name := range append([]string{m.Name}, m.Aliases...) {
				if _, taken := c.routes[name]; taken {
					err = fmt.Errorf("the name %q is used twice (model names and aliases share one namespace)", name)
					break
				}
				c.routes[name] = m
			}
		}
		if err != nil {
			return nil, fmt.Errorf("models[%d]: %w", i, err)
		}
		c.Models = append(c.Models, m)
	}
	_, hasClients := member(top, "clients")
	if err := c.parseDay3(clients, hasClients, redis, prices, byName, getenv); err != nil {
		return nil, err
	}
	if err := c.parseDay4(metrics, logging); err != nil {
		return nil, err
	}
	return c, nil
}

// parseModel reads one model. It has either a provider (and an upstream model, its own name by default), as on
// day 1, or a list of targets, each with a provider, an upstream model and a weight.
func parseModel(raw json.RawMessage, byName map[string]*Provider) (Model, error) {
	var m Model
	members, err := object(raw)
	if err != nil {
		return m, err
	}
	var provider, upstream string
	var aliases []string
	var targets []json.RawMessage
	var cache json.RawMessage
	maxAttempts := 0
	if err := decodeExact("the model", members, fields{
		"name": &m.Name, "aliases": &aliases, "provider": &provider, "upstream_model": &upstream,
		"targets": &targets, "max_attempts": &maxAttempts, "cache": &cache,
	}); err != nil {
		return m, err
	}
	if cache != nil {
		if m.Cache, err = parseCache(cache); err != nil {
			return m, fmt.Errorf("cache: %w", err)
		}
	}
	_, hasProvider := member(members, "provider")
	_, hasUpstream := member(members, "upstream_model")
	_, hasTargets := member(members, "targets")
	_, hasMax := member(members, "max_attempts")
	switch {
	case m.Name == "":
		return m, errors.New("name is required")
	case hasTargets && (hasProvider || hasUpstream):
		return m, errors.New("give either provider (and upstream_model) or targets, not both")
	case !hasTargets && !hasProvider:
		return m, errors.New("a provider or a list of targets is required")
	}
	for j, a := range aliases {
		if a == "" {
			return m, fmt.Errorf("aliases[%d] must be a non-empty string", j)
		}
	}
	m.Aliases = aliases

	if hasProvider {
		if byName[provider] == nil {
			return m, fmt.Errorf("provider %q is not one of the providers", provider)
		}
		if upstream == "" {
			upstream = m.Name
		}
		m.Targets = []Target{{Provider: byName[provider], UpstreamModel: upstream, Weight: DefaultWeight}}
	} else {
		if len(targets) == 0 {
			return m, errors.New("targets is empty; a model needs at least one")
		}
		seen := map[[2]string]bool{}
		for j, raw := range targets {
			t, err := parseTarget(raw, m.Name, byName)
			if err != nil {
				return m, fmt.Errorf("targets[%d]: %w", j, err)
			}
			key := [2]string{t.Provider.Name, t.UpstreamModel}
			if seen[key] {
				return m, fmt.Errorf("targets[%d]: %s as %q is a target twice", j, t.Provider.Name, t.UpstreamModel)
			}
			seen[key] = true
			m.Targets = append(m.Targets, t)
		}
	}
	if !slices.ContainsFunc(m.Targets, func(t Target) bool { return t.Weight > 0 }) {
		return m, errors.New("every target has weight 0, so none would be tried first")
	}

	m.MaxAttempts = len(m.Targets)
	if hasMax {
		if maxAttempts < 1 || maxAttempts > len(m.Targets) {
			return m, fmt.Errorf("max_attempts must be from 1 to the number of targets (%d): a request tries each "+
				"target at most once", len(m.Targets))
		}
		m.MaxAttempts = maxAttempts
	}
	return m, nil
}

func parseTarget(raw json.RawMessage, model string, byName map[string]*Provider) (Target, error) {
	t := Target{Weight: DefaultWeight}
	members, err := object(raw)
	if err != nil {
		return t, err
	}
	var provider string
	if err := decodeExact("the target", members, fields{
		"provider": &provider, "upstream_model": &t.UpstreamModel, "weight": &t.Weight,
	}); err != nil {
		return t, err
	}
	if t.Provider = byName[provider]; t.Provider == nil {
		return t, fmt.Errorf("provider %q is not one of the providers", provider)
	}
	if t.UpstreamModel == "" {
		t.UpstreamModel = model
	}
	if t.Weight < 0 || t.Weight > MaxWeight {
		return t, fmt.Errorf("weight must be an integer from 0 to %d", MaxWeight)
	}
	return t, nil
}

func parseRetryBudget(raw json.RawMessage, b *RetryBudget) error {
	members, err := object(raw)
	if err != nil {
		return err
	}
	if err := decodeExact("retry_budget", members, fields{
		"ratio": &b.Ratio, "min_per_second": &b.MinPerSecond,
	}); err != nil {
		return err
	}
	switch {
	case !(b.Ratio >= 0 && b.Ratio <= 1):
		return errors.New("ratio must be from 0 to 1 (retries per request received)")
	case !(b.MinPerSecond >= 0 && b.MinPerSecond <= 1000):
		return errors.New("min_per_second must be from 0 to 1000")
	}
	return nil
}

func parseBreaker(raw json.RawMessage, b *Breaker) error {
	members, err := object(raw)
	if err != nil {
		return err
	}
	var cooldown string
	if err := decodeExact("circuit_breaker", members, fields{
		"failures": &b.Failures, "cooldown": &cooldown,
	}); err != nil {
		return err
	}
	if b.Failures < 0 || b.Failures > 1000 {
		return errors.New("failures must be from 0 (no breakers) to 1000")
	}
	if cooldown != "" {
		d, err := time.ParseDuration(cooldown)
		if err != nil || d <= 0 {
			return fmt.Errorf("cooldown: %q is not a positive duration such as 10s", cooldown)
		}
		b.Cooldown = d
	}
	return nil
}

// member returns the value of an object's key, if it has it.
func member(members []jsonstrict.Member, key string) (json.RawMessage, bool) {
	for _, m := range members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

func parseProvider(raw json.RawMessage, getenv func(string) string) (*Provider, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	p := &Provider{Timeout: DefaultTimeout, StreamUsage: true}
	var base, timeout string
	if err := decodeExact("the provider", members, fields{
		"name": &p.Name, "base_url": &base, "api_key_env": &p.APIKeyEnv, "timeout": &timeout,
		"stream_usage": &p.StreamUsage,
	}); err != nil {
		return nil, err
	}
	if !providerName.MatchString(p.Name) {
		return nil, fmt.Errorf("name %q: use 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or "+
			"digit", p.Name)
	}
	// None of these errors repeats the URL, not even redacted: a key pasted into it can sit in the user name, the
	// password or the query, and an unparseable URL can't be redacted at all (url.Error quotes it whole). The
	// provider's index says which one is wrong (found in the day-1 review: u.Redacted() hid only a password).
	u, err := url.Parse(base)
	switch {
	case base == "":
		return nil, errors.New("base_url is required")
	case err != nil:
		return nil, errors.New("base_url can't be parsed as a URL (not shown: it may hold a secret)")
	case u.User != nil:
		return nil, errors.New("base_url has credentials before an @; put the key in an environment variable " +
			"and name it in api_key_env")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("base_url must be an absolute http or https URL")
	case u.Host == "":
		return nil, errors.New("base_url has no host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#"):
		return nil, errors.New("base_url must not have a query or a fragment (a key in a query belongs in " +
			"api_key_env)")
	}
	p.BaseURL = strings.TrimRight(base, "/")
	if timeout != "" {
		if p.Timeout, err = time.ParseDuration(timeout); err != nil || p.Timeout <= 0 {
			return nil, fmt.Errorf("timeout: %q is not a positive duration such as 30s", timeout)
		}
	}
	if p.APIKeyEnv != "" {
		// api_key_env is where a key is likeliest to be pasted by mistake, so its errors show the value only when it
		// looks like a variable's name by convention, in capitals: sk-proj-... and gsk_... never do (found in the
		// day-1 second review: both were printed whole).
		shown := "named in api_key_env (not shown: it isn't in capitals, so it may be a pasted key)"
		if conventionalEnvName.MatchString(p.APIKeyEnv) {
			shown = p.APIKeyEnv + " (api_key_env)"
		}
		if !envName.MatchString(p.APIKeyEnv) {
			return nil, errors.New("api_key_env isn't an environment variable's name (not shown: it may be a " +
				"pasted key); name the variable that holds the key, such as OPENAI_API_KEY")
		}
		if p.APIKey = getenv(p.APIKeyEnv); p.APIKey == "" {
			return nil, fmt.Errorf("the environment variable %s is unset or empty", shown)
		}
		// A key goes into a header, where a space or a control character either breaks the request (net/http
		// refuses it, and every request then looked like an unreachable provider, a suspicion of the day-1 review
		// confirmed with a trailing \r) or changes the key. The usual cause is a line ending from a .env file.
		if strings.IndexFunc(p.APIKey, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("the environment variable %s holds a space or a control character (a line "+
				"ending from a .env file?); no key does", shown)
		}
	}
	return p, nil
}

// DefaultAllowedHosts are the Host names a gateway accepts when the file names none.
var DefaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// hostName is a DNS name, with '_' allowed: net/http accepts it in a Host, and Docker Compose names services and
// containers with it (my_gateway, project_gateway_1; found in the day-1 second review).
var hostName = regexp.MustCompile(`^[a-z0-9_]([a-z0-9._-]*[a-z0-9_])?$`)

// allowedHosts checks the file's allowed_hosts, giving the defaults if it named none.
func allowedHosts(hosts []string) ([]string, error) {
	if hosts == nil {
		return DefaultAllowedHosts, nil
	}
	if len(hosts) == 0 {
		return nil, errors.New("allowed_hosts is empty, so every request would be refused; leave it out for the " +
			`loopback names, or use ["*"] for any`)
	}
	out := make([]string, len(hosts))
	for i, h := range hosts {
		h = strings.ToLower(h)
		if ip := net.ParseIP(h); ip != nil {
			h = ip.String() // one spelling per address: 0:0:0:0:0:0:0:1 is ::1, as the gateway compares it
		} else if h != "*" && !hostName.MatchString(h) {
			return nil, fmt.Errorf("allowed_hosts[%d] %q: a host name or IP address, without a port or brackets", i,
				hosts[i])
		}
		out[i] = h
	}
	return out, nil
}

// fields maps each key an object may have to where its value goes.
type fields map[string]any

// decodeExact decodes each member into its field, refusing keys that aren't spelled exactly as one of the fields.
func decodeExact(what string, members []jsonstrict.Member, into fields) error {
	for _, m := range members {
		dst, ok := into[m.Key]
		if !ok {
			known := make([]string, 0, len(into))
			for k := range into {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("%s has an unknown key %q (known: %s)", what, m.Key, strings.Join(known, ", "))
		}
		if string(m.Value) == "null" {
			return fmt.Errorf("%s: %s is null; leave it out for the default", what, m.Key)
		}
		if err := json.Unmarshal(m.Value, dst); err != nil {
			return fmt.Errorf("%s: %s: %w", what, m.Key, err)
		}
	}
	return nil
}

// object reads a nested value that must be an object. jsonstrict has already checked the whole file, so only the
// type can be wrong.
func object(raw json.RawMessage) ([]jsonstrict.Member, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("must be an object")
	}
	return jsonstrict.ParseObject(raw)
}
