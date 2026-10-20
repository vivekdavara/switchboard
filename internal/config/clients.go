package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Day 3's configuration: the client keys and their rate limits, the Redis they are counted in, response caching per
// model, and the prices tokens are charged at. DESIGN.md, "Client keys", "Rate limits", "Caching", "Accounting".

// Defaults for day 3's keys.
const (
	DefaultRedisPrefix  = "switchboard:"
	DefaultRedisTimeout = 100 * time.Millisecond
	// Anonymous is the client every request is counted as when the file names no clients. No client may take the
	// name.
	Anonymous = "anonymous"
)

// Bounds a file can't go past.
const (
	MaxRequestsPerMinute = 1_000_000
	MaxBurst             = 1_000_000
	MaxRefillMinutes     = 30 * 24 * 60 // the longest a bucket may take to refill: 30 days
	MaxCacheTTL          = 30 * 24 * time.Hour
	MaxRedisTimeout      = 10 * time.Second
	// MaxPricePerMillion bounds a price, in dollars per million tokens, so a cost can't overflow (DESIGN.md,
	// "Accounting"): the dearest models cost hundreds.
	MaxPricePerMillion = 1000
)

// Redis is where rate limits, cached answers and usage totals live, shared by every gateway that names the same one.
type Redis struct {
	Addr string
	// Password is the value of the environment variable PasswordEnv names, read at load; empty for none.
	Password    string
	PasswordEnv string
	// Prefix starts every key the gateway writes, so several gateways (or tests) can share one Redis.
	Prefix string
	// Timeout bounds each command. A Redis that doesn't answer in time is treated as down.
	Timeout time.Duration
	// RefuseWhenDown says what a rate-limited client's request gets when Redis can't say whether it is within its
	// limit: refused (503) if true; let through, unlimited, if false (the default).
	RefuseWhenDown bool
}

// Client is a holder of a key for the gateway.
type Client struct {
	Name string
	// KeyHash is the SHA-256 of the client's key. The file holds only the hash, so it can be checked in; `switchboard
	// keygen` makes a key and prints both.
	KeyHash   [sha256.Size]byte
	RateLimit *RateLimit // nil: no limit
}

// RateLimit is a token bucket: Burst requests at once, refilled at PerMinute a minute.
type RateLimit struct {
	PerMinute float64
	Burst     int
}

// Cache is a model's response cache: answers kept for TTL, per client unless Shared.
type Cache struct {
	TTL time.Duration
	// Shared lets every client get an answer another client's identical request was given. Off by default: a hit
	// tells a client that someone asked exactly this before.
	Shared bool
}

// Price is what a provider charges for one upstream model, in picodollars (10^-12 USD) per token, which is the same
// number as microdollars per million tokens: $0.15 per million is 150,000.
type Price struct {
	Input, Output int64
}

// Client returns the client whose key this is, if any.
func (c *Config) Client(key string) (*Client, bool) {
	cl, ok := c.clientsByHash[sha256.Sum256([]byte(key))]
	return cl, ok
}

// Price returns the price of a provider's upstream model, if the file gives one.
func (c *Config) Price(provider, upstreamModel string) (Price, bool) {
	p, ok := c.prices[[2]string{provider, upstreamModel}]
	return p, ok
}

// Prices returns the price table, for the check command.
func (c *Config) Prices() map[[2]string]Price { return c.prices }

// clientName is a provider name's pattern: client names go into logs and Redis keys, and ':' (a key's separator)
// can't be in one.
var clientName = providerName

func parseRedis(raw json.RawMessage, getenv func(string) string) (*Redis, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	r := &Redis{Prefix: DefaultRedisPrefix, Timeout: DefaultRedisTimeout}
	var timeout, onError string
	if err := decodeExact("redis", members, fields{
		"addr": &r.Addr, "password_env": &r.PasswordEnv, "prefix": &r.Prefix, "timeout": &timeout,
		"on_error": &onError,
	}); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(r.Addr); err != nil {
		return nil, errors.New("addr must be a host:port address, such as 127.0.0.1:6382")
	}
	if r.Prefix == "" || len(r.Prefix) > 64 || strings.IndexFunc(r.Prefix, func(c rune) bool {
		return c <= ' ' || c >= 0x7f
	}) >= 0 {
		return nil, errors.New("prefix must be 1 to 64 printable ASCII characters without spaces")
	}
	if timeout != "" {
		if r.Timeout, err = time.ParseDuration(timeout); err != nil || r.Timeout <= 0 || r.Timeout > MaxRedisTimeout {
			return nil, fmt.Errorf("timeout: %q is not a positive duration of at most %s, such as 100ms", timeout,
				MaxRedisTimeout)
		}
	}
	switch onError {
	case "", "allow":
	case "refuse":
		r.RefuseWhenDown = true
	default:
		return nil, fmt.Errorf(`on_error must be "allow" or "refuse", not %q`, onError)
	}
	if r.PasswordEnv != "" {
		if !envName.MatchString(r.PasswordEnv) {
			return nil, errors.New("password_env isn't an environment variable's name (not shown: it may be a " +
				"pasted password)")
		}
		if r.Password = getenv(r.PasswordEnv); r.Password == "" {
			shown := "named in password_env"
			if conventionalEnvName.MatchString(r.PasswordEnv) {
				shown = r.PasswordEnv
			}
			return nil, fmt.Errorf("the environment variable %s is unset or empty", shown)
		}
	}
	return r, nil
}

func parseClient(raw json.RawMessage) (*Client, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	cl := &Client{}
	var hash string
	var limit json.RawMessage
	if err := decodeExact("the client", members, fields{
		"name": &cl.Name, "key_sha256": &hash, "rate_limit": &limit,
	}); err != nil {
		return nil, err
	}
	switch {
	case !clientName.MatchString(cl.Name):
		return nil, fmt.Errorf("name %q: use 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or "+
			"digit", cl.Name)
	case cl.Name == Anonymous:
		return nil, fmt.Errorf("the name %q is the one requests without clients are counted under", Anonymous)
	}
	// The hash, not the key: a key pasted here by mistake is 64 characters only by coincidence, and isn't shown.
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("key_sha256 must be 64 hexadecimal digits, the SHA-256 of the key (not shown: it " +
			"may be a pasted key); switchboard keygen makes a key and prints its hash")
	}
	copy(cl.KeyHash[:], b)
	if limit != nil {
		if cl.RateLimit, err = parseRateLimit(limit); err != nil {
			return nil, fmt.Errorf("rate_limit: %w", err)
		}
	}
	return cl, nil
}

func parseRateLimit(raw json.RawMessage) (*RateLimit, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	l := &RateLimit{}
	if err := decodeExact("rate_limit", members, fields{
		"requests_per_minute": &l.PerMinute, "burst": &l.Burst,
	}); err != nil {
		return nil, err
	}
	if !(l.PerMinute > 0 && l.PerMinute <= MaxRequestsPerMinute) {
		return nil, fmt.Errorf("requests_per_minute must be above 0 and at most %d", MaxRequestsPerMinute)
	}
	if _, given := member(members, "burst"); !given {
		l.Burst = int(math.Ceil(l.PerMinute)) // a minute's requests at once
	} else if l.Burst < 1 || l.Burst > MaxBurst {
		return nil, fmt.Errorf("burst must be an integer from 1 to %d", MaxBurst)
	}
	// The bucket's span (the time a burst takes to refill) is kept in whole microseconds in Redis's Lua, whose
	// numbers are doubles: exact below 2^53. A bound of 30 days keeps it far below that, and Go's int64 of
	// nanoseconds too (a rate of 10^-9 a minute overflowed both before; found in the day-3 review).
	if float64(l.Burst)/l.PerMinute > MaxRefillMinutes {
		return nil, fmt.Errorf("a burst of %d at %g requests a minute takes more than 30 days to refill", l.Burst,
			l.PerMinute)
	}
	return l, nil
}

func parseCache(raw json.RawMessage) (*Cache, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	c := &Cache{}
	var ttl string
	if err := decodeExact("cache", members, fields{"ttl": &ttl, "shared": &c.Shared}); err != nil {
		return nil, err
	}
	if c.TTL, err = time.ParseDuration(ttl); err != nil || c.TTL < time.Millisecond || c.TTL > MaxCacheTTL {
		return nil, fmt.Errorf("ttl: %q is not a duration from 1ms to %s, such as 10m", ttl, MaxCacheTTL)
	}
	return c, nil
}

func parsePrice(raw json.RawMessage, byName map[string]*Provider) (key [2]string, p Price, err error) {
	members, err := object(raw)
	if err != nil {
		return key, p, err
	}
	var provider, upstream string
	var input, output json.Number
	if err := decodeExact("the price", members, fields{
		"provider": &provider, "upstream_model": &upstream, "input_per_million": &input,
		"output_per_million": &output,
	}); err != nil {
		return key, p, err
	}
	switch {
	case byName[provider] == nil:
		return key, p, fmt.Errorf("provider %q is not one of the providers", provider)
	case upstream == "":
		return key, p, errors.New("upstream_model is required: a price is the provider's, for one of its models")
	}
	for _, f := range []struct {
		name string
		text json.Number
		into *int64
	}{{"input_per_million", input, &p.Input}, {"output_per_million", output, &p.Output}} {
		if f.text == "" {
			return key, p, fmt.Errorf("%s is required (dollars per million tokens; 0 for free)", f.name)
		}
		if raw, _ := member(members, f.name); raw[0] == '"' { // json.Number takes a quoted number too
			return key, p, fmt.Errorf("%s must be a number, not a string", f.name)
		}
		if *f.into, err = microdollars(string(f.text)); err != nil {
			return key, p, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return [2]string{provider, upstream}, p, nil
}

// decimal is a price as the file may write it: digits, then at most six decimal places. No exponent and no sign, so
// it reads the same to every reader and converts to whole microdollars exactly.
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})(\.[0-9]{1,6})?$`)

// microdollars converts dollars, written as a decimal, to whole microdollars, exactly (no float on the way).
func microdollars(dollars string) (int64, error) {
	if !decimal.MatchString(dollars) {
		return 0, fmt.Errorf("%s: write dollars per million tokens as a plain decimal with at most 6 places, "+
			"such as 0.15", dollars)
	}
	whole, frac, _ := strings.Cut(dollars, ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	if w*1_000_000+f > MaxPricePerMillion*1_000_000 {
		return 0, fmt.Errorf("%s is more than %d dollars per million tokens", dollars, MaxPricePerMillion)
	}
	return w*1_000_000 + f, nil
}

// parseDay3 reads the file's clients, redis and prices, and checks that every feature that needs Redis has one.
func (c *Config) parseDay3(clients []json.RawMessage, hasClients bool, redisRaw json.RawMessage,
	prices []json.RawMessage, byName map[string]*Provider, getenv func(string) string) error {
	if redisRaw != nil {
		r, err := parseRedis(redisRaw, getenv)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		c.Redis = r
	}
	if hasClients && len(clients) == 0 {
		return errors.New("clients is empty, so every request would be refused; leave it out to serve without " +
			"client keys")
	}
	c.clientsByHash = map[[sha256.Size]byte]*Client{}
	names := map[string]bool{}
	for i, raw := range clients {
		cl, err := parseClient(raw)
		if err != nil {
			return fmt.Errorf("clients[%d]: %w", i, err)
		}
		if names[cl.Name] {
			return fmt.Errorf("clients[%d]: the name %q is used twice", i, cl.Name)
		}
		if c.clientsByHash[cl.KeyHash] != nil {
			return fmt.Errorf("clients[%d]: the same key_sha256 as %s: two clients can't share a key", i,
				c.clientsByHash[cl.KeyHash].Name)
		}
		if cl.RateLimit != nil && c.Redis == nil {
			return fmt.Errorf("clients[%d]: a rate_limit needs redis (the limits are counted there)", i)
		}
		names[cl.Name] = true
		c.clientsByHash[cl.KeyHash] = cl
		c.Clients = append(c.Clients, cl)
	}
	c.prices = map[[2]string]Price{}
	for i, raw := range prices {
		key, p, err := parsePrice(raw, byName)
		if err != nil {
			return fmt.Errorf("prices[%d]: %w", i, err)
		}
		if _, dup := c.prices[key]; dup {
			return fmt.Errorf("prices[%d]: %s's %q is priced twice", i, key[0], key[1])
		}
		c.prices[key] = p
	}
	for i, m := range c.Models {
		if m.Cache != nil && c.Redis == nil {
			return fmt.Errorf("models[%d]: a cache needs redis (the answers are kept there)", i)
		}
	}
	return nil
}
