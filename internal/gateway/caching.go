package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/vivekdavara/switchboard/internal/cache"
	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/openai"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// Caching (DESIGN.md, "Caching"), for a model whose configuration asks: an identical earlier request's answer, from
// Redis, before any provider is tried. Only answers are kept (a 200 with a completion, or a stream that ended with
// [DONE]), never an error or a broken stream. A cached answer is no attempt: it costs no budget, tells no breaker
// anything, and counts as a cache hit in the client's totals, with no tokens. It does count against the client's rate
// limit, which comes first. A client can ask for a fresh answer: Cache-Control: no-cache isn't served from the cache
// (its answer is kept), no-store isn't served from it nor kept.

// lookup answers the request from the cache if it can. It returns the key to keep this request's answer under (""
// for none), and whether the request was answered.
func (g *Gateway) lookup(w http.ResponseWriter, r *http.Request, x *exchange, req *openai.ChatRequest,
	model config.Model) (key string, done bool) {
	noCache, noStore := cacheControl(r.Header)
	if noStore {
		x.cache = "bypass"
		w.Header().Set("X-Switchboard-Cache", x.cache)
		return "", false
	}
	scope := clientName(clientOf(r))
	if model.Cache.Shared {
		scope = cache.Shared
	}
	key = cache.Key(scope, model.Name, req.WithModel(model.Name))
	if noCache {
		x.cache = "refresh"
		w.Header().Set("X-Switchboard-Cache", x.cache)
		return key, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.Redis.Timeout)
	defer cancel()
	e, ok, err := g.cache.Get(ctx, key)
	switch {
	case err != nil:
		x.cache, x.cacheErr = "unavailable", err
	case !ok || e.Stream != req.Stream: // a stream's key differs from its plain twin's, so the second can't happen
		x.cache = "miss"
	default:
		x.cache = "hit"
		w.Header().Set("X-Switchboard-Cache", x.cache)
		g.serveHit(w, r, x, e)
		return "", true
	}
	// The client is told "miss" when Redis couldn't say too: it gets a fresh answer either way; the log has why.
	w.Header().Set("X-Switchboard-Cache", "miss")
	return key, false
}

// cacheControl reads the request's Cache-Control directives the gateway honours.
func cacheControl(h http.Header) (noCache, noStore bool) {
	for _, v := range h.Values("Cache-Control") {
		for _, d := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(d)) {
			case "no-cache":
				noCache = true
			case "no-store":
				noStore = true
			}
		}
	}
	return noCache, noStore
}

// serveHit writes a cached answer as it was first written: the completion, or the whole stream at once.
func (g *Gateway) serveHit(w http.ResponseWriter, r *http.Request, x *exchange, e cache.Entry) {
	h := w.Header()
	h.Set("X-Switchboard-Provider", e.Provider)
	h.Set("X-Switchboard-Attempts", "0")
	x.provider, x.upstreamModel = e.Provider, e.UpstreamModel
	if e.Stream {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		x.status, x.streamed, x.events, x.responseBytes = http.StatusOK, true, e.Events, len(e.Body)
		w.Write(e.Body)
	} else {
		g.pass(w, x, http.StatusOK, e.Body, nil)
	}
	g.addTotals(r, x, usage.Entry{CacheHits: 1})
}

// store keeps a completion's answer under key, once the client has it. A 200 whose body is an error object (some
// providers answer so) is no answer, and isn't kept (a suspicion of the day-3 review).
func (g *Gateway) store(r *http.Request, x *exchange, key string, ttl time.Duration, body []byte) {
	if _, isError := streamError(body); isError && !x.streamed {
		return
	}
	e := cache.Entry{Provider: x.provider, UpstreamModel: x.upstreamModel, Stream: x.streamed, Events: x.events,
		Body: body}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), g.cfg.Redis.Timeout)
	defer cancel()
	kept, err := g.cache.Put(ctx, key, e, ttl)
	x.cacheStored, x.cacheErr = kept, err
}

// keeping is the buffer a stream's bytes are kept in for the cache, or nil once they are past its limit.
type keeping struct{ b *bytes.Buffer }

func (k *keeping) add(p []byte) {
	if k.b == nil {
		return
	}
	if k.b.Len()+len(p) > cache.MaxEntryBytes {
		k.b = nil
		return
	}
	k.b.Write(p)
}
