package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the server's goroutine and the test can share.
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

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// serve starts run with args and returns what it started, a stop function, and the channel its exit code arrives
// on.
func serve(t *testing.T, args []string, getenv func(string) string, logs *syncBuffer) (started, func(), chan int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listening := make(chan started, 1)
	code := make(chan int, 1)
	go func() { code <- run(ctx, append([]string{"-addr", "127.0.0.1:0"}, args...), getenv, logs, listening) }()
	select {
	case s := <-listening:
		return s, cancel, code
	case c := <-code:
		cancel()
		t.Fatalf("exited %d before listening: %s", c, logs)
	case <-time.After(10 * time.Second):
		t.Fatal("never listened")
	}
	return started{}, nil, nil
}

func TestItServesWithItsFlagsAndStopsCleanly(t *testing.T) {
	logs := &syncBuffer{}
	srv, stop, code := serve(t, []string{"-name", "mock-b", "-models", "small, large", "-ttft", "0",
		"-per-token", "0", "-completion-tokens", "3", "-api-key-env", "MOCK_B_KEY"},
		env(map[string]string{"MOCK_B_KEY": "sk-mock-b-test"}), logs)
	req, _ := http.NewRequest("POST", "http://"+srv.Addr+"/v1/chat/completions",
		strings.NewReader(`{"model":"large","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-mock-b-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var c struct {
		Model   string
		Choices []struct{ Message struct{ Content string } }
	}
	if err := json.Unmarshal(b, &c); err != nil || resp.StatusCode != 200 || c.Model != "large" ||
		c.Choices[0].Message.Content != "This is mock-b" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	stop()
	if got := <-code; got != 0 {
		t.Errorf("exit %d", got)
	}
	out := logs.String()
	for _, want := range []string{`"msg":"listening"`, `"name":"mock-b"`, `"models":["small","large"]`,
		`"api_key":true`, `"msg":"stopped"`, `"requests":1`, `"answered":1`, `"clean":true`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-mock-b-test") {
		t.Error("the key is in the logs")
	}
}

func TestAStopDropsAHungRequestAndStillExitsCleanly(t *testing.T) {
	logs := &syncBuffer{}
	srv, stop, code := serve(t, []string{"-hang-rate", "1"}, env(nil), logs)
	errc := make(chan error, 1)
	go func() {
		resp, err := http.Post("http://"+srv.Addr+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"mock-small","messages":[{"role":"user","content":"hi"}]}`))
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	// Wait for the handler, not just for the client to write: a connection still in the listener's queue when the
	// stop closes it is reset, never served (an earlier version of this test waited on httptrace's WroteRequest
	// and saw exactly that).
	for deadline := time.Now().Add(5 * time.Second); srv.Stats().Hung != 1; {
		if time.Now().After(deadline) {
			t.Fatal("the request never hung")
		}
		time.Sleep(time.Millisecond)
	}
	began := time.Now()
	stop()
	if got := <-code; got != 0 {
		t.Errorf("exit %d", got)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("stopping took %v; the hung request held it", took)
	}
	if err := <-errc; err == nil {
		t.Error("the hung request got an answer")
	}
	if out := logs.String(); !strings.Contains(out, `"hung":1`) {
		t.Errorf("logs:\n%s", out)
	}
}

func TestBadFlagsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-nonsense"},
		{"-ttft", "soon"},
		{"-error-rate", "2"},
		{"-cut-rate", "1.5"},
		{"-jitter", "-1"},
		{"-name", ""},
		{"extra"},
	} {
		logs := &syncBuffer{}
		if got := run(context.Background(), args, env(nil), logs, nil); got != 2 {
			t.Errorf("%q: exit %d, want 2: %s", args, got, logs)
		}
	}
}

func TestAMissingKeyOrABusyPortIsAnError(t *testing.T) {
	logs := &syncBuffer{}
	if got := run(context.Background(), []string{"-api-key-env", "UNSET_KEY"}, env(nil), logs, nil); got != 1 {
		t.Errorf("unset key: exit %d", got)
	}
	if !strings.Contains(logs.String(), "UNSET_KEY") {
		t.Errorf("logs don't name the variable: %s", logs)
	}
	srv, stop, code := serve(t, nil, env(nil), &syncBuffer{})
	defer func() { stop(); <-code }()
	if got := run(context.Background(), []string{"-addr", srv.Addr}, env(nil), &syncBuffer{}, nil); got != 1 {
		t.Errorf("busy port: exit %d", got)
	}
}

func TestACutRateOfOneBreaksEveryAnswer(t *testing.T) {
	logs := &syncBuffer{}
	srv, stop, code := serve(t, []string{"-ttft", "0", "-per-token", "0", "-cut-rate", "1"}, env(nil), logs)
	for _, body := range []string{`{"model":"mock-small","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"mock-small","messages":[{"role":"user","content":"hi"}],"stream":true}`} {
		resp, err := http.Post("http://"+srv.Addr+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || err == nil {
			t.Errorf("%s: %d, read error %v; want a 200 that breaks off", body, resp.StatusCode, err)
		}
	}
	stop()
	if got := <-code; got != 0 {
		t.Errorf("exit %d", got)
	}
	for _, want := range []string{`"cut_rate":1`, `"cut":2`, `"answered":0`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}
