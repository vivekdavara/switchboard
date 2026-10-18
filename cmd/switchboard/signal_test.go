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

// TestASecondSignalEndsTheProcessAtOnce runs the real binary: the first signal starts a 30 s drain, and a second one
// during it, of either kind, ends the process within seconds, by that signal: SIGINT after SIGTERM, and the same
// signal again (an operator's second Ctrl-C, a supervisor's second SIGTERM) once the first was taken. (Found as a
// suspicion in the day-1 review: main's NotifyContext kept catching signals until main returned, so a second Ctrl-C
// did nothing.) The same signal twice back to back is another matter: the kernel and Go's runtime keep one pending
// flag per signal number, so the two can merge into one before any code sees them (the third review: 10 of 10).
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
	for _, tc := range []struct {
		name          string
		first, second syscall.Signal
	}{
		{"SIGINT after SIGTERM", syscall.SIGTERM, syscall.SIGINT},
		{"a second Ctrl-C", syscall.SIGINT, syscall.SIGINT},
		{"a second SIGTERM", syscall.SIGTERM, syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) { secondSignal(t, bin, cfg, tc.first, tc.second) })
	}
}

// secondSignal starts the binary, sends first, waits for the drain to start, sends second, and expects the process
// to die by second within 5 s.
func secondSignal(t *testing.T, bin, cfg string, first, second syscall.Signal) {
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
	cmd.Process.Signal(first)
	waitForLine("draining")
	cmd.Process.Signal(second)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() ||
			exit.Sys().(syscall.WaitStatus).Signal() != second {
			t.Errorf("the process ended with %v, want killed by %v", err, second)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal didn't end the process; it is still draining")
	}
}

// A process started with SIGINT ignored (nohup, or `cmd &` in a script) still gets its first SIGINT, because
// signal.Notify takes it over; but once main hands signals back, SIGINT is ignored again, so raising the second one
// does nothing, and main exits 130 a second later instead. (The day-1 third review found that path untested: with
// the exit changed to 0, every test passed.)
func TestASecondSIGINTWhenItWasIgnoredExits130(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "switchboard")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:0","drain_delay":"30s",`+
		`"providers":[{"name":"p","base_url":"http://127.0.0.1:1/v1"}],"models":[{"name":"m","provider":"p"}]}`),
		0o600)
	// The shell ignores SIGINT, then execs the gateway in its place (same process), which inherits the ignoring.
	cmd := exec.Command("sh", "-c", `trap "" INT; exec "$0" serve -config "$1"`, bin, cfg)
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
		for deadline := time.After(10 * time.Second); ; {
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
	cmd.Process.Signal(syscall.SIGINT)
	waitForLine("draining")
	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signaled() || exit.ExitCode() != 130 {
			t.Errorf("the process ended with %v, want exit status 130", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second SIGINT didn't end the process; it is still draining")
	}
}
