package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/mock"
	"github.com/vivekdavara/switchboard/internal/openai"
)

// syncBuffer is a log the server's goroutines and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// seen is a request as a provider received it.
type seen struct {
	path   string
	header http.Header
	body   []byte
}

// provider is a test upstream that records every request before handing it to its handler.
type provider struct {
	*httptest.Server
	mu  sync.Mutex
	got []seen
}

func newProvider(t *testing.T, h http.Handler) *provider {
	t.Helper()
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.got = append(p.got, seen{r.URL.Path, r.Header.Clone(), body})
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *provider) requests() []seen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seen(nil), p.got...)
}

// gatewayFor serves a gateway whose providers are the given test servers, named p0, p1... Each model is
// "name=provider/upstream". extra is spliced into the top level of the config.
func gatewayFor(t *testing.T, extra string, providers []string, models ...string) (*httptest.Server, *Gateway,
	*syncBuffer) {
	t.Helper()
	var ps, ms []string
	for i, p := range providers {
		ps = append(ps, fmt.Sprintf(`{"name":"p%d",%s}`, i, p))
	}
	for _, m := range models {
		name, rest, _ := strings.Cut(m, "=")
		prov, up, _ := strings.Cut(rest, "/")
		ms = append(ms, fmt.Sprintf(`{"name":%q,"provider":%q,"upstream_model":%q}`, name, prov, up))
	}
	if extra != "" {
		extra += ","
	}
	cfg, err := config.Parse([]byte(`{`+extra+`"providers":[`+strings.Join(ps, ",")+`],"models":[`+
		strings.Join(ms, ",")+`]}`), func(k string) string {
		if k == "P_KEY" {
			return "sk-provider-key-for-tests"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	g := New(cfg, nil, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv, g, logs
}

func at(p *provider, rest string) string {
	s := fmt.Sprintf(`"base_url":%q`, p.URL+"/v1")
	if rest != "" {
		s += "," + rest
	}
	return s
}

const ask = `{"model":"fast","messages":[{"role":"user","content":"What is two plus two?"}]}`

func send(t *testing.T, srv *httptest.Server, body string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// errorOf reads an OpenAI-shaped error, failing the test if the body isn't one.
func errorOf(t *testing.T, b []byte) (typ, code, param, message string) {
	t.Helper()
	var e struct {
		Error struct {
			Message, Type string
			Code, Param   *string
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&e); err != nil || e.Error.Message == "" || e.Error.Type == "" {
		t.Fatalf("not an OpenAI error: %s", b)
	}
	if e.Error.Code != nil {
		code = *e.Error.Code
	}
	if e.Error.Param != nil {
		param = *e.Error.Param
	}
	return e.Error.Type, code, param, e.Error.Message
}

// errorOfOrEmpty is errorOf for bodies that may not be errors at all.
func errorOfOrEmpty(b []byte) (typ, code, param, message string) {
	var e struct {
		Error struct {
			Message, Type string
			Code, Param   *string
		}
	}
	if json.Unmarshal(b, &e) != nil {
		return "", "", "", ""
	}
	if e.Error.Code != nil {
		code = *e.Error.Code
	}
	if e.Error.Param != nil {
		param = *e.Error.Param
	}
	return e.Error.Type, code, param, e.Error.Message
}

func mockProvider(t *testing.T, name string) *mock.Provider {
	t.Helper()
	cfg := mock.Defaults()
	cfg.Name, cfg.TTFT, cfg.PerToken = name, 0, 0
	m, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

var requestID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestARequestIsForwardedWithOnlyTheModelRewrittenAndAnsweredByteForByte(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, "", []string{at(up, `"api_key_env":"P_KEY"`)}, "fast=p0/mock-small")
	body := `{"temperature": 0.25, "model":"fast","messages":[ {"role":"user","content":"What is two plus two?"} ],` +
		`"tools":[{"type":"function","function":{"name":"add","parameters":{"type":"object"}}}],"user":"u-1"}`
	resp, answer := send(t, srv, body, "Authorization", "Bearer client-key", "Cookie", "session=abc",
		"OpenAI-Organization", "org-1", "X-Custom", "1", "Idempotency-Key", "k-1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, answer)
	}
	got := up.requests()
	if len(got) != 1 {
		t.Fatalf("the provider got %d requests", len(got))
	}
	r := got[0]
	wantBody := `{"temperature":0.25,"model":"mock-small","messages":[ {"role":"user","content":"What is two plus two?"} ],` +
		`"tools":[{"type":"function","function":{"name":"add","parameters":{"type":"object"}}}],"user":"u-1"}`
	if r.path != "/v1/chat/completions" || string(r.body) != wantBody {
		t.Errorf("upstream got %s %s\nwant %s", r.path, r.body, wantBody)
	}
	// Exactly the allowlist went upstream, plus what Go's client adds itself.
	for k := range r.header {
		switch k {
		case "Content-Type", "Accept", "X-Request-Id", "Authorization", "Content-Length", "User-Agent",
			"Accept-Encoding":
		default:
			t.Errorf("the client's header %s went upstream", k)
		}
	}
	if r.header.Get("Authorization") != "Bearer sk-provider-key-for-tests" ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("Accept") != "application/json" {
		t.Errorf("upstream headers %v", r.header)
	}
	id := resp.Header.Get("X-Request-Id")
	if !requestID.MatchString(id) || r.header.Get("X-Request-Id") != id {
		t.Errorf("request id %q, upstream saw %q", id, r.header.Get("X-Request-Id"))
	}
	if resp.Header.Get("X-Switchboard-Provider") != "p0" || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("response headers %v", resp.Header)
	}
	var c openai.ChatCompletion
	if err := json.Unmarshal(answer, &c); err != nil || c.Model != "mock-small" ||
		!strings.Contains(c.Choices[0].Message.Content, "What is two plus two?") {
		t.Errorf("answer %s", answer)
	}
	// The same request again: the mock's answer differs only in its id. (This compares two answers through the
	// gateway, so it shows the forward is repeatable, not that it is byte for byte; the mapping table's "an answer"
	// row checks that the client gets exactly the provider's bytes. The day-1 review caught the old comment.)
	resp2, again := send(t, srv, body)
	if resp2.StatusCode != 200 || !bytes.Equal(bytes.Replace(again, []byte("000002"), []byte("000001"), 1), answer) {
		t.Errorf("answers differ beyond the id:\n%s\n%s", answer, again)
	}
}

func TestNoKeyIsSentToAProviderWithout(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
	if resp, b := send(t, srv, ask, "Authorization", "Bearer client-key"); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if auth, ok := up.requests()[0].header["Authorization"]; ok {
		t.Errorf("upstream got Authorization %q", auth)
	}
}

func TestRequestsTheGatewayRefusesNeverReachAProvider(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, `"max_request_bytes":2000`, []string{at(up, "")}, "fast=p0/mock-small")
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
		code, param             string
	}{
		{"no content type", "", ask, 415, "unsupported_media_type", ""},
		{"a form", "application/x-www-form-urlencoded", ask, 415, "unsupported_media_type", ""},
		{"text", "text/plain", ask, 415, "unsupported_media_type", ""},
		{"a bad media type", "application/json; charset", ask, 415, "unsupported_media_type", ""},
		{"too big", "application/json", `{"model":"fast","pad":"` + strings.Repeat("x", 2000) + `"}`, 413,
			"request_too_large", ""},
		{"not JSON", "application/json", `{"model":`, 400, "invalid_json", ""},
		{"a model twice", "application/json", `{"model":"fast","model":"other","messages":[]}`, 400,
			"invalid_json", ""},
		{"a model in another case", "application/json",
			`{"model":"fast","MODEL":"other","messages":[{"role":"user","content":"x"}]}`, 400,
			"invalid_parameter", "MODEL"},
		{"no messages", "application/json", `{"model":"fast"}`, 400, "missing_required_parameter", "messages"},
		{"streaming", "application/json",
			`{"model":"fast","messages":[{"role":"user","content":"x"}],"stream":true}`, 400,
			"unsupported_parameter", "stream"},
		{"an unknown model", "application/json", `{"model":"gpt-9","messages":[{"role":"user","content":"x"}]}`,
			404, "model_not_found", "model"},
		{"a model by its upstream name", "application/json",
			`{"model":"mock-large","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found", "model"},
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(tc.body))
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d: %s", tc.name, resp.StatusCode, tc.status, b)
			continue
		}
		if _, code, param, _ := errorOf(t, b); code != tc.code || param != tc.param {
			t.Errorf("%s: code %q param %q, want %q %q", tc.name, code, param, tc.code, tc.param)
		}
		if !requestID.MatchString(resp.Header.Get("X-Request-Id")) {
			t.Errorf("%s: no request id", tc.name)
		}
	}
	// application/json with parameters is fine.
	if resp, b := send(t, srv, ask, "Content-Type", "application/json; charset=utf-8"); resp.StatusCode != 200 {
		t.Errorf("with a charset: %d %s", resp.StatusCode, b)
	}
	if n := len(up.requests()); n != 1 {
		t.Errorf("the provider got %d requests, want only the last one", n)
	}
	if n := strings.Count(logs.String(), `"msg":"chat completion"`); n != 13 {
		t.Errorf("%d log lines, want 13:\n%s", n, logs)
	}
}

func TestOtherPathsAndMethodsAreOpenAIErrors(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small", "smart=p0/mock-large",
		"org/model-1=p0/mock-small")
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/v1/chat/completions", 405, "POST"},
		{"PUT", "/v1/models", 405, "GET, HEAD"},
		{"POST", "/v1/completions", 404, ""},
		{"POST", "/v1/embeddings", 404, ""},
		{"GET", "/v2/models", 404, ""},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || resp.Header.Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d Allow %q", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Allow"))
		}
		errorOf(t, b)
	}

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var list openai.ModelList
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	want := openai.ModelList{Object: "list", Data: []openai.Model{
		{ID: "fast", Object: "model", OwnedBy: "p0"}, {ID: "smart", Object: "model", OwnedBy: "p0"},
		{ID: "org/model-1", Object: "model", OwnedBy: "p0"}}}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Errorf("models %+v", list)
	}

	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/v1/models/fast", 200, `{"id":"fast","object":"model","created":0,"owned_by":"p0"}`},
		{"/v1/models/org/model-1", 200, `{"id":"org/model-1","object":"model","created":0,"owned_by":"p0"}`},
		{"/v1/models/org%2Fmodel-1", 200, `{"id":"org/model-1","object":"model","created":0,"owned_by":"p0"}`},
		{"/v1/models/mock-small", 404, ""}, // an upstream name isn't a model here
		{"/v1/models/", 404, ""},
		{"/v1/models/gpt-9", 404, ""},
	} {
		resp, err := srv.Client().Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: %d %s", tc.path, resp.StatusCode, b)
		} else if tc.status == 200 && string(b) != tc.body {
			t.Errorf("%s: %s", tc.path, b)
		} else if _, code, _, _ := errorOfOrEmpty(b); tc.status == 404 && code != "model_not_found" {
			t.Errorf("%s: %s", tc.path, b)
		}
	}

	status := func(path string) int {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status("/livez") != 200 || status("/readyz") != 503 {
		t.Error("before SetReady: want live and not ready")
	}
	g.SetReady(true)
	if status("/readyz") != 200 {
		t.Error("after SetReady(true): not ready")
	}
	g.SetReady(false)
	if status("/readyz") != 503 {
		t.Error("after SetReady(false): still ready")
	}
	if len(up.requests()) != 0 {
		t.Error("the provider was asked")
	}
}

// answering is a provider that answers every request with status, body and header.
func answering(status int, body string, header ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func TestEachAnswerIsMappedByTheTable(t *testing.T) {
	const oops = `{"error":{"message":"bad temperature","type":"invalid_request_error","param":"temperature","code":null}}`
	const done = `{"id":"x","object":"chat.completion","choices":[]}`
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		header     []string
		want       int
		passed     bool   // the body is the provider's
		code       string // else the gateway's error code
		retryAfter string
	}{
		{"an answer", 200, done, nil, 200, true, "", ""},
		{"an answer with whitespace", 200, "\n " + done + "\n", nil, 200, true, "", ""},
		{"200 with text", 200, "hello", nil, 502, false, "upstream_error", ""},
		{"200 with an array", 200, `[1,2]`, nil, 502, false, "upstream_error", ""},
		{"200 with broken JSON", 200, `{"id":`, nil, 502, false, "upstream_error", ""},
		{"200 with nothing", 200, ``, nil, 502, false, "upstream_error", ""},
		{"a 400", 400, oops, nil, 400, true, "", ""},
		{"a 413", 413, oops, nil, 413, true, "", ""},
		{"a 422", 422, oops, nil, 422, true, "", ""},
		{"a 429", 429, oops, []string{"Retry-After", "7", "X-Ratelimit-Remaining-Requests", "0"}, 429, true, "", "7"},
		{"a 429 in HTML", 429, "<html>slow down</html>", []string{"Retry-After", "3"}, 429, false,
			"upstream_error", "3"},
		{"a 400 in text", 400, "bad", nil, 400, false, "upstream_error", ""},
		{"a 401", 401, oops, nil, 502, false, "upstream_auth", ""},
		{"a 403", 403, oops, nil, 502, false, "upstream_auth", ""},
		{"a 404", 404, oops, nil, 502, false, "upstream_error", ""},
		{"a 409", 409, oops, nil, 502, false, "upstream_error", ""},
		{"a 500", 500, oops, nil, 502, false, "upstream_error", ""},
		{"a 503", 503, oops, []string{"Retry-After", "1"}, 502, false, "upstream_error", ""},
		{"a 201", 201, done, nil, 502, false, "upstream_error", ""},
		{"a 204", 204, "", nil, 502, false, "upstream_error", ""},
		{"a redirect", 307, "", []string{"Location", "/elsewhere"}, 502, false, "upstream_error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newProvider(t, answering(tc.status, tc.body, tc.header...))
			srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/m")
			resp, b := send(t, srv, ask)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.want, b)
			}
			if tc.passed {
				if string(b) != tc.body {
					t.Errorf("body %q, want the provider's %q", b, tc.body)
				}
			} else if _, code, _, msg := errorOf(t, b); code != tc.code || !strings.Contains(msg, "p0") {
				t.Errorf("code %q (%s), want %q naming the provider", code, msg, tc.code)
			}
			if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After %q, want %q", got, tc.retryAfter)
			}
			if resp.Header.Get("X-Ratelimit-Remaining-Requests") != "" {
				t.Error("the provider's own rate-limit headers reached the client")
			}
			if n := len(up.requests()); n != 1 {
				t.Errorf("the provider got %d requests (a redirect followed?)", n)
			}
		})
	}
}

func TestAnAnswerOverTheLimitIs502(t *testing.T) {
	exactly := `{"pad":"` + strings.Repeat("x", 1000-10) + `"}`
	if len(exactly) != 1000 {
		t.Fatal(len(exactly))
	}
	up := newProvider(t, answering(200, exactly))
	srv, _, _ := gatewayFor(t, `"max_response_bytes":1000`, []string{at(up, "")}, "fast=p0/m")
	if resp, b := send(t, srv, ask); resp.StatusCode != 200 || string(b) != exactly {
		t.Errorf("exactly the limit: %d", resp.StatusCode)
	}
	up2 := newProvider(t, answering(200, exactly+" "))
	srv2, _, _ := gatewayFor(t, `"max_response_bytes":1000`, []string{at(up2, "")}, "fast=p0/m")
	resp, b := send(t, srv2, ask)
	if _, code, _, msg := errorOf(t, b); resp.StatusCode != 502 || code != "upstream_error" ||
		!strings.Contains(msg, "more than 1000 bytes") {
		t.Errorf("one byte over: %d %s", resp.StatusCode, b)
	}
}

// waiting is a provider that never answers; it reports each request's cancellation on cancelled.
func waiting(cancelled chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
}

func TestAProviderSlowerThanItsTimeoutIs504AndIsCancelled(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, waiting(cancelled))
	srv, _, logs := gatewayFor(t, "", []string{at(up, `"timeout":"150ms"`)}, "fast=p0/m")
	began := time.Now()
	resp, b := send(t, srv, ask)
	took := time.Since(began)
	if _, code, _, msg := errorOf(t, b); resp.StatusCode != 504 || code != "upstream_timeout" ||
		!strings.Contains(msg, "150ms") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if took < 150*time.Millisecond {
		t.Errorf("answered in %v, before the timeout", took)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the provider's request was never cancelled")
	}
	if !strings.Contains(logs.String(), `"status":504`) || !strings.Contains(logs.String(), `"upstream_status":0`) {
		t.Errorf("log:\n%s", logs)
	}
}

func TestAnAnswerThatTricklesPastTheTimeoutIs504(t *testing.T) {
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"id":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the rest never comes
	}))
	srv, _, logs := gatewayFor(t, "", []string{at(up, `"timeout":"150ms"`)}, "fast=p0/m")
	resp, b := send(t, srv, ask)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 504 || code != "upstream_timeout" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if !strings.Contains(logs.String(), `"upstream_status":200`) {
		t.Errorf("the log should show the provider's 200 before the body stalled:\n%s", logs)
	}
}

func TestAProviderThatCantBeReachedOrBreaksOffIs502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close() // nothing listens here now

	breaks := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"id\":"))
			conn.Close()
		}
	}))
	srv, _, logs := gatewayFor(t, "", []string{fmt.Sprintf(`"base_url":"http://%s/v1"`, closed), at(breaks, "")},
		"gone=p0/m", "cut=p1/m")
	for _, model := range []string{"gone", "cut"} {
		resp, b := send(t, srv, `{"model":"`+model+`","messages":[{"role":"user","content":"x"}]}`)
		if _, code, _, msg := errorOf(t, b); resp.StatusCode != 502 || code != "upstream_unavailable" ||
			strings.Contains(msg, "127.0.0.1") {
			t.Errorf("%s: %d %s (the client shouldn't see the provider's address)", model, resp.StatusCode, b)
		}
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("the log should have the cause:\n%s", logs)
	}
}

// A completion that reached the provider is never sent again by Go's transport. net/http resends a request by itself
// on a reused connection that breaks, if the request is "replayable": a GET, or a POST carrying an Idempotency-Key
// or X-Idempotency-Key header. The gateway's POSTs carry neither, so a provider that reads the second request and
// drops the connection sees two requests in all, and the client a 502. Adding such a header upstream (say, for
// caching on day 3) would make this test see a third.
func TestAForwardedRequestIsNeverResentByTheTransport(t *testing.T) {
	var n atomic.Int32
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"first"}`) // keep-alive: the second request reuses this connection
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() // read the request, answer nothing
		}
	}))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/m")
	if resp, b := send(t, srv, ask); resp.StatusCode != 200 {
		t.Fatalf("first: %d %s", resp.StatusCode, b)
	}
	resp, b := send(t, srv, ask)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 502 || code != "upstream_unavailable" {
		t.Errorf("second: %d %s", resp.StatusCode, b)
	}
	time.Sleep(50 * time.Millisecond) // a resend would arrive about now
	if got := len(up.requests()); got != 2 {
		t.Errorf("the provider saw %d requests, want 2: the transport resent one", got)
	}
}

// Real providers are https. NewTransport keeps Go's default of attempting HTTP/2 there (a clone of the default
// transport, ForceAttemptHTTP2 set), which also survives giving it a TLS configuration of its own, as this test does
// to trust the test server's certificate.
func TestAnHTTPSProviderIsReachedOverHTTP2(t *testing.T) {
	var proto atomic.Value
	m := mockProvider(t, "mock-a")
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		m.ServeHTTP(w, r)
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	t.Cleanup(up.Close)

	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"providers":[{"name":"tls","base_url":%q}],`+
		`"models":[{"name":"fast","provider":"tls","upstream_model":"mock-small"}]}`, up.URL+"/v1")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTransport()
	tr.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	srv := httptest.NewServer(New(cfg, tr, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	if resp, b := send(t, srv, ask); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got := proto.Load(); got != "HTTP/2.0" {
		t.Errorf("the provider was reached over %v", got)
	}
}

func TestAClientThatGoesAwayCancelsTheProvidersRequest(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, waiting(cancelled))
	srv, _, logs := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/m")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(ask))
	req.Header.Set("Content-Type", "application/json")
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("the request finished")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's request outlived the client's")
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"status":499`); {
		if time.Now().After(deadline) {
			t.Fatalf("no 499 in the log:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAShutdownCancellationIsA503(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, waiting(cancelled))
	_, g, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/m")
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	go func() {
		for len(up.requests()) == 0 {
			time.Sleep(time.Millisecond)
		}
		stop(ErrShutdown)
	}()
	resp, b := send(t, srv, ask)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "shutting_down" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	<-cancelled
}

func TestTheLogHasTheFactsAndNoContentOrKeys(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, "", []string{at(up, `"api_key_env":"P_KEY"`)}, "fast=p0/mock-small")
	resp, _ := send(t, srv, `{"model":"fast","messages":[{"role":"user","content":"my secret prompt"}]}`,
		"Authorization", "Bearer client-secret")
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &line); err != nil {
		t.Fatalf("%v: %s", err, logs)
	}
	for k, want := range map[string]any{
		"msg": "chat completion", "level": "INFO", "id": resp.Header.Get("X-Request-Id"), "status": 200.0,
		"model": "fast", "provider": "p0", "upstream_model": "mock-small", "upstream_status": 200.0,
	} {
		if line[k] != want {
			t.Errorf("%s = %v, want %v", k, line[k], want)
		}
	}
	for _, k := range []string{"ms", "upstream_ms", "request_bytes", "response_bytes"} {
		if v, ok := line[k].(float64); !ok || v < 0 {
			t.Errorf("%s = %v", k, line[k])
		}
	}
	for _, secret := range []string{"my secret prompt", "sk-provider-key-for-tests", "client-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log has %q", secret)
		}
	}
}

func TestConcurrentRequestsEachGetTheirOwnAnswer(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			word := fmt.Sprintf("request-%d", i)
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"`+word+`"}]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				errs <- err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(b), `\"`+word+`\"`) {
				errs <- fmt.Errorf("%s: %d %s", word, resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// sendAs is send with the request's Host header set to host.
func sendAs(t *testing.T, srv *httptest.Server, host, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(ask))
	req.Header.Set("Content-Type", "application/json")
	req.Host = host // Go's client ignores a "Host" in req.Header
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// A web page whose own domain is made to resolve to 127.0.0.1 (DNS rebinding) is same-origin with the gateway it
// reaches that way, so it can post JSON without a preflight; only its Host header gives it away. Suspected in the
// day-1 review; before the check, a request with Host attacker.example:8108 got a 200 and reached the provider.
func TestOnlyAllowedHostsAreAnswered(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"localhost:8108", true},
		{"LOCALHOST:8108", true},
		{"localhost.:8108", true},
		{"localhost", true},
		{"127.0.0.1:8108", true},
		{"[::1]:8108", true},
		{"[::1]", true},
		{"attacker.example:8108", false},
		{"attacker.example", false},
		{"127.0.0.1.nip.io:8108", false},
		{"localhost.attacker.example", false},
		{"0.0.0.0:8108", false},
	} {
		resp, b := sendAs(t, srv, tc.host, "/v1/chat/completions")
		if tc.ok != (resp.StatusCode == 200) {
			t.Errorf("Host %s: %d %s", tc.host, resp.StatusCode, b)
			continue
		}
		if !tc.ok {
			if _, code, _, _ := errorOf(t, b); resp.StatusCode != 403 || code != "host_not_allowed" {
				t.Errorf("Host %s: %d %s", tc.host, resp.StatusCode, b)
			}
		}
	}
	if got := len(up.requests()); got != 7 {
		t.Errorf("the provider got %d requests, want only the 7 allowed", got)
	}
	// Every path is checked, health endpoints included.
	if resp, _ := sendAs(t, srv, "attacker.example", "/v1/models"); resp.StatusCode != 403 {
		t.Errorf("/v1/models from a foreign host: %d", resp.StatusCode)
	}

	named, _, _ := gatewayFor(t, `"allowed_hosts":["gateway.internal"]`, []string{at(up, "")}, "fast=p0/mock-small")
	if resp, _ := sendAs(t, named, "gateway.internal:443", "/v1/chat/completions"); resp.StatusCode != 200 {
		t.Errorf("a configured host: %d", resp.StatusCode)
	}
	if resp, _ := sendAs(t, named, "localhost:8108", "/v1/chat/completions"); resp.StatusCode != 403 {
		t.Errorf("localhost when only gateway.internal is allowed: %d", resp.StatusCode)
	}
	anyHost, _, _ := gatewayFor(t, `"allowed_hosts":["*"]`, []string{at(up, "")}, "fast=p0/mock-small")
	if resp, _ := sendAs(t, anyHost, "attacker.example", "/v1/chat/completions"); resp.StatusCode != 200 {
		t.Errorf(`"*": %d`, resp.StatusCode)
	}
}

// A body still arriving when the stop cancels the requests: the read is woken and the client gets its 503, rather
// than having the connection cut under it when the server closes (a suspicion of the day-1 review; before the
// change this test hit its own deadline, the read never woke).
func TestABodyStillArrivingAtTheStopGetsA503(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	_, g, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\n\r\n%s", len(ask), ask[:10])
	time.Sleep(50 * time.Millisecond) // the handler is now reading the body
	stop(ErrShutdown)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "shutting_down" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("the provider got %d requests", n)
	}
}
