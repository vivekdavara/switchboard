// Command mockprovider runs a mock OpenAI-compatible provider (internal/mock) until SIGINT or SIGTERM.
//
//	mockprovider [-addr 127.0.0.1:8118] [-name mock] [-models mock-small,mock-large]
//	             [-ttft 200ms] [-per-token 5ms] [-jitter 0] [-completion-tokens 32]
//	             [-error-rate 0] [-error-status 500] [-hang-rate 0] [-cut-rate 0] [-seed 1] [-api-key-env NAME]
//
// It serves POST /v1/chat/completions (streamed as server-sent events with "stream": true), GET /v1/models and
// GET /livez. With -api-key-env it requires
// "Authorization: Bearer" with that environment variable's value. Logs are JSON lines on stderr: one when it
// listens, one when it stops, with its counts. A usage error exits 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vivekdavara/switchboard/internal/mock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stderr, nil))
}

// shutdownGrace is how long requests in flight get after a stop: none are held (Close drops those), so this is
// only for answers being written.
const shutdownGrace = 5 * time.Second

// started is what run tells a test once its port is open.
type started struct {
	Addr  string
	Stats func() mock.Stats
}

// run is main without the process: it serves until ctx is done and returns the exit code. If listening isn't nil,
// it receives the address and the provider's counts once the port is open.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer,
	listening chan<- started) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg := mock.Defaults()
	fs := flag.NewFlagSet("mockprovider", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8118", "address to listen on")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "the provider's name, in its answers")
	models := fs.String("models", strings.Join(cfg.Models, ","), "comma-separated models it serves; empty: any")
	fs.DurationVar(&cfg.TTFT, "ttft", cfg.TTFT, "time to first token")
	fs.DurationVar(&cfg.PerToken, "per-token", cfg.PerToken, "time per token written")
	fs.Float64Var(&cfg.Jitter, "jitter", cfg.Jitter, "scale each delay by a random factor in [1-j, 1+j]")
	fs.IntVar(&cfg.CompletionTokens, "completion-tokens", cfg.CompletionTokens, "tokens (words) per answer")
	fs.Float64Var(&cfg.ErrorRate, "error-rate", cfg.ErrorRate, "fraction of requests answered with -error-status")
	fs.IntVar(&cfg.ErrorStatus, "error-status", cfg.ErrorStatus, "the status of an injected error")
	fs.Float64Var(&cfg.HangRate, "hang-rate", cfg.HangRate, "fraction of requests never answered")
	fs.Float64Var(&cfg.CutRate, "cut-rate", cfg.CutRate, "fraction of answers that break off part-way")
	fs.Uint64Var(&cfg.Seed, "seed", cfg.Seed, "the seed of the fates")
	keyEnv := fs.String("api-key-env", "", "require this environment variable's value as a bearer key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mockprovider: unexpected arguments: %q\n", fs.Args())
		return 2
	}
	cfg.Models = nil
	for _, m := range strings.Split(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			cfg.Models = append(cfg.Models, m)
		}
	}
	if *keyEnv != "" {
		if cfg.APIKey = getenv(*keyEnv); cfg.APIKey == "" {
			log.Error("the API key's environment variable is empty or unset", "api_key_env", *keyEnv)
			return 1
		}
	}
	p, err := mock.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "mockprovider:", err)
		return 2
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "name", cfg.Name, "models", cfg.Models, "ttft",
		cfg.TTFT.String(), "per_token", cfg.PerToken.String(), "jitter", cfg.Jitter, "error_rate", cfg.ErrorRate,
		"error_status", cfg.ErrorStatus, "hang_rate", cfg.HangRate, "cut_rate", cfg.CutRate, "seed", cfg.Seed,
		"api_key", cfg.APIKey != "")
	if listening != nil {
		listening <- started{Addr: ln.Addr().String(), Stats: p.Stats}
	}

	select {
	case err := <-served: // Serve only returns early on a listener failure
		log.Error("serve", "err", err)
		return 1
	case <-ctx.Done():
	}
	p.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = srv.Shutdown(shutdown)
	srv.Close()
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
	}
	s := p.Stats()
	log.Info("stopped", "requests", s.Requests, "refused", s.Refused, "failed", s.Failed, "hung", s.Hung,
		"answered", s.Answered, "streamed", s.Streamed, "cut", s.Cut, "abandoned", s.Abandoned, "clean", err == nil)
	return 0
}
