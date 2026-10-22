package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/mock"
)

// writeObservedConfig is writeConfig with extra top-level members (a metrics or log section).
func writeObservedConfig(t *testing.T, upstream, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"listen":"127.0.0.1:0",%s,
		"providers":[{"name":"mock-a","base_url":%q,"timeout":"10s"}],
		"models":[{"name":"fast","provider":"mock-a","upstream_model":"mock-small"}]}`, extra, upstream+"/v1")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// metricsAddr reads the metrics listener's address from the "listening" line.
func metricsAddr(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	for _, l := range strings.Split(logs.String(), "\n") {
		var line struct{ Msg, Metrics string }
		if json.Unmarshal([]byte(l), &line) == nil && line.Msg == "listening" {
			return line.Metrics
		}
	}
	t.Fatalf("no listening line:\n%s", logs)
	return ""
}

func get(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTheMetricsAreServedOnTheirOwnPort(t *testing.T) {
	_, up := upstream(t, nil)
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeObservedConfig(t, up.URL, `"metrics":{"listen":"127.0.0.1:0"}`), logs)
	m := metricsAddr(t, logs)
	if m == "" || m == addr {
		t.Fatalf("metrics at %q, the API at %q", m, addr)
	}
	if status, body, err := chat(addr); err != nil || status != 200 {
		t.Fatalf("%d %s %v", status, body, err)
	}
	status, body := get(t, "http://"+m+"/metrics", "")
	if status != 200 || !strings.Contains(body, `switchboard_requests_total{code="",model="fast",status="200"} 1`+"\n") ||
		!strings.Contains(body, `switchboard_breaker_state{provider="mock-a",state="closed"} 1`) {
		t.Errorf("%d:\n%s", status, body)
	}
	if status, _ := get(t, "http://"+addr+"/metrics", ""); status != 404 {
		t.Errorf("the API's port answered /metrics with %d", status)
	}
	if status, _ := get(t, "http://"+m+"/v1/models", ""); status != 404 {
		t.Errorf("the metrics port answered /v1/models with %d", status)
	}
	if status, _ := get(t, "http://"+m+"/metrics", "attacker.example"); status != 403 {
		t.Errorf("a rebinding page's Host got %d", status)
	}
	stop()
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
	if conn, err := net.Dial("tcp", m); err == nil {
		conn.Close()
		t.Error("the metrics port is still open after the stop")
	}
}

func TestABusyMetricsPortIsAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, up := upstream(t, nil)
	logs := &syncBuffer{}
	path := writeObservedConfig(t, up.URL, fmt.Sprintf(`"metrics":{"listen":%q}`, ln.Addr()))
	if c := run(context.Background(), []string{"serve", "-config", path}, noEnv, io.Discard, logs, nil); c != 1 ||
		!strings.Contains(logs.String(), "address already in use") || !strings.Contains(logs.String(), `"metrics":true`) {
		t.Errorf("exit %d: %s", c, logs)
	}
}

func TestTheLogSectionSetsTheLevelAndFormat(t *testing.T) {
	_, up := upstream(t, nil)
	for _, tc := range []struct {
		log            string
		chatLine, text bool
	}{
		{`"log":{}`, true, false},
		{`"log":{"level":"warn"}`, false, false},
		{`"log":{"format":"text"}`, true, true},
	} {
		logs := &syncBuffer{}
		addr, stop, code := start(t, writeObservedConfig(t, up.URL, tc.log), logs)
		if status, body, err := chat(addr); err != nil || status != 200 {
			t.Fatalf("%s: %d %s %v", tc.log, status, body, err)
		}
		stop()
		<-code
		out := logs.String()
		chatLine := strings.Contains(out, `"msg":"chat completion"`) || strings.Contains(out, `msg="chat completion"`)
		text := strings.HasPrefix(out, "time=")
		if chatLine != tc.chatLine || text != tc.text {
			t.Errorf("%s: chat line %v, text %v:\n%s", tc.log, chatLine, text, out)
		}
	}
}

func TestCheckPrintsTheLogWhenItIsntTheDefault(t *testing.T) {
	var out strings.Builder
	path := writeObservedConfig(t, "http://h", `"log":{"level":"warn","format":"text"}`)
	if code := run(context.Background(), []string{"check", "-config", path}, noEnv, &out, io.Discard, nil); code != 0 ||
		!strings.HasSuffix(out.String(), "\nlog: text from warn\n") {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
}

func TestTheMetricsAnswerThroughTheDrain(t *testing.T) {
	// The day-4 review's mutant M3: closing the metrics listener at the stop's start passed every test.
	_, up := upstream(t, nil)
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeObservedConfig(t, up.URL,
		`"drain_delay":"1s","metrics":{"listen":"127.0.0.1:0"}`), logs)
	m := metricsAddr(t, logs)
	if status, _, err := chat(addr); err != nil || status != 200 {
		t.Fatalf("%d %v", status, err)
	}
	stop()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"draining"`); {
		if time.Now().After(deadline) {
			t.Fatalf("never drained:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
	if status, body := get(t, "http://"+m+"/metrics", ""); status != 200 ||
		!strings.Contains(body, `switchboard_requests_total{code="",model="fast",status="200"} 1`) {
		t.Errorf("during the drain: %d", status)
	}
	if status, _ := get(t, "http://"+addr+"/readyz", ""); status != 503 {
		t.Errorf("readyz during the drain: %d", status)
	}
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
}

func TestTheMetricsAnswerThroughTheGraceAndShowTheRequestInFlight(t *testing.T) {
	// The day-4 second review's surviving mutant: closing the metrics as the grace began passed, since only the drain
	// was tested. A request held 1 s by its provider keeps the grace going.
	_, up := upstream(t, func(c *mock.Config) { c.TTFT = time.Second })
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeObservedConfig(t, up.URL,
		`"shutdown_grace":"5s","metrics":{"listen":"127.0.0.1:0"}`), logs)
	m := metricsAddr(t, logs)
	answered := make(chan int, 1)
	go func() {
		status, _, _ := chat(addr)
		answered <- status
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, body := get(t, "http://"+m+"/metrics", ""); strings.Contains(body, "switchboard_requests_in_flight 1\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request was never in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"stopping"`); {
		if time.Now().After(deadline) {
			t.Fatalf("never stopped:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // well into the grace
	if status, body := get(t, "http://"+m+"/metrics", ""); status != 200 ||
		!strings.Contains(body, "switchboard_requests_in_flight 1\n") {
		t.Errorf("during the grace: %d", status)
	}
	if status := <-answered; status != 200 {
		t.Errorf("the request in flight got %d", status)
	}
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
}
