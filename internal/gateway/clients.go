package gateway

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/openai"
)

// Day 3's front door: client keys and rate limits (DESIGN.md, "Client keys" and "Rate limits"). A request must carry
// a client's key (when the file names clients), and a client with a rate limit gets one request from its token
// bucket in Redis before anything else is done for it: a refused request costs the gateway one Redis round trip, and
// nothing upstream. The limit counts the client's requests, not the gateway's attempts: a fallback is the gateway's
// cost, not the client's.

// NewRedis is the gateway's Redis client: every command bounded by the configured timeout, no retries (a command
// that failed once is answered as Redis being down, rather than made slower), and none of the handshakes a plain
// Redis may not know.
func NewRedis(c *config.Redis) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                     c.Addr,
		Password:                 c.Password,
		DialTimeout:              c.Timeout,
		ReadTimeout:              c.Timeout,
		WriteTimeout:             c.Timeout,
		PoolTimeout:              c.Timeout,
		ContextTimeoutEnabled:    true,
		MaxRetries:               -1,
		DialerRetries:            1,
		DisableIdentity:          true,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	})
}

// quiet discards go-redis's own log lines (a failed dial, a broken pool connection): the request's log line already
// says what Redis's failure did to it, once per request, in the gateway's JSON.
type quiet struct{}

func (quiet) Printf(context.Context, string, ...any) {}

type clientKey struct{}

// clientOf returns the client a request was authenticated as; nil when the file names no clients.
func clientOf(r *http.Request) *config.Client {
	cl, _ := r.Context().Value(clientKey{}).(*config.Client)
	return cl
}

// clientName is the name a request is counted under.
func clientName(cl *config.Client) string {
	if cl == nil {
		return config.Anonymous
	}
	return cl.Name
}

// authenticate finds the client whose key the request carries, as Authorization: Bearer <key> (what every OpenAI SDK
// sends), and returns the request with its client; or it answers 401 and returns false. With no clients in the file
// every request passes, as on days 1 and 2. The key is compared by its SHA-256 (the file holds only hashes), and is
// never logged or quoted.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if len(g.cfg.Clients) == 0 {
		return r, true
	}
	refuse := func(code, reason, message string) (*http.Request, bool) {
		g.log.Warn("client key refused", "reason", reason, "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard"`)
		openai.WriteError(w, openai.Errorf(http.StatusUnauthorized, openai.TypeInvalidRequest, "", code, "%s",
			message))
		return nil, false
	}
	key, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return refuse("missing_api_key", "missing",
			"no key: send your key for this gateway as Authorization: Bearer <key>")
	}
	cl, ok := g.cfg.Client(key)
	if !ok {
		return refuse("invalid_api_key", "unknown", "the key isn't one this gateway knows")
	}
	return r.WithContext(context.WithValue(r.Context(), clientKey{}, cl)), true
}

// bearer returns the token of an Authorization header of the Bearer scheme (its name in any case, as RFC 9110 has
// it).
func bearer(h string) (string, bool) {
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// rateLimit takes one request from the client's bucket and says whether the request may go on. Allowed or not, the
// answer carries OpenAI's x-ratelimit-*-requests headers; a refusal is 429 with Retry-After in seconds and
// retry-after-ms, which the OpenAI SDKs read (and wait for) before retrying on their own. When Redis can't say, the
// request goes on unlimited, or is refused with 503 if the file says on_error: refuse.
func (g *Gateway) rateLimit(w http.ResponseWriter, r *http.Request, x *exchange, cl *config.Client) bool {
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.Redis.Timeout)
	defer cancel()
	d, err := g.limiter.Take(ctx, cl.Name, *cl.RateLimit)
	if err != nil {
		x.rateLimit, x.redisErr = "unavailable", err
		if g.cfg.Redis.RefuseWhenDown {
			g.fail(w, x, openai.Errorf(http.StatusServiceUnavailable, openai.TypeServer, "", "rate_limit_unavailable",
				"the gateway can't check your rate limit right now; retry"), err)
			return false
		}
		return true
	}
	h := w.Header()
	h.Set("X-Ratelimit-Limit-Requests", strconv.Itoa(d.Limit))
	h.Set("X-Ratelimit-Remaining-Requests", strconv.Itoa(d.Remaining))
	h.Set("X-Ratelimit-Reset-Requests", d.Reset.Round(time.Millisecond).String())
	if d.Allowed {
		return true
	}
	x.rateLimit = "refused"
	h.Set("Retry-After", strconv.FormatFloat(math.Ceil(d.RetryAfter.Seconds()), 'f', 0, 64))
	h.Set("Retry-After-Ms", strconv.FormatInt((d.RetryAfter+time.Millisecond-1).Milliseconds(), 10))
	g.fail(w, x, openai.Errorf(http.StatusTooManyRequests, openai.TypeRateLimit, "", "rate_limit_exceeded",
		"rate limit reached for %s (%d requests at once, %g a minute); retry after %s", cl.Name, cl.RateLimit.Burst,
		cl.RateLimit.PerMinute, d.RetryAfter.Round(time.Millisecond)), nil)
	return false
}
