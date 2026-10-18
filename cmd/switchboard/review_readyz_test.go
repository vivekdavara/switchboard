package main

// Found by the day-1 review (at 07b8b14): DESIGN.md said GET /readyz is "503 while shutting down", but serve called
// g.SetReady(false) and srv.Shutdown in the same instant, and Shutdown closes the listener and every idle
// keep-alive connection at once, so no probe could ever see the 503: during the whole grace a GET /readyz got a
// refused connection. A load balancer learned nothing from /readyz that it wouldn't learn from the closed port.
//
// Now drain_delay keeps the gateway serving, with /readyz at 503, for that long before the listener closes; requests
// that still arrive meanwhile are answered.
//
// Run: go test -race -count=1 -run TestReviewReadyzIs503WhileDraining -v ./cmd/switchboard

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReviewReadyzIs503WhileDraining(t *testing.T) {
	_, up := upstream(t, nil)
	logs := &syncBuffer{}
	addr, stop, code := start(t, writeDrainingConfig(t, up.URL, "2s", "1s"), logs)
	began := time.Now()
	stop()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"draining"`); {
		if time.Now().After(deadline) {
			t.Fatalf("no draining line:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := http.Get("http://" + addr + "/readyz")
	switch {
	case err != nil:
		t.Errorf("GET /readyz while draining: %v, want a 503", err)
	case resp.StatusCode != 503:
		t.Errorf("GET /readyz while draining: %d, want 503", resp.StatusCode)
	}
	if resp != nil {
		resp.Body.Close()
	}
	if status, body, err := chat(addr); err != nil || status != 200 {
		t.Errorf("a chat completion while draining: %d %s %v, want it answered", status, body, err)
	}
	if c := <-code; c != 0 {
		t.Errorf("exit %d", c)
	}
	if took := time.Since(began); took < time.Second {
		t.Errorf("stopped after %v, before the 1s drain", took)
	}
	if _, _, err := chat(addr); err == nil {
		t.Error("still answering after the drain and the stop")
	}
}
