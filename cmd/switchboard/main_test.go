package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/mock"
)

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

func noEnv(string) string { return "" }

// writeConfig writes a configuration that listens on a free port and sends model "fast" to upstream.
func writeConfig(t *testing.T, upstream, grace string) string {
	t.Helper()
	return writeDrainingConfig(t, upstream, grace, "0s")
}

// writeDrainingConfig is writeConfig with a drain_delay.
func writeDrainingConfig(t *testing.T, upstream, grace, drain string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"listen":"127.0.0.1:0","shutdown_grace":%q,"drain_delay":%q,
		"providers":[{"name":"mock-a","base_url":%q,"timeout":"10s"}],
		"models":[{"name":"fast","provider":"mock-a","upstream_model":"mock-small"}]}`, grace, drain, upstream+"/v1")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func upstream(t *testing.T, change func(*mock.Config)) (*mock.Provider, *httptest.Server) {
	t.Helper()
	cfg := mock.Defaults()
	cfg.Name, cfg.TTFT, cfg.PerToken = "mock-a", 0, 0
	if change != nil {
		change(&cfg)
	}
	p, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { p.Close(); srv.Close() })
	return p, srv
}

// start runs serve and returns its address, a stop function and the exit code's channel.
func start(t *testing.T, path string, logs io.Writer) (string, context.CancelFunc, chan int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listening := make(chan string, 1)
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"serve", "-config", path}, noEnv, io.Discard, logs, listening) }()
	select {
	case addr := <-listening:
		return addr, cancel, code
	case c := <-code:
		cancel()
		t.Fatalf("exited %d before listening: %s", c, logs)
	case <-time.After(10 * time.Second):
		t.Fatal("never listened")
	}
	return "", nil, nil
}

func chat(addr string) (int, string, error) {
	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func TestServeAnswersAndStopsCleanly(t *testing.T) {
	_, up := upstream(t, nil)
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeConfig(t, up.URL, "5s"), logs)
	status, body, err := chat(addr)
	if err != nil || status != 200 || !strings.Contains(body, `"model":"mock-small"`) {
		t.Fatalf("%d %s %v", status, body, err)
	}
	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("readyz: %v %v", resp, err)
	}
	resp.Body.Close()
	stop()
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
	out := logs.String()
	for _, want := range []string{`"msg":"listening"`, `"providers":["mock-a"]`, `"models":["fast"]`,
		`"msg":"chat completion"`, `"msg":"stopping"`, `"msg":"stopped","clean":true`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %s:\n%s", want, out)
		}
	}
	if _, _, err := chat(addr); err == nil {
		t.Error("still answering after the stop")
	}
}

func TestAStopLetsARequestInFlightFinishWithinTheGrace(t *testing.T) {
	p, up := upstream(t, func(c *mock.Config) { c.TTFT = 400 * time.Millisecond })
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeConfig(t, up.URL, "5s"), logs)
	result := make(chan string, 1)
	go func() {
		status, body, err := chat(addr)
		result <- fmt.Sprint(status, " ", err, " ", len(body) > 0)
	}()
	waitUntil(t, "the request to reach the provider", func() bool { return p.Stats().Requests > 0 })
	stop()
	if got := <-result; got != "200 <nil> true" {
		t.Errorf("the request in flight got %s", got)
	}
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
	if !strings.Contains(logs.String(), `"clean":true`) {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestARequestStillRunningAfterTheGraceGetsA503(t *testing.T) {
	p, up := upstream(t, func(c *mock.Config) { c.HangRate = 1 })
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeConfig(t, up.URL, "300ms"), logs)
	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, body, err := chat(addr)
		done <- result{status, body, err}
	}()
	waitUntil(t, "the request to hang", func() bool { return p.Stats().Hung > 0 })
	began := time.Now()
	stop()
	r := <-done
	if r.err != nil || r.status != 503 || !strings.Contains(r.body, `"code":"shutting_down"`) {
		t.Errorf("got %d %s %v", r.status, r.body, r.err)
	}
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
	if took := time.Since(began); took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("the stop took %v; want the 300ms grace and a little more", took)
	}
	if !strings.Contains(logs.String(), `"msg":"stopped","clean":false`) {
		t.Errorf("logs:\n%s", logs)
	}
	// The provider's request was cancelled with the client's: the hung handler returned.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var line map[string]any
		for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if strings.Contains(l, `"chat completion"`) {
				json.Unmarshal([]byte(l), &line)
			}
		}
		if line != nil {
			if line["code"] != "shutting_down" || line["status"] != 503.0 {
				t.Errorf("log line %v", line)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCheckPrintsTheRoutes(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), []string{"check", "-config", "../../configs/local.json"}, noEnv, &out,
		io.Discard, nil)
	want := `listen 127.0.0.1:8108, drain 0s, shutdown grace 10s, request limit 4194304 bytes, response limit 16777216 bytes
provider mock-a: http://127.0.0.1:8118/v1, timeout 10s, no key
provider mock-b: http://127.0.0.1:8119/v1, timeout 10s, no key
retry budget 0.2 a request + 1 a second; circuit breaker after 5 failures, cooldown 10s
model fast (also gpt-4o-mini) -> mock-a as mock-small (weight 1); no fallback
model smart -> mock-b as mock-large (weight 1), mock-a as mock-large (weight 0); up to 2 attempts
model mock-small -> mock-a as mock-small (weight 1); no fallback
no clients: no key is asked for
`
	if code != 0 || out.String() != want {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
	out.Reset()
	env := func(k string) string {
		if k == "OPENAI_API_KEY" {
			return "sk-test-not-real"
		}
		return ""
	}
	if code := run(context.Background(), []string{"check", "-config", "../../configs/openai.example.json"}, env,
		&out, io.Discard, nil); code != 0 || !strings.Contains(out.String(), "key from OPENAI_API_KEY") ||
		strings.Contains(out.String(), "sk-test-not-real") {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
}

func TestBadInvocationsAndConfigsFail(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"listen":"127.0.0.1:0","listen":"127.0.0.1:1"}`), 0o600)
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{nil, 2, "usage"},
		{[]string{"run"}, 2, "usage"},
		{[]string{"serve"}, 2, "usage"},
		{[]string{"serve", "-config"}, 2, "flag needs an argument"},
		{[]string{"serve", "-config", "x.json", "extra"}, 2, "usage"},
		{[]string{"version", "extra"}, 2, "usage"},
		{[]string{"serve", "-config", "does-not-exist.json"}, 1, "no such file"},
		{[]string{"check", "-config", bad}, 1, `"listen" appears twice`},
		{[]string{"check", "-config", "../../configs/openai.example.json"}, 1, "OPENAI_API_KEY"},
		{[]string{"keygen", "extra"}, 2, "usage"},
		{[]string{"usage"}, 2, "usage"},
		{[]string{"usage", "-config", "../../configs/local.json"}, 1, "names no redis"},
		{[]string{"usage", "-config", "../../configs/clients.example.json", "-day", "2026-13-01"}, 2,
			"not a date"},
		{[]string{"check", "-config", "../../configs/local.json", "-day", "2026-10-20"}, 2,
			"flag provided but not defined"},
	} {
		var errs bytes.Buffer
		if code := run(context.Background(), tc.args, noEnv, io.Discard, &errs, nil); code != tc.code ||
			!strings.Contains(errs.String(), tc.says) {
			t.Errorf("%q: exit %d, want %d; stderr %q", tc.args, code, tc.code, errs.String())
		}
	}
}

func TestABusyPortIsAnError(t *testing.T) {
	_, up := upstream(t, nil)
	addr, stop, code := start(t, writeConfig(t, up.URL, "1s"), io.Discard)
	defer func() { stop(); <-code }()
	path := filepath.Join(t.TempDir(), "busy.json")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"listen":%q,"providers":[{"name":"p","base_url":"http://h"}],`+
		`"models":[{"name":"m","provider":"p"}]}`, addr)), 0o600)
	logs := &syncBuffer{}
	if c := run(context.Background(), []string{"serve", "-config", path}, noEnv, io.Discard, logs, nil); c != 1 ||
		!strings.Contains(logs.String(), "address already in use") {
		t.Errorf("exit %d: %s", c, logs)
	}
}

func TestVersionNamesTheGoRelease(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"version"}, noEnv, &out, io.Discard, nil); code != 0 ||
		!strings.HasPrefix(out.String(), "switchboard ") || !strings.Contains(out.String(), "go1.") {
		t.Errorf("exit %d: %q", code, out.String())
	}
}

// The server's timeouts are DESIGN.md's ("Timeouts and shutdown"): the day-1 review removed all three and no test
// noticed. Waiting out the real values (10 s, 60 s, 120 s) in a test would be slow, so this pins the settings.
func TestTheServerHasItsTimeoutsAndNoWriteTimeout(t *testing.T) {
	base := context.Background()
	srv := newServer(http.NotFoundHandler(), base, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 60*time.Second ||
		srv.IdleTimeout != 120*time.Second || srv.WriteTimeout != 0 {
		t.Errorf("timeouts: header %v, read %v, idle %v, write %v", srv.ReadHeaderTimeout, srv.ReadTimeout,
			srv.IdleTimeout, srv.WriteTimeout)
	}
	if srv.BaseContext(nil) != base {
		t.Error("requests don't derive from the base context")
	}
}

// waitUntil polls cond until it holds, failing the test after 10 s rather than leaving it to go test's timeout.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
