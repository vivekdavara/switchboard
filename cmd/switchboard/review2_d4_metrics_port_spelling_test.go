package main

// Bug (the day-4 review's fix 5, incomplete): config refuses a metrics.listen whose port is the API's by comparing the
// two port strings, but net.Listen reads a port as a number, so the same port spelled another way passes: "listen":
// ":P" with "metrics": {"listen": "127.0.0.1:0P"} (a leading zero; "+P" too) is accepted. On macOS both binds
// succeed and the more specific loopback listener takes every local connection to the API's port: chat completions
// sent to 127.0.0.1:P get the metrics handler's 404, exactly the bug the fix was for. (On Linux the second bind fails
// with EADDRINUSE and serve exits 1, which this test accepts, as the first review's test does.)

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

func TestReview2D4TheMetricsPortCantShadowTheAPIsSpelledAnotherWay(t *testing.T) {
	_, up := upstream(t, nil)
	for _, spell := range []string{"0%s", "+%s"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		ln.Close()
		metricsPort := fmt.Sprintf(spell, port)
		path := filepath.Join(t.TempDir(), "config.json")
		body := fmt.Sprintf(`{"listen":":%s","metrics":{"listen":"127.0.0.1:%s"},
		"providers":[{"name":"mock-a","base_url":%q,"timeout":"10s"}],
		"models":[{"name":"fast","provider":"mock-a","upstream_model":"mock-small"}]}`, port, metricsPort, up.URL+"/v1")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listening := make(chan string, 1)
			code := make(chan int, 1)
			go func() { code <- run(ctx, []string{"serve", "-config", path}, noEnv, io.Discard, io.Discard, listening) }()
			select {
			case c := <-code:
				if c == 0 {
					t.Errorf("%s: exit 0 before serving", metricsPort)
				}
				return // refused (at load, or by the second bind): fine
			case <-listening:
			case <-time.After(10 * time.Second):
				t.Fatal("never listened")
			}
			defer func() { cancel(); <-code }()
			if status, body, err := chat("127.0.0.1:" + port); err != nil || status != 200 {
				t.Errorf("metrics on 127.0.0.1:%s, a chat completion to the API's port %s: %d %q %v", metricsPort, port,
					status, body, err)
			}
		}()
	}
}
