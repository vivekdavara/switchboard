package main

// Bug: config refuses a metrics.listen only when it is the same string as listen (DESIGN.md: "A metrics.listen equal
// to the API's listen is refused at load"). "listen": ":P" with "metrics": {"listen": "127.0.0.1:P"} is the same port
// and passes; on macOS (BSD sockets, SO_REUSEADDR, which Go sets) both binds succeed, and the more specific loopback
// listener takes every local connection to the API's port: chat completions sent to 127.0.0.1:P get the metrics
// handler's 404 while the gateway logs "listening" as if all were well. (On Linux the second bind fails with
// EADDRINUSE and serve exits 1, which this test accepts.)

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewD4TheMetricsListenerCantShadowTheAPIsPort(t *testing.T) {
	_, up := upstream(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"listen":":%s","metrics":{"listen":"127.0.0.1:%s"},
		"providers":[{"name":"mock-a","base_url":%q,"timeout":"10s"}],
		"models":[{"name":"fast","provider":"mock-a","upstream_model":"mock-small"}]}`, port, port, up.URL+"/v1")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening := make(chan string, 1)
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"serve", "-config", path}, noEnv, io.Discard, io.Discard, listening) }()
	select {
	case c := <-code:
		if c == 0 {
			t.Errorf("exit 0 before serving")
		}
		return // refused (at load, or by the second bind): fine
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("never listened")
	}
	defer func() { cancel(); <-code }()
	if status, body, err := chat("127.0.0.1:" + port); err != nil || status != 200 {
		t.Errorf("a chat completion to the API's port on 127.0.0.1: %d %q %v", status, body, err)
	}
}
