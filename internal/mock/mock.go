// Package mock is an OpenAI-compatible chat completions provider for tests, demos and load runs. It simulates a
// model's latency (time to first token plus time per token) and its failures (error statuses, requests it never
// answers), and it is deterministic given its seed: the n-th request it draws for always meets the same fate.
//
// It has no tokenizer and counts a word as a token.
package mock

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vivekdavara/switchboard/internal/openai"
)

// Config is how a mock provider behaves.
type Config struct {
	Name   string   // the provider's name, in its answers
	Models []string // the models it serves; if empty, any model

	TTFT             time.Duration // time to first token
	PerToken         time.Duration // then this much per token written
	Jitter           float64       // each of the two delays is scaled by a random factor in [1-Jitter, 1+Jitter]
	CompletionTokens int           // tokens an answer has, unless max_completion_tokens or max_tokens is smaller

	ErrorRate   float64 // this fraction of requests is answered at once with ErrorStatus
	ErrorStatus int
	HangRate    float64 // this fraction is never answered, until the client gives up or the mock closes

	APIKey string // if set, every request must carry "Authorization: Bearer <APIKey>"
	Seed   uint64

	Now func() time.Time // the clock for "created"; time.Now if nil
}

// Defaults are the settings of `mockprovider` without flags.
func Defaults() Config {
	return Config{
		Name:             "mock",
		Models:           []string{"mock-small", "mock-large"},
		TTFT:             200 * time.Millisecond,
		PerToken:         5 * time.Millisecond,
		CompletionTokens: 32,
		ErrorStatus:      http.StatusInternalServerError,
		Seed:             1,
	}
}

// MaxRequestBytes is the largest body the mock reads.
const MaxRequestBytes = 8 << 20

// maxChoices is OpenAI's limit on n.
const maxChoices = 128

// Stats counts what a provider did. Requests counts every request to /v1/chat/completions; the others count the
// ones that got that far.
type Stats struct {
	Requests, Refused, Failed, Hung, Answered, Abandoned int64
}

// Provider is a mock provider. It is an http.Handler.
type Provider struct {
	cfg  Config
	mux  *http.ServeMux
	stop chan struct{}
	once sync.Once

	mu  sync.Mutex // guards rng
	rng *rand.Rand

	seq                                                  atomic.Int64 // for completion ids
	requests, refused, failed, hung, answered, abandoned atomic.Int64
}

// New checks cfg and makes a provider.
func New(cfg Config) (*Provider, error) {
	switch {
	case cfg.Name == "":
		return nil, errors.New("mock: a name is required")
	case cfg.TTFT < 0 || cfg.PerToken < 0:
		return nil, errors.New("mock: delays must not be negative")
	case cfg.Jitter < 0 || cfg.Jitter > 1:
		return nil, errors.New("mock: jitter must be between 0 and 1")
	case cfg.CompletionTokens < 1:
		return nil, errors.New("mock: an answer needs at least one token")
	case cfg.ErrorRate < 0 || cfg.ErrorRate > 1 || cfg.HangRate < 0 || cfg.HangRate > 1:
		return nil, errors.New("mock: rates must be between 0 and 1")
	case cfg.ErrorRate+cfg.HangRate > 1:
		return nil, errors.New("mock: the error and hang rates add up to more than 1")
	case cfg.ErrorRate > 0 && (cfg.ErrorStatus < 400 || cfg.ErrorStatus > 599):
		return nil, errors.New("mock: the error status must be a 4xx or 5xx")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &Provider{
		cfg:  cfg,
		mux:  http.NewServeMux(),
		stop: make(chan struct{}),
		rng:  rand.New(rand.NewPCG(cfg.Seed, 0x5157)),
	}
	p.mux.HandleFunc("POST /v1/chat/completions", p.chatCompletions)
	p.mux.HandleFunc("GET /v1/models", p.models)
	p.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return p, nil
}

// ServeHTTP answers a request. Unknown paths and wrong methods get OpenAI-shaped 404s and 405s.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) { openai.Serve(p.mux, w, r) }

// Close drops the connection of every request the provider is holding (hung, or waiting out its latency), at once,
// as a provider that dies would, and of every later request that gets as far as waiting. Close is for shutting
// down; to take a provider down in a test, stop its server. (Its comment used to promise that later requests were
// still served, which they never were; found in the day-1 review.)
func (p *Provider) Close() { p.once.Do(func() { close(p.stop) }) }

// Stats returns the counts so far.
func (p *Provider) Stats() Stats {
	return Stats{
		Requests: p.requests.Load(), Refused: p.refused.Load(), Failed: p.failed.Load(), Hung: p.hung.Load(),
		Answered: p.answered.Load(), Abandoned: p.abandoned.Load(),
	}
}

// fate is what happens to one request, drawn before it is served.
type fate struct {
	fail, hang       bool
	ttftF, perTokenF float64 // jitter factors
}

// draw takes the next request's fate from the RNG. It always takes four numbers, whatever the fate, so the n-th
// request gets the same numbers at any rates: with the same seed, raising the error rate turns more answers into
// failures and raising the hang rate turns more answers into hangs, without reshuffling the rest, and runs at
// different rates compare request for request.
func (p *Provider) draw() fate {
	p.mu.Lock()
	u, f1, f2 := p.rng.Float64(), p.rng.Float64(), p.rng.Float64()
	_ = p.rng.Float64() // reserved, so a later use doesn't shift the sequence
	p.mu.Unlock()
	j := p.cfg.Jitter
	return fate{
		// Failures take the bottom of [0, 1) and hangs the top, so raising either rate only takes answers from the
		// middle (the rates add up to at most 1, so the bands never meet). With hangs right above the failures, a
		// higher error rate moved the hang band and reshuffled hangs and answers (found in the day-1 review).
		fail:      u < p.cfg.ErrorRate,
		hang:      u >= 1-p.cfg.HangRate,
		ttftF:     1 + j*(2*f1-1),
		perTokenF: 1 + j*(2*f2-1),
	}
}

func (p *Provider) chatCompletions(w http.ResponseWriter, r *http.Request) {
	p.requests.Add(1)
	if p.cfg.APIKey != "" {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(p.cfg.APIKey)) != 1 {
			p.refused.Add(1)
			openai.WriteError(w, openai.Errorf(http.StatusUnauthorized, openai.TypeInvalidRequest, "",
				"invalid_api_key", "incorrect API key provided"))
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		p.refused.Add(1)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			openai.WriteError(w, openai.Errorf(http.StatusRequestEntityTooLarge, openai.TypeInvalidRequest, "",
				"request_too_large", "the body is larger than %d bytes", MaxRequestBytes))
			return
		}
		// A malformed body, or a client that left mid-body. Answer either way: a handler that writes nothing gets
		// an empty 200 from net/http (found in the day-1 review).
		openai.WriteError(w, openai.InvalidRequest("", "invalid_body", "the request body couldn't be read: %s",
			err))
		return
	}
	req, e := openai.ParseChatRequest(body)
	if e == nil {
		e = p.check(req)
	}
	if e != nil {
		p.refused.Add(1)
		openai.WriteError(w, e)
		return
	}
	tokens, choices, e := p.lengths(req)
	if e != nil {
		p.refused.Add(1)
		openai.WriteError(w, e)
		return
	}

	f := p.draw()
	switch {
	case f.fail:
		p.failed.Add(1)
		typ := openai.TypeServer
		if p.cfg.ErrorStatus == http.StatusTooManyRequests {
			typ = openai.TypeRateLimit
		}
		openai.WriteError(w, openai.Errorf(p.cfg.ErrorStatus, typ, "", "mock_failure",
			"%s failed this request on purpose (error rate %g)", p.cfg.Name, p.cfg.ErrorRate))
		return
	case f.hang:
		p.hung.Add(1)
		select {
		case <-r.Context().Done():
		case <-p.stop:
		}
		// Returning would send an empty 200 to a client still reading (one that half-closed, or any client after
		// Close). Drop the connection instead, as a provider that dies does.
		panic(http.ErrAbortHandler)
	}

	delay := time.Duration(float64(p.cfg.TTFT)*f.ttftF) +
		time.Duration(float64(p.cfg.PerToken)*f.perTokenF)*time.Duration(tokens)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
		p.abandoned.Add(1)
		panic(http.ErrAbortHandler) // as above: never an empty 200
	case <-p.stop:
		p.abandoned.Add(1)
		panic(http.ErrAbortHandler)
	}

	p.answered.Add(1)
	b, _ := json.Marshal(p.answer(req, tokens, choices))
	openai.WriteJSON(w, http.StatusOK, b)
}

// check refuses what this provider can't serve.
func (p *Provider) check(req *openai.ChatRequest) *openai.Error {
	if len(p.cfg.Models) > 0 && !slices.Contains(p.cfg.Models, req.Model) {
		return openai.Errorf(http.StatusNotFound, openai.TypeInvalidRequest, "model", "model_not_found",
			"the model %q does not exist on %s", req.Model, p.cfg.Name)
	}
	if req.Stream {
		return openai.InvalidRequest("stream", "unsupported_parameter", "%s doesn't stream yet", p.cfg.Name)
	}
	return nil
}

// lengths reads how many tokens each choice gets and how many choices there are.
func (p *Provider) lengths(req *openai.ChatRequest) (tokens, choices int, e *openai.Error) {
	tokens, choices = p.cfg.CompletionTokens, 1
	for _, name := range []string{"max_tokens", "max_completion_tokens"} {
		limit, e := positiveInt(req, name, 1<<30)
		if e != nil {
			return 0, 0, e
		}
		if limit > 0 && limit < tokens {
			tokens = limit
		}
	}
	n, e := positiveInt(req, "n", maxChoices)
	if e != nil {
		return 0, 0, e
	}
	if n > 0 {
		choices = n
	}
	return tokens, choices, nil
}

// positiveInt reads an optional integer member in [1, max]; 0 means it is absent or null.
func positiveInt(req *openai.ChatRequest, name string, max int) (int, *openai.Error) {
	raw, ok := req.Member(name)
	if !ok || string(raw) == "null" {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n > max {
		return 0, openai.InvalidRequest(name, "invalid_value", "%s must be an integer from 1 to %d", name, max)
	}
	return n, nil
}

// answer writes the completion: a sentence naming the provider and model and quoting the start of the last user
// message, cut or padded with filler to exactly tokens words, once per choice.
func (p *Provider) answer(req *openai.ChatRequest, tokens, choices int) openai.ChatCompletion {
	var said []string
	prompt := 0
	for _, m := range req.Messages {
		words := strings.Fields(m.Text)
		prompt += len(words)
		if m.Role == "user" {
			said = words
		}
	}
	if len(said) > 8 {
		said = append(said[:8:8], "...")
	}
	words := strings.Fields(fmt.Sprintf("This is %s answering as %s. You said: %q.", p.cfg.Name, req.Model,
		strings.Join(said, " ")))
	finish := "stop"
	if tokens < p.cfg.CompletionTokens {
		finish = "length"
	}
	for i := 0; len(words) < tokens; i++ {
		words = append(words, filler[i%len(filler)])
	}
	content := strings.Join(words[:tokens], " ")

	c := openai.ChatCompletion{
		ID:      fmt.Sprintf("chatcmpl-%s-%06d", p.cfg.Name, p.seq.Add(1)),
		Object:  "chat.completion",
		Created: p.cfg.Now().Unix(),
		Model:   req.Model,
		Usage:   openai.Usage{PromptTokens: prompt, CompletionTokens: tokens * choices},
	}
	c.Usage.TotalTokens = c.Usage.PromptTokens + c.Usage.CompletionTokens
	for i := range choices {
		c.Choices = append(c.Choices, openai.Choice{Index: i, FinishReason: finish,
			Message: openai.Reply{Role: "assistant", Content: content}})
	}
	return c
}

var filler = strings.Fields("and so the gateway passes the words along one by one until the answer is long enough")

func (p *Provider) models(w http.ResponseWriter, r *http.Request) {
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	for _, m := range p.cfg.Models {
		list.Data = append(list.Data, openai.Model{ID: m, Object: "model", OwnedBy: p.cfg.Name})
	}
	b, _ := json.Marshal(list)
	openai.WriteJSON(w, http.StatusOK, b)
}
