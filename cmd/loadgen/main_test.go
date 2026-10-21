package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestLoadgenPrintsASummaryAndATimeline(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sb-test-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("X-Switchboard-Provider", "mock-a")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	env := func(k string) string {
		if k == "SB_KEY" {
			return "sb-test-key"
		}
		return ""
	}
	code := run(context.Background(), []string{"-url", srv.URL, "-n", "40", "-warmup", "4", "-c", "4", "-unique",
		"-model", "smart", "-key-env", "SB_KEY", "-timeline", "1h"}, env, &out, io.Discard)
	if code != 0 || !strings.HasPrefix(out.String(), "requests 40 in ") || !strings.Contains(out.String(), "; 200: 40\n") ||
		!strings.Contains(out.String(), "latency p50 ") || !strings.Contains(out.String(), "  mock-a 40\n") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	distinct := map[string]bool{}
	for _, b := range bodies {
		var req struct{ Model string }
		json.Unmarshal([]byte(b), &req)
		if req.Model != "smart" {
			t.Fatalf("body %s", b)
		}
		distinct[b] = true
	}
	if len(bodies) != 44 || len(distinct) != 44 {
		t.Errorf("%d bodies, %d distinct; want 44 of each with -unique", len(bodies), len(distinct))
	}

	out.Reset()
	if code := run(context.Background(), []string{"-url", srv.URL, "-n", "5", "-warmup", "0", "-json"}, env, &out,
		io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var s struct {
		Requests int
		Statuses map[string]int
	}
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || s.Requests != 5 || s.Statuses["401"] != 5 {
		t.Errorf("%v %s", err, out.String())
	}
}

func TestLoadgenRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{{"-n", "0"}, {"-c", "0"}, {"-rate", "-1"}, {"extra"}, {"-key-env", "UNSET"},
		{"-bogus"}} {
		if code := run(context.Background(), args, func(string) string { return "" }, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
