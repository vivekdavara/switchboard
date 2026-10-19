// Package gateway is Switchboard's HTTP handler: it reads a chat completion, routes it to the model's provider,
// forwards it, and maps the answer or the failure to what the client gets. DESIGN.md, "A chat completion, step by
// step" and "Mapping the provider's answer", is the specification; this file follows it in order.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	mathrand "math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vivekdavara/switchboard/internal/breaker"
	"github.com/vivekdavara/switchboard/internal/budget"
	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/openai"
	"github.com/vivekdavara/switchboard/internal/route"
)

// ErrShutdown is the cause the server's base context is cancelled with when the shutdown grace runs out. A request
// cancelled for it gets a 503 rather than being taken for a client that went away.
var ErrShutdown = errors.New("the gateway is shutting down")

// StatusClientClosed is what the log says when the client went away before its answer: nginx's 499.
const StatusClientClosed = 499

// statusTimeoutCDN is Cloudflare's 524, "a timeout occurred": its edge waited too long for the provider's server.
const statusTimeoutCDN = 524

// errProviderTimeout is the cause an attempt's context is cancelled with when the provider makes no progress for its
// timeout.
var errProviderTimeout = errors.New("the provider's timeout passed with no progress")

// Gateway is the handler. Make one with New.
type Gateway struct {
	cfg    *config.Config
	client *http.Client
	log    *slog.Logger
	mux    *http.ServeMux
	ready  atomic.Bool
	newID  func() string

	intn     func(n int) int // the router's randomness: a uniform integer in [0, n)
	budget   *budget.Budget
	breakers map[string]*breaker.Breaker // by provider name
}

// New makes a gateway for cfg, forwarding through transport (NewTransport's if nil) and logging to log. It isn't
// ready (GET /readyz is 503) until SetReady(true).
func New(cfg *config.Config, transport http.RoundTripper, log *slog.Logger) *Gateway {
	if transport == nil {
		transport = NewTransport()
	}
	g := &Gateway{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			// A provider has no business redirecting an API call; a redirect is an answer like any other 3xx (502).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:      log,
		mux:      http.NewServeMux(),
		newID:    newRequestID,
		intn:     mathrand.IntN,
		budget:   budget.New(cfg.RetryBudget, nil),
		breakers: map[string]*breaker.Breaker{},
	}
	for _, p := range cfg.Providers {
		g.breakers[p.Name] = breaker.New(cfg.Breaker, nil)
	}
	g.mux.HandleFunc("POST /v1/chat/completions", g.chat)
	g.mux.HandleFunc("GET /v1/models", g.models)
	g.mux.HandleFunc("GET /v1/models/{model...}", g.model) // "..." because model names can hold slashes
	g.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	g.mux.HandleFunc("GET /readyz", g.readyz)
	return g
}

// NewTransport is the transport New uses by default: Go's default (proxies from the environment, HTTP/2 for https
// providers), keeping up to 256 idle connections per provider instead of 2. DESIGN.md, "Connections to providers".
func NewTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 1024
	t.MaxIdleConnsPerHost = 256
	return t
}

// SetReady says whether GET /readyz answers 200.
func (g *Gateway) SetReady(ready bool) { g.ready.Store(ready) }

// ServeHTTP answers a request whose Host is allowed (config.AllowedHosts). The health endpoints answer any Host: a
// load balancer probes by address (Host: 10.0.0.5:8108), /readyz's 503 during a drain exists for it, and what they
// say is no secret to a rebinding page. A refusal is logged, so an operator sees rebinding attempts (both found in
// the day-1 second review).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	health := r.URL.Path == "/livez" || r.URL.Path == "/readyz"
	if !health && !g.hostAllowed(r.Host) {
		g.log.Warn("host not allowed", "host", r.Host, "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr)
		openai.WriteError(w, openai.Errorf(http.StatusForbidden, openai.TypeInvalidRequest, "", "host_not_allowed",
			"the Host %q is not one this gateway answers to (allowed_hosts)", r.Host))
		return
	}
	openai.Serve(g.mux, w, r)
}

// hostAllowed says whether a request's Host header names an allowed host, with or without a port. No Host at all
// (HTTP/1.0, as health checkers such as HAProxy's send) is allowed: a browser always sends one, so no rebinding page
// can send none. IP addresses compare as addresses, so [0::1] is ::1. (Both found in the day-1 second review.)
func (g *Gateway) hostAllowed(host string) bool {
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	for _, a := range g.cfg.AllowedHosts {
		if a == "*" || a == host {
			return true
		}
	}
	return false
}

func (g *Gateway) readyz(w http.ResponseWriter, r *http.Request) {
	if !g.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready\n"))
}

// models lists every name a client may send: each model's, then its aliases.
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	for _, m := range g.cfg.Models {
		for _, name := range append([]string{m.Name}, m.Aliases...) {
			list.Data = append(list.Data, modelObject(name))
		}
	}
	b, _ := json.Marshal(list)
	openai.WriteJSON(w, http.StatusOK, b)
}

// model answers GET /v1/models/{model}, which SDKs call to check that a model exists.
func (g *Gateway) model(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("model")
	if _, ok := g.cfg.Route(name); !ok {
		openai.WriteError(w, openai.Errorf(http.StatusNotFound, openai.TypeInvalidRequest, "model",
			"model_not_found", "the model %q is not served here", name))
		return
	}
	b, _ := json.Marshal(modelObject(name))
	openai.WriteJSON(w, http.StatusOK, b)
}

// modelObject is a model as GET /v1/models shows it. The gateway owns the name: which provider serves a request is
// decided per request, by weight and by fallback.
func modelObject(name string) openai.Model {
	return openai.Model{ID: name, Object: "model", OwnedBy: "switchboard"}
}

// exchange is one chat completion as the log line reports it.
type exchange struct {
	id              string
	began           time.Time
	status          int // what the client got, or StatusClientClosed
	model, provider string
	upstreamModel   string
	upstreamStatus  int
	upstreamTook    time.Duration
	requestBytes    int
	responseBytes   int
	code            string       // the error's code, if the gateway answered with one
	err             error        // the cause behind it, for the log only
	upstreamSent    bool         // the request went upstream, so the upstream fields mean something
	streamed        bool         // the answer was streamed: status 200 went out with the first event
	events          int          // data events streamed to the client
	attempts        []attemptLog // the attempts made, in order
	tried           []attemptLog // the attempts and the targets skipped, in order
	budgetSpent     bool         // a retry was refused by the retry budget
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	x := &exchange{id: g.newID(), began: time.Now()}
	defer g.logExchange(x)
	w.Header().Set("X-Request-Id", x.id)

	// 1. Content type: only application/json, so a browser can't post here cross-origin without asking first.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		g.fail(w, x, openai.Errorf(http.StatusUnsupportedMediaType, openai.TypeInvalidRequest, "",
			"unsupported_media_type", "send the body as Content-Type: application/json"), nil)
		return
	}

	// 2. Size. A read blocked on a slow body doesn't watch the request's context, so a stop would otherwise cut it
	// off with the connection, unanswered; on the shutdown cause, a read deadline in the past wakes it, and it gets
	// its 503 like any other request still running (a suspicion of the day-1 review).
	// The mutex keeps the callback from touching the connection once the read is over: stopWaking doesn't wait for a
	// callback already running, and a ResponseController must not be used after ServeHTTP returns (harmless over
	// HTTP/1, a use of a recycled stream over HTTP/2; the day-1 third review).
	var mu sync.Mutex
	reading := true
	stopWaking := context.AfterFunc(r.Context(), func() {
		mu.Lock()
		defer mu.Unlock()
		if reading && context.Cause(r.Context()) == ErrShutdown {
			http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	})
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBytes))
	mu.Lock()
	reading = false
	mu.Unlock()
	stopWaking()
	x.requestBytes = len(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		var netErr net.Error
		switch {
		case context.Cause(r.Context()) == ErrShutdown:
			g.fail(w, x, openai.Errorf(http.StatusServiceUnavailable, openai.TypeServer, "", "shutting_down",
				"the gateway is shutting down; retry"), err)
		case errors.As(err, &tooBig):
			g.fail(w, x, openai.Errorf(http.StatusRequestEntityTooLarge, openai.TypeInvalidRequest, "",
				"request_too_large", "the body is larger than %d bytes", g.cfg.MaxRequestBytes), nil)
		case errors.As(err, &netErr) && netErr.Timeout():
			g.fail(w, x, openai.Errorf(http.StatusRequestTimeout, openai.TypeInvalidRequest, "",
				"request_timeout", "the body took too long to arrive"), err)
		default:
			// A malformed body (a bad chunk length, a body shorter than its Content-Length) or a client that left
			// mid-body; the two look alike from here. Answer either way: returning without writing would make
			// net/http send an empty 200 to a client still listening (found in the day-1 review).
			g.fail(w, x, openai.Errorf(http.StatusBadRequest, openai.TypeInvalidRequest, "", "invalid_body",
				"the request body couldn't be read: %s", err), err)
		}
		return
	}

	// 3 and 4. One meaning; the fields the gateway acts on.
	req, e := openai.ParseChatRequest(body)
	if e != nil {
		g.fail(w, x, e, nil)
		return
	}
	x.model = req.Model

	// 5. Route.
	model, ok := g.cfg.Route(req.Model)
	if !ok {
		g.fail(w, x, openai.Errorf(http.StatusNotFound, openai.TypeInvalidRequest, "model", "model_not_found",
			"the model %q is not served here; GET /v1/models lists the ones that are", req.Model), nil)
		return
	}

	// 6 to 8, once per target until one answers: rewrite, forward, answer. DESIGN.md, "Fallback".
	var last *result
	var probeAt time.Time       // the earliest an open breaker skipped here lets a probe through
	unsure := map[string]bool{} // providers that may still be working on an earlier attempt
	for _, t := range route.Order(model.Targets, g.intn) {
		if len(x.attempts) == model.MaxAttempts {
			break
		}
		if len(x.attempts) > 0 && r.Context().Err() != nil {
			// The client left, or the gateway began to stop, after the last attempt: another would never leave the
			// gateway, but would still spend the retry budget and maybe a breaker's probe (a suspicion of the day-2
			// review).
			res := g.unreached(r, r.Context(), t.Provider, context.Cause(r.Context()), false)
			if res.gone {
				x.status, x.err = StatusClientClosed, res.cause
				panic(http.ErrAbortHandler)
			}
			last = &res
			break
		}
		if unsure[t.Provider.Name] {
			x.skip(t, "unanswered_before")
			continue
		}
		ticket, ok, at := g.breakers[t.Provider.Name].Allow()
		if !ok {
			x.skip(t, "circuit_open")
			if probeAt.IsZero() || at.Before(probeAt) {
				probeAt = at
			}
			continue
		}
		if len(x.attempts) == 0 {
			g.budget.Request() // only a request that makes an attempt adds load the budget is a share of
		} else if !g.budget.Retry() {
			ticket.Release()
			x.budgetSpent = true
			break
		}
		res := g.try(w, r, x, t, req.WithModel(t.UpstreamModel), req.Stream, len(x.attempts)+1, ticket)
		switch res.health {
		case healthy:
			ticket.Success()
		case unhealthy:
			ticket.Failure()
			if res.written {
				// A stream reports its first event as a success, at the commit (relay), so its ticket has reported
				// already; its break after that is a failure seen now.
				g.breakers[t.Provider.Name].Fail()
			}
		default:
			ticket.Release()
		}
		if res.gone {
			// The client went away, or half-closed its side, which net/http also reports as gone. Drop the
			// connection: returning without writing would make net/http send an empty 200 to a client that may still
			// be reading (found in the day-1 review). The log line still runs, deferred.
			x.status, x.err = StatusClientClosed, res.cause
			panic(http.ErrAbortHandler)
		}
		if res.written { // a stream: it has ended, cleanly or with its error event
			if res.err != nil {
				panic(http.ErrAbortHandler) // so no client can take the stream for a complete one
			}
			return
		}
		last = &res
		if res.kind == answered {
			break
		}
		if res.kind == unanswered {
			unsure[t.Provider.Name] = true
		}
	}

	if last == nil { // every target was skipped: their breakers are open
		// At least a second: a probe may be out (its breaker can say only "now"), and Retry-After counts in seconds.
		secs := int(math.Ceil(max(time.Until(probeAt).Seconds(), 1)))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.Header().Set("X-Switchboard-Attempts", "0")
		g.fail(w, x, upstreamError(http.StatusServiceUnavailable, "circuit_open",
			"every provider of %q is failing and resting (circuit open); retry after %ds", req.Model, secs), nil)
		return
	}
	w.Header().Set("X-Switchboard-Provider", x.provider)
	w.Header().Set("X-Switchboard-Attempts", strconv.Itoa(len(x.attempts)))
	if last.err != nil {
		for k, v := range last.header {
			w.Header()[k] = v
		}
		g.fail(w, x, last.err, last.cause)
		return
	}
	g.pass(w, x, last.status, last.body, last.header)
}

// What an attempt's answer means for the request.
type outcome int

const (
	// answered: the answer goes to the client, whatever it is (a completion, or an error about the request).
	answered outcome = iota
	// failed: the provider didn't do the work (it answered with an error about itself, or never got the request),
	// so any other target may be tried.
	failed
	// unanswered: the provider got the request and gave no usable answer (it timed out, broke off, or answered
	// garbage), so it may have done the work and may bill for it. Other providers may be tried; this one isn't
	// sent the request again.
	unanswered
)

func (o outcome) String() string { return [...]string{"answered", "failed", "unanswered"}[o] }

// What an attempt says about the provider, for its breaker.
type health int

const (
	neutral health = iota // nothing (the client left; the provider doesn't know the upstream model)
	healthy
	unhealthy
)

// result is one attempt, and what the client gets if no other target is tried after it.
type result struct {
	kind    outcome
	health  health
	gone    bool // the client went away: drop the connection
	written bool // the answer is written already (a stream that started); with err, it ended with that error

	status int         // passed through: the provider's status and bytes,
	body   []byte      //
	header http.Header // and the headers kept with them (Retry-After);
	err    *openai.Error
	cause  error // or the gateway's own error, and its cause for the log
}

// attemptLog is one attempt or one skipped target, as the log line lists it.
type attemptLog struct {
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstream_model"`
	Status        int     `json:"status,omitempty"` // the provider's, if it answered
	Ms            float64 `json:"ms,omitempty"`
	Outcome       string  `json:"outcome,omitempty"`
	Code          string  `json:"code,omitempty"`    // the gateway's error, if the attempt ended in one
	Skipped       string  `json:"skipped,omitempty"` // why the target wasn't tried
}

func (x *exchange) skip(t config.Target, why string) {
	x.tried = append(x.tried, attemptLog{Provider: t.Provider.Name, UpstreamModel: t.UpstreamModel, Skipped: why})
}

// try sends body to the target's provider and says what came of it. It writes nothing to the client, unless a
// stream starts: then it writes the whole stream (relay).
func (g *Gateway) try(w http.ResponseWriter, r *http.Request, x *exchange, t config.Target, body []byte, stream bool,
	attempt int, ticket *breaker.Ticket) (res result) {
	p := t.Provider
	x.provider, x.upstreamModel = p.Name, t.UpstreamModel
	x.upstreamSent, x.upstreamStatus = true, 0
	entry := attemptLog{Provider: p.Name, UpstreamModel: t.UpstreamModel}
	sent := time.Now()
	defer func() {
		x.upstreamTook = time.Since(sent)
		entry.Status, entry.Ms, entry.Outcome = x.upstreamStatus, ms(x.upstreamTook), res.kind.String()
		if res.err != nil {
			entry.Code = res.err.Code
		}
		if res.gone {
			entry.Outcome, entry.Code = "", "client_closed"
		}
		x.attempts = append(x.attempts, entry)
		x.tried = append(x.tried, entry)
	}()

	// The provider's timeout is the longest the gateway waits for its next sign of progress: the whole answer, or
	// a stream's next event.
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	timer := time.AfterFunc(p.Timeout, func() { cancel(errProviderTimeout) })
	defer timer.Stop()
	// Whether the provider got the whole request decides whether a failure may have cost it work: one that never
	// did can't have started (DESIGN.md, "Fallback").
	var delivered atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				delivered.Store(true)
			}
		},
	})
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions",
		bytes.NewReader(body))
	if err != nil { // the base URL was checked at load, so this can't happen
		return result{kind: answered, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider's URL is invalid"), cause: err}
	}
	// The allowlist: nothing of the client's headers goes upstream.
	up.Header = http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
		"X-Request-Id": {x.id},
	}
	if stream {
		up.Header.Set("Accept", "text/event-stream")
	}
	if p.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := g.client.Do(up)
	if err != nil {
		return g.unreached(r, ctx, p, err, delivered.Load())
	}
	x.upstreamStatus = resp.StatusCode
	if stream && resp.StatusCode == http.StatusOK && eventStream(resp.Header.Get("Content-Type")) {
		return g.relay(w, r, x, ctx, p, attempt, timer, ticket, resp)
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, g.cfg.MaxResponseBytes+1))
	resp.Body.Close()
	if err != nil {
		return g.unreached(r, ctx, p, err, true)
	}
	if int64(len(answer)) > g.cfg.MaxResponseBytes {
		// The provider did the work; another would most likely write as much.
		return result{kind: answered, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered with more than %d bytes", p.Name, g.cfg.MaxResponseBytes)}
	}

	// The mapping table: an answer about the request passes through; anything else is the gateway's error, and
	// a reason to try another target.
	switch status := resp.StatusCode; {
	case status == http.StatusOK && stream:
		// Asked to stream, it answered in one piece (or with something else): it did the work, wrongly.
		return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_error", "the provider %s answered a streaming request without an event stream", p.Name)}
	case status == http.StatusOK && jsonObject(answer):
		return result{kind: answered, health: healthy, status: status, body: answer}
	case status == http.StatusOK:
		return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_error", "the provider %s answered 200 without a JSON object", p.Name)}
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity || status == http.StatusTooManyRequests:
		res = result{kind: answered, health: healthy, status: status, body: answer}
		if status == http.StatusTooManyRequests {
			// The provider is rate-limiting the gateway's key: another target may have room.
			res.kind, res.health = failed, unhealthy
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				res.header = http.Header{"Retry-After": {ra}}
			}
		}
		if !jsonObject(answer) {
			typ := openai.TypeInvalidRequest
			if status == http.StatusTooManyRequests {
				typ = openai.TypeRateLimit
			}
			res.body, res.err = nil, openai.Errorf(status, typ, "", "upstream_error",
				"the provider %s answered %d without a JSON error", p.Name, status)
		}
		return res
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return result{kind: failed, health: unhealthy, err: upstreamError(http.StatusBadGateway, "upstream_auth",
			"the provider %s refused the gateway's credentials (%d)", p.Name, status)}
	case status == http.StatusBadGateway || status == http.StatusGatewayTimeout || status == statusTimeoutCDN:
		// The provider's own front end lost its back end: a broken answer from it, or a timeout waiting on it (524
		// is Cloudflare's). That back end may have done the work, so this is as unanswered as a timeout of ours
		// (a suspicion of the day-2 review: as "failed", the same provider could be sent the request again).
		return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_error", "the provider %s answered %d", p.Name, status)}
	case status == http.StatusNotFound:
		// About this target (the provider doesn't know its upstream model, or the base URL is wrong), not about
		// the provider's health: its other models may be fine, so its breaker isn't told.
		return result{kind: failed, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered %d", p.Name, status)}
	default:
		return result{kind: failed, health: unhealthy, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered %d", p.Name, status)}
	}
}

// unreached is the result of an attempt that got no complete answer: the client went away, the gateway is shutting
// down, the provider took longer than its timeout, or it couldn't be reached or broke off. delivered says whether the
// provider got the whole request.
func (g *Gateway) unreached(r *http.Request, ctx context.Context, p *config.Provider, err error,
	delivered bool) result {
	kind := failed
	if delivered {
		kind = unanswered
	}
	switch {
	case r.Context().Err() != nil && context.Cause(r.Context()) == ErrShutdown:
		// Final: every attempt after this one would be cancelled too.
		return result{kind: answered, err: openai.Errorf(http.StatusServiceUnavailable,
			openai.TypeServer, "", "shutting_down", "the gateway is shutting down; retry"), cause: err}
	case r.Context().Err() != nil:
		return result{gone: true, cause: err}
	case context.Cause(ctx) == errProviderTimeout:
		return result{kind: kind, health: unhealthy, err: upstreamError(http.StatusGatewayTimeout,
			"upstream_timeout", "the provider %s made no progress for %s (its timeout)", p.Name, p.Timeout),
			cause: err}
	default:
		return result{kind: kind, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_unavailable", "the provider %s couldn't be reached or broke off", p.Name), cause: err}
	}
}

func upstreamError(status int, code, format string, args ...any) *openai.Error {
	return openai.Errorf(status, openai.TypeServer, "", code, format, args...)
}

// pass writes the provider's answer as it came.
func (g *Gateway) pass(w http.ResponseWriter, x *exchange, status int, body []byte, keep http.Header) {
	for k, v := range keep {
		w.Header()[k] = v
	}
	x.status, x.responseBytes = status, len(body)
	openai.WriteJSON(w, status, body)
}

// fail writes the gateway's own error. cause, if not nil, goes to the log only.
func (g *Gateway) fail(w http.ResponseWriter, x *exchange, e *openai.Error, cause error) {
	x.status, x.code, x.err = e.Status, e.Code, cause
	b, _ := json.Marshal(e)
	x.responseBytes = len(b)
	openai.WriteJSON(w, e.Status, b)
}

func (g *Gateway) logExchange(x *exchange) {
	attrs := []slog.Attr{
		slog.String("id", x.id),
		slog.Int("status", x.status),
		slog.String("model", x.model),
		slog.Float64("ms", ms(time.Since(x.began))),
		slog.Int("request_bytes", x.requestBytes),
		slog.Int("response_bytes", x.responseBytes),
	}
	if x.provider != "" {
		attrs = append(attrs, slog.String("provider", x.provider), slog.String("upstream_model", x.upstreamModel))
	}
	if x.upstreamSent {
		attrs = append(attrs, slog.Int("upstream_status", x.upstreamStatus),
			slog.Float64("upstream_ms", ms(x.upstreamTook)))
	}
	if len(x.tried) > 0 {
		attrs = append(attrs, slog.Int("attempts", len(x.attempts)))
	}
	if len(x.tried) > 1 || len(x.attempts) != len(x.tried) { // more than the fields above say
		attrs = append(attrs, slog.Any("tried", x.tried))
	}
	if x.streamed {
		attrs = append(attrs, slog.Bool("streamed", true), slog.Int("events", x.events))
	}
	if x.budgetSpent {
		attrs = append(attrs, slog.Bool("retry_budget_spent", true))
	}
	if x.code != "" {
		attrs = append(attrs, slog.String("code", x.code))
	}
	if x.err != nil {
		attrs = append(attrs, slog.String("err", x.err.Error()))
	}
	level := slog.LevelInfo
	if x.status >= 500 || (x.streamed && x.code != "") { // a stream that broke went out as a 200
		level = slog.LevelWarn
	}
	g.log.LogAttrs(context.Background(), level, "chat completion", attrs...)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// jsonObject says whether b is one valid JSON object, whitespace around it allowed.
func jsonObject(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && b[0] == '{' && json.Valid(b)
}

// newRequestID returns 16 random hex digits.
func newRequestID() string {
	var b [8]byte
	rand.Read(b[:]) // never fails (crypto/rand panics rather than return an error since Go 1.24)
	return hex.EncodeToString(b[:])
}
