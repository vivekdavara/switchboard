package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/openai"
)

// fast is a config that answers at once, for tests that aren't about latency.
func fast() Config {
	c := Defaults()
	c.Name = "mock-a"
	c.TTFT, c.PerToken = 0, 0
	c.Now = func() time.Time { return time.Unix(1_760_000_000, 0) }
	return c
}

func start(t *testing.T, cfg Config) (*Provider, *httptest.Server) {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { p.Close(); srv.Close() })
	return p, srv
}

const hello = `{"model":"mock-small","messages":[{"role":"system","content":"Be brief please"},` +
	`{"role":"user","content":"Hello there, how are you today?"}]`

// post sends a chat completion and returns the status and the body.
func post(t *testing.T, srv *httptest.Server, body string, header ...string) (int, []byte) {
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
	return resp.StatusCode, b
}

func completion(t *testing.T, b []byte) openai.ChatCompletion {
	t.Helper()
	var c openai.ChatCompletion
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return c
}

func apiError(t *testing.T, b []byte) (typ, code, param string) {
	t.Helper()
	var e struct {
		Error struct {
			Message, Type string
			Code, Param   *string
		}
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Message == "" {
		t.Fatalf("not an OpenAI error: %s", b)
	}
	if e.Error.Code != nil {
		code = *e.Error.Code
	}
	if e.Error.Param != nil {
		param = *e.Error.Param
	}
	return e.Error.Type, code, param
}

func TestAnAnswerIsDeterministicAndCountsWordsAsTokens(t *testing.T) {
	p, srv := start(t, fast())
	status, b := post(t, srv, hello+`}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, b)
	}
	c := completion(t, b)
	want := `This is mock-a answering as mock-small. You said: "Hello there, how are you today?". and so the ` +
		`gateway passes the words along one by one until the answer is long enough and`
	if len(c.Choices) != 1 || c.Choices[0].Message.Content != want {
		t.Fatalf("got %+v\nwant content %q", c.Choices, want)
	}
	if got := len(strings.Fields(c.Choices[0].Message.Content)); got != 32 {
		t.Errorf("%d words, want 32", got)
	}
	ch := c.Choices[0]
	if ch.Index != 0 || ch.FinishReason != "stop" || ch.Message.Role != "assistant" || ch.Logprobs != nil {
		t.Errorf("choice %+v", ch)
	}
	// 3 + 6 words of prompt, 32 of answer.
	if c.Usage != (openai.Usage{PromptTokens: 9, CompletionTokens: 32, TotalTokens: 41}) {
		t.Errorf("usage %+v", c.Usage)
	}
	if c.ID != "chatcmpl-mock-a-000001" || c.Object != "chat.completion" || c.Created != 1_760_000_000 ||
		c.Model != "mock-small" {
		t.Errorf("completion %+v", c)
	}
	_, b2 := post(t, srv, hello+`}`)
	if c2 := completion(t, b2); c2.ID != "chatcmpl-mock-a-000002" || c2.Choices[0] != c.Choices[0] {
		t.Errorf("second answer %+v", c2)
	}
	if s := p.Stats(); s != (Stats{Requests: 2, Answered: 2}) {
		t.Errorf("stats %+v", s)
	}
}

func TestALongMessageIsQuotedByItsFirstEightWords(t *testing.T) {
	_, srv := start(t, fast())
	_, b := post(t, srv, `{"model":"mock-large","messages":[{"role":"user","content":"one two three four five six `+
		`seven eight nine ten"},{"role":"assistant","content":"ok"}]}`)
	got := completion(t, b).Choices[0].Message.Content
	if !strings.HasPrefix(got, `This is mock-a answering as mock-large. You said: "one two three four five six seven `+
		`eight ...". and`) {
		t.Errorf("got %q", got)
	}
}

func TestMaxTokensCutsTheAnswer(t *testing.T) {
	_, srv := start(t, fast())
	for _, tc := range []struct {
		extra  string
		tokens int
		finish string
	}{
		{`,"max_tokens":5`, 5, "length"},
		{`,"max_completion_tokens":7`, 7, "length"},
		{`,"max_tokens":9,"max_completion_tokens":4`, 4, "length"}, // the smaller wins
		{`,"max_tokens":1`, 1, "length"},
		{`,"max_tokens":32`, 32, "stop"}, // exactly the default isn't a cut
		{`,"max_tokens":1000`, 32, "stop"},
		{`,"max_tokens":null`, 32, "stop"},
	} {
		status, b := post(t, srv, hello+tc.extra+`}`)
		if status != 200 {
			t.Errorf("%s: status %d: %s", tc.extra, status, b)
			continue
		}
		c := completion(t, b)
		if got := len(strings.Fields(c.Choices[0].Message.Content)); got != tc.tokens ||
			c.Usage.CompletionTokens != tc.tokens || c.Choices[0].FinishReason != tc.finish {
			t.Errorf("%s: %d words, usage %+v, finish %s; want %d, %s", tc.extra, got, c.Usage,
				c.Choices[0].FinishReason, tc.tokens, tc.finish)
		}
	}
}

func TestNChoicesAreCountedInUsage(t *testing.T) {
	_, srv := start(t, fast())
	_, b := post(t, srv, hello+`,"n":3,"max_tokens":4}`)
	c := completion(t, b)
	if len(c.Choices) != 3 || c.Usage.CompletionTokens != 12 || c.Usage.TotalTokens != 21 {
		t.Fatalf("%+v", c)
	}
	for i, ch := range c.Choices {
		if ch.Index != i || ch.Message.Content != c.Choices[0].Message.Content {
			t.Errorf("choice %d: %+v", i, ch)
		}
	}
}

func TestRequestsItCantServeAreRefusedInOpenAIsShape(t *testing.T) {
	p, srv := start(t, fast())
	for _, tc := range []struct {
		body             string
		status           int
		code, param, typ string
	}{
		{`{"model":"gpt-9","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found", "model",
			openai.TypeInvalidRequest},
		{hello + `,"stream":true}`, 400, "unsupported_parameter", "stream", openai.TypeInvalidRequest},
		{hello + `,"max_tokens":0}`, 400, "invalid_value", "max_tokens", openai.TypeInvalidRequest},
		{hello + `,"max_tokens":2.5}`, 400, "invalid_value", "max_tokens", openai.TypeInvalidRequest},
		{hello + `,"max_completion_tokens":"9"}`, 400, "invalid_value", "max_completion_tokens",
			openai.TypeInvalidRequest},
		{hello + `,"n":129}`, 400, "invalid_value", "n", openai.TypeInvalidRequest},
		{`{"model":"mock-small"}`, 400, "missing_required_parameter", "messages", openai.TypeInvalidRequest},
		{hello + `,"model":"x"}`, 400, "invalid_json", "", openai.TypeInvalidRequest},
	} {
		status, b := post(t, srv, tc.body)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d: %s", tc.body, status, tc.status, b)
			continue
		}
		if typ, code, param := apiError(t, b); typ != tc.typ || code != tc.code || param != tc.param {
			t.Errorf("%s: got %s %s %s, want %s %s %s", tc.body, typ, code, param, tc.typ, tc.code, tc.param)
		}
	}
	if s := p.Stats(); s.Requests != 8 || s.Refused != 8 || s.Answered != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestAnEmptyModelListServesAnyModel(t *testing.T) {
	cfg := fast()
	cfg.Models = nil
	_, srv := start(t, cfg)
	if status, b := post(t, srv, `{"model":"anything","messages":[{"role":"user","content":"x"}]}`); status != 200 {
		t.Errorf("status %d: %s", status, b)
	}
}

func TestTheAPIKeyIsRequiredWhenSet(t *testing.T) {
	cfg := fast()
	cfg.APIKey = "sk-test-not-a-real-key"
	p, srv := start(t, cfg)
	for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer sk-test-not-a-real-ke", "bearer sk-test-not-a-real-key",
		"sk-test-not-a-real-key", "Bearer sk-test-not-a-real-key2"} {
		var header []string
		if auth != "" {
			header = []string{"Authorization", auth}
		}
		status, b := post(t, srv, hello+`}`, header...)
		if status != 401 {
			t.Errorf("%q: status %d", auth, status)
			continue
		}
		if _, code, _ := apiError(t, b); code != "invalid_api_key" {
			t.Errorf("%q: code %s", auth, code)
		}
	}
	if status, _ := post(t, srv, hello+`}`, "Authorization", "Bearer sk-test-not-a-real-key"); status != 200 {
		t.Errorf("the right key: status %d", status)
	}
	if s := p.Stats(); s.Refused != 7 || s.Answered != 1 {
		t.Errorf("stats %+v", s)
	}
}

func TestOtherPathsAndMethodsAreOpenAIErrors(t *testing.T) {
	_, srv := start(t, fast())
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/v1/chat/completions", 405, "POST"},
		{"DELETE", "/v1/models", 405, "GET, HEAD"},
		{"POST", "/v1/embeddings", 404, ""},
		{"GET", "/", 404, ""},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || resp.Header.Get("Allow") != tc.allow ||
			resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d, Allow %q, %s", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Allow"), b)
			continue
		}
		apiError(t, b)
	}
	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list openai.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || list.Object != "list" || len(list.Data) != 2 ||
		list.Data[0] != (openai.Model{ID: "mock-small", Object: "model", OwnedBy: "mock-a"}) {
		t.Errorf("models: %+v %v", list, err)
	}
}

func TestABodyOverTheLimitIs413(t *testing.T) {
	_, srv := start(t, fast())
	big := hello + `,"pad":"` + strings.Repeat("x", MaxRequestBytes) + `"}`
	status, b := post(t, srv, big)
	if status != 413 {
		t.Fatalf("status %d", status)
	}
	if _, code, _ := apiError(t, b); code != "request_too_large" {
		t.Errorf("code %s", code)
	}
}

func TestTheLatencyIsTimeToFirstTokenPlusPerToken(t *testing.T) {
	cfg := fast()
	cfg.TTFT, cfg.PerToken, cfg.CompletionTokens = 60*time.Millisecond, 4*time.Millisecond, 10
	_, srv := start(t, cfg)
	began := time.Now()
	if status, _ := post(t, srv, hello+`}`); status != 200 {
		t.Fatal(status)
	}
	// 60 + 10 × 4 = 100 ms at least. No upper bound: a loaded CI machine can be slow, never fast.
	if took := time.Since(began); took < 100*time.Millisecond {
		t.Errorf("answered in %v, want at least 100ms", took)
	}
	began = time.Now()
	post(t, srv, hello+`,"max_tokens":2}`) // 60 + 2 × 4
	if took := time.Since(began); took < 68*time.Millisecond {
		t.Errorf("answered in %v, want at least 68ms", took)
	}
}

func TestJitterScalesEachDelayWithinItsBounds(t *testing.T) {
	cfg := fast()
	cfg.Jitter = 0.25
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := 2.0, 0.0
	for range 10_000 {
		f := p.draw()
		for _, x := range []float64{f.ttftF, f.perTokenF} {
			lo, hi = min(lo, x), max(hi, x)
		}
	}
	if lo < 0.75 || hi > 1.25 || lo > 0.76 || hi < 1.24 {
		t.Errorf("factors in [%v, %v], want spread over [0.75, 1.25]", lo, hi)
	}
}

// outcomes sends n requests one at a time, giving up on each after timeout, and returns what each got.
func outcomes(t *testing.T, srv *httptest.Server, n int, timeout time.Duration) []string {
	t.Helper()
	var got []string
	for range n {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(hello+`}`))
		resp, err := srv.Client().Do(req)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			got = append(got, "hang")
		case err != nil:
			t.Fatal(err)
		default:
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			got = append(got, resp.Status[:3])
		}
		cancel()
	}
	return got
}

func TestFailuresFollowTheSeed(t *testing.T) {
	cfg := fast()
	cfg.ErrorRate, cfg.ErrorStatus, cfg.HangRate, cfg.Seed = 0.3, 503, 0.1, 42
	p1, srv1 := start(t, cfg)
	_, srv2 := start(t, cfg)
	a := outcomes(t, srv1, 100, 50*time.Millisecond)
	b := outcomes(t, srv2, 100, 50*time.Millisecond)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("the same seed gave different runs:\n%v\n%v", a, b)
	}
	count := map[string]int{}
	for _, o := range a {
		count[o]++
	}
	// Binomial(100, 0.3) and (100, 0.1): these bounds are about four standard deviations wide, and the seed is fixed.
	if count["503"] < 12 || count["503"] > 48 || count["hang"] < 1 || count["hang"] > 22 ||
		count["200"] != 100-count["503"]-count["hang"] {
		t.Errorf("outcomes %v", count)
	}
	s := p1.Stats()
	if s.Requests != 100 || s.Failed != int64(count["503"]) || s.Hung != int64(count["hang"]) ||
		s.Answered != int64(count["200"]) {
		t.Errorf("stats %+v, outcomes %v", s, count)
	}
	cfg.Seed = 43
	_, srv3 := start(t, cfg)
	if c := outcomes(t, srv3, 100, 50*time.Millisecond); strings.Join(c, ",") == strings.Join(a, ",") {
		t.Error("another seed gave the same run")
	}
}

func TestARaisedErrorRateOnlyAddsFailures(t *testing.T) {
	cfg := fast()
	cfg.Jitter, cfg.ErrorStatus, cfg.Seed = 0.5, 500, 7
	cfg.ErrorRate = 0.1
	_, low := start(t, cfg)
	cfg.ErrorRate = 0.4
	_, high := start(t, cfg)
	a, b := outcomes(t, low, 200, time.Second), outcomes(t, high, 200, time.Second)
	more := 0
	for i := range a {
		if a[i] == "500" && b[i] != "500" {
			t.Fatalf("request %d failed at rate 0.1 but not at 0.4", i)
		}
		if a[i] != b[i] {
			more++
		}
	}
	if more == 0 {
		t.Error("the higher rate failed no more requests")
	}
}

func TestAnInjected429IsARateLimitError(t *testing.T) {
	cfg := fast()
	cfg.ErrorRate, cfg.ErrorStatus = 1, 429
	_, srv := start(t, cfg)
	status, b := post(t, srv, hello+`}`)
	if typ, code, _ := apiError(t, b); status != 429 || typ != openai.TypeRateLimit || code != "mock_failure" {
		t.Errorf("%d %s %s", status, typ, code)
	}
}

func TestCloseDropsHeldRequestsAndCountsAbandonedOnes(t *testing.T) {
	cfg := fast()
	cfg.HangRate = 1
	p, srv := start(t, cfg)
	done := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(hello+`}`))
		if err == nil {
			resp.Body.Close()
			err = errors.New("got a response: " + resp.Status)
		}
		done <- err
	}()
	waitFor(t, func() bool { return p.Stats().Hung == 1 })
	p.Close()
	select {
	case err := <-done:
		if !strings.Contains(err.Error(), "EOF") {
			t.Errorf("got %v, want the connection dropped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't release the hung request")
	}

	// A client that gives up while the answer is being "written" is counted as abandoning it.
	cfg = fast()
	cfg.TTFT = time.Minute
	p, srv = start(t, cfg)
	if got := outcomes(t, srv, 1, 50*time.Millisecond); got[0] != "hang" {
		t.Fatalf("got %v", got)
	}
	waitFor(t, func() bool { return p.Stats().Abandoned == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewRefusesImpossibleConfigs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"no name", func(c *Config) { c.Name = "" }},
		{"negative ttft", func(c *Config) { c.TTFT = -1 }},
		{"negative per token", func(c *Config) { c.PerToken = -1 }},
		{"jitter over 1", func(c *Config) { c.Jitter = 1.5 }},
		{"negative jitter", func(c *Config) { c.Jitter = -0.1 }},
		{"no tokens", func(c *Config) { c.CompletionTokens = 0 }},
		{"error rate over 1", func(c *Config) { c.ErrorRate = 2 }},
		{"negative hang rate", func(c *Config) { c.HangRate = -1 }},
		{"rates over 1", func(c *Config) { c.ErrorRate, c.HangRate = 0.6, 0.6 }},
		{"a 200 as the error", func(c *Config) { c.ErrorRate, c.ErrorStatus = 0.1, 200 }},
		{"a 600 as the error", func(c *Config) { c.ErrorRate, c.ErrorStatus = 0.1, 600 }},
	} {
		cfg := fast()
		tc.change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	cfg := fast()
	cfg.ErrorStatus = 0 // fine while nothing fails
	if _, err := New(cfg); err != nil {
		t.Errorf("no error status at error rate 0: %v", err)
	}
}
