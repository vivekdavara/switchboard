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
	"mime"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/openai"
)

// ErrShutdown is the cause the server's base context is cancelled with when the shutdown grace runs out. A request
// cancelled for it gets a 503 rather than being taken for a client that went away.
var ErrShutdown = errors.New("the gateway is shutting down")

// StatusClientClosed is what the log says when the client went away before its answer: nginx's 499.
const StatusClientClosed = 499

// Gateway is the handler. Make one with New.
type Gateway struct {
	cfg    *config.Config
	client *http.Client
	log    *slog.Logger
	mux    *http.ServeMux
	ready  atomic.Bool
	newID  func() string
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
		log:   log,
		mux:   http.NewServeMux(),
		newID: newRequestID,
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

// ServeHTTP answers a request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { openai.Serve(g.mux, w, r) }

func (g *Gateway) readyz(w http.ResponseWriter, r *http.Request) {
	if !g.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready\n"))
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	for _, m := range g.cfg.Models {
		list.Data = append(list.Data, modelObject(m))
	}
	b, _ := json.Marshal(list)
	openai.WriteJSON(w, http.StatusOK, b)
}

// model answers GET /v1/models/{model}, which SDKs call to check that a model exists.
func (g *Gateway) model(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("model")
	m, ok := g.cfg.Route(name)
	if !ok {
		openai.WriteError(w, openai.Errorf(http.StatusNotFound, openai.TypeInvalidRequest, "model",
			"model_not_found", "the model %q is not served here", name))
		return
	}
	b, _ := json.Marshal(modelObject(m))
	openai.WriteJSON(w, http.StatusOK, b)
}

func modelObject(m config.Model) openai.Model {
	return openai.Model{ID: m.Name, Object: "model", OwnedBy: m.Provider.Name}
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
	code            string // the error's code, if the gateway answered with one
	err             error  // the cause behind it, for the log only
	upstreamSent    bool   // the request went upstream, so the upstream fields mean something
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

	// 2. Size.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBytes))
	x.requestBytes = len(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		var netErr net.Error
		switch {
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
	if req.Stream {
		g.fail(w, x, openai.InvalidRequest("stream", "unsupported_parameter",
			"stream: true isn't supported yet; it arrives with streaming passthrough on day 2"), nil)
		return
	}

	// 5. Route.
	route, ok := g.cfg.Route(req.Model)
	if !ok {
		g.fail(w, x, openai.Errorf(http.StatusNotFound, openai.TypeInvalidRequest, "model", "model_not_found",
			"the model %q is not served here; GET /v1/models lists the ones that are", req.Model), nil)
		return
	}
	x.provider, x.upstreamModel = route.Provider.Name, route.UpstreamModel
	w.Header().Set("X-Switchboard-Provider", route.Provider.Name)

	// 6 to 8. Rewrite, forward, answer.
	g.forward(w, r, x, route, req.WithModel(route.UpstreamModel))
}

// forward sends the rewritten body to the route's provider and writes what the client gets.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, x *exchange, route config.Model, body []byte) {
	p := route.Provider
	ctx, cancel := context.WithTimeout(r.Context(), p.Timeout)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions",
		bytes.NewReader(body))
	if err != nil { // the base URL was checked at load, so this can't happen
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_error", "the provider's URL is invalid"), err)
		return
	}
	// The allowlist: nothing of the client's headers goes upstream.
	up.Header = http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
		"X-Request-Id": {x.id},
	}
	if p.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	sent := time.Now()
	x.upstreamSent = true
	resp, err := g.client.Do(up)
	if err != nil {
		x.upstreamTook = time.Since(sent)
		g.failUpstream(w, r, ctx, x, p, err)
		return
	}
	x.upstreamStatus = resp.StatusCode
	answer, err := io.ReadAll(io.LimitReader(resp.Body, g.cfg.MaxResponseBytes+1))
	resp.Body.Close()
	x.upstreamTook = time.Since(sent)
	if err != nil {
		g.failUpstream(w, r, ctx, x, p, err)
		return
	}
	if int64(len(answer)) > g.cfg.MaxResponseBytes {
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered with more than %d bytes", p.Name, g.cfg.MaxResponseBytes), nil)
		return
	}

	// The mapping table: an answer about the request passes through; anything else is the gateway's error.
	switch status := resp.StatusCode; {
	case status == http.StatusOK && jsonObject(answer):
		g.pass(w, x, status, answer, nil)
	case status == http.StatusOK:
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered 200 without a JSON object", p.Name), nil)
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity || status == http.StatusTooManyRequests:
		var keep http.Header
		if status == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "" {
			keep = http.Header{"Retry-After": {resp.Header.Get("Retry-After")}}
		}
		if !jsonObject(answer) {
			typ := openai.TypeInvalidRequest
			if status == http.StatusTooManyRequests {
				typ = openai.TypeRateLimit
			}
			for k, v := range keep {
				w.Header()[k] = v
			}
			g.fail(w, x, openai.Errorf(status, typ, "", "upstream_error",
				"the provider %s answered %d without a JSON error", p.Name, status), nil)
			return
		}
		g.pass(w, x, status, answer, keep)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_auth",
			"the provider %s refused the gateway's credentials (%d)", p.Name, status), nil)
	default:
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_error", "the provider %s answered %d",
			p.Name, status), nil)
	}
}

// failUpstream answers a request whose upstream exchange failed with err: the client went away, the gateway is
// shutting down, the provider took longer than its timeout, or it couldn't be reached or broke off.
func (g *Gateway) failUpstream(w http.ResponseWriter, r *http.Request, ctx context.Context, x *exchange,
	p *config.Provider, err error) {
	switch {
	case r.Context().Err() != nil && context.Cause(r.Context()) == ErrShutdown:
		g.fail(w, x, openai.Errorf(http.StatusServiceUnavailable, openai.TypeServer, "", "shutting_down",
			"the gateway is shutting down; retry"), err)
	case r.Context().Err() != nil:
		// The client went away, or half-closed its side, which net/http also reports as gone. Drop the connection:
		// returning without writing would make net/http send an empty 200 to a client that may still be reading
		// (found in the day-1 review). The log line still runs, deferred.
		x.status, x.err = StatusClientClosed, err
		panic(http.ErrAbortHandler)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		g.fail(w, x, upstreamError(http.StatusGatewayTimeout, "upstream_timeout",
			"the provider %s didn't answer within %s", p.Name, p.Timeout), err)
	default:
		g.fail(w, x, upstreamError(http.StatusBadGateway, "upstream_unavailable",
			"the provider %s couldn't be reached or broke off", p.Name), err)
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
	if x.code != "" {
		attrs = append(attrs, slog.String("code", x.code))
	}
	if x.err != nil {
		attrs = append(attrs, slog.String("err", x.err.Error()))
	}
	level := slog.LevelInfo
	if x.status >= 500 {
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
