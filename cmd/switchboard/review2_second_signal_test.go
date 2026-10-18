package main

// Found by the day-1 second review (at 92f8214): a second signal that arrived right after the first was swallowed,
// so the process drained and waited out its grace anyway. main restored the default handling from a goroutine that
// woke on ctx.Done(), in no order with serve, which wakes on the same ctx.Done() and logs "draining"; until
// signal.Stop had returned, a signal went to NotifyContext's full, unread channel and was dropped. Measured with the
// real binary and a 30 s drain: SIGINT sent right after SIGTERM left it draining 5 of 5 times. The same race made
// TestASecondSignalEndsTheProcessAtOnce flaky (CI run 39161463852 caught it too).
//
// A first repair (reset the handling, then cancel) still swallowed it here 10 of 10 times with both signals sent
// back to back, and 3 of 10 with GOMAXPROCS=2 after buffering two: a signal still pending inside the runtime when
// signal.Reset runs is dropped. Now notification stays on until the second signal arrives, which is then raised
// again with the default handling.
//
// Run: go test -race -count=1 -run TestReview2ASecondSignalRightAfterTheFirstEndsTheProcess -v ./cmd/switchboard

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReview2ASecondSignalRightAfterTheFirstEndsTheProcess(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "switchboard")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:0","drain_delay":"30s",`+
		`"providers":[{"name":"p","base_url":"http://127.0.0.1:1/v1"}],"models":[{"name":"m","provider":"p"}]}`),
		0o600)
	cmd := exec.Command(bin, "serve", "-config", cfg)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	listening := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"msg":"listening"`) {
				close(listening)
			}
		}
	}()
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("never listened")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Process.Signal(syscall.SIGINT) // an operator's second Ctrl-C, or a supervisor's second signal, right away
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
			t.Errorf("the process ended with %v, want killed by the second signal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal was swallowed: the process is still in its 30 s drain")
	}
}
