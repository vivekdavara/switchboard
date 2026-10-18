package main

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

// TestASecondSignalEndsTheProcessAtOnce runs the real binary: the first SIGTERM starts a 30 s drain, and a SIGINT
// during it ends the process within seconds, by the signal. (Found as a suspicion in the day-1 review: main's
// NotifyContext kept catching signals until main returned, so a second Ctrl-C did nothing.)
func TestASecondSignalEndsTheProcessAtOnce(t *testing.T) {
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
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitForLine := func(msg string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the process ended before logging %s", msg)
				}
				if strings.Contains(l, `"msg":"`+msg+`"`) {
					return
				}
			case <-deadline:
				t.Fatalf("no %s line", msg)
			}
		}
	}
	waitForLine("listening")
	cmd.Process.Signal(syscall.SIGTERM)
	waitForLine("draining")
	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() ||
			exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
			t.Errorf("the process ended with %v, want killed by SIGINT", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal didn't end the process; it is still draining")
	}
}
