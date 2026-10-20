// Command switchboard runs the gateway.
//
//	switchboard serve -config FILE   serve until SIGINT or SIGTERM
//	switchboard check -config FILE   load and check the configuration, print the routes, and exit
//	switchboard keygen               print a new client key and the SHA-256 the configuration holds for it
//	switchboard usage -config FILE [-day YYYY-MM-DD]
//	                                 print a day's totals (UTC; today by default) per client and model from Redis
//	switchboard version              print the version, the commit and the Go release, and exit
//
// The configuration is a JSON file (DESIGN.md, "Configuration"); provider keys come from the environment variables
// it names. Logs are JSON lines on stderr: one per chat completion, one per request refused for its Host, and the
// server's own. A usage error exits 2; a configuration that doesn't load or a port that won't open exits 1; a stop by
// SIGINT or SIGTERM exits 0, whether or not every request finished in the grace, and a second signal during the stop
// ends the process at once, by that signal (or with exit status 130 if SIGINT was ignored when it started). Two
// identical signals sent back to back can merge into one before any code sees them; DESIGN.md has why.
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
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/gateway"
	"github.com/vivekdavara/switchboard/internal/usage"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel() // the first signal starts the stop
		// A second one ends the process at once, mid-drain or mid-grace, as an operator pressing Ctrl-C twice
		// expects. Notification stays on until it comes, so it can't be lost: handing signals back to Go's default
		// handling at the first one, as an earlier fix did, dropped a second one still pending inside the runtime
		// (the second review sent them back to back: 10 of 10 swallowed, and 3 of 10 after a first repair). Raise
		// it again with the default handling, which kills the process; exit 130 if it was ignored when we started.
		// (The same signal twice back to back can still count as one: the kernel and the runtime keep one pending
		// flag per signal number. A second signal sent after the first was taken always arrives here.)
		sig := <-sigs
		signal.Reset(os.Interrupt, syscall.SIGTERM)
		// os.Process.Signal, not syscall.Kill: it builds on every platform (syscall.Kill broke the Windows build;
		// there, where a process can't signal itself an interrupt, the exit below ends it).
		if self, err := os.FindProcess(os.Getpid()); err == nil {
			self.Signal(sig)
		}
		time.Sleep(time.Second)
		os.Exit(130)
	}()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil))
}

const usageLine = "usage: switchboard serve -config FILE | check -config FILE | keygen | " +
	"usage -config FILE [-day YYYY-MM-DD] | version"

// Server timeouts (DESIGN.md, "Timeouts and shutdown"). No WriteTimeout: a streamed answer can rightly take minutes.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	// finishWriting is how long requests cancelled at the end of the grace get to write their 503s before the
	// connections close.
	finishWriting = time.Second
)

// run is main without the process, so tests can drive it: it returns the exit code, and stops serving when ctx is
// cancelled. If listening isn't nil it receives the address once the port is open.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer,
	listening chan<- string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	switch args[0] {
	case "version", "keygen":
		if len(args) != 1 {
			fmt.Fprintln(stderr, usageLine)
			return 2
		}
		if args[0] == "keygen" {
			return keygen(stdout)
		}
		fmt.Fprintln(stdout, version())
		return 0
	case "serve", "check", "usage":
	default:
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	fs := flag.NewFlagSet("switchboard "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the configuration file (JSON)")
	var day *string
	if args[0] == "usage" {
		day = fs.String("day", time.Now().UTC().Format(time.DateOnly), "the day (UTC) to report")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	cfg, err := config.Load(*path, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "switchboard:", err)
		return 1
	}
	switch args[0] {
	case "check":
		printRoutes(stdout, cfg)
		return 0
	case "usage":
		return report(ctx, cfg, *day, stdout, stderr)
	}
	return serve(ctx, cfg, slog.New(slog.NewJSONHandler(stderr, nil)), listening)
}

func printRoutes(w io.Writer, cfg *config.Config) {
	fmt.Fprintf(w, "listen %s, drain %s, shutdown grace %s, request limit %d bytes, response limit %d bytes\n",
		cfg.Listen, cfg.DrainDelay, cfg.ShutdownGrace, cfg.MaxRequestBytes, cfg.MaxResponseBytes)
	for _, p := range cfg.Providers {
		key := "no key"
		if p.APIKeyEnv != "" {
			key = "key from " + p.APIKeyEnv
		}
		fmt.Fprintf(w, "provider %s: %s, timeout %s, %s\n", p.Name, p.BaseURL, p.Timeout, key)
	}
	fmt.Fprintf(w, "retry budget %g a request + %g a second; circuit breaker after %d failures, cooldown %s\n",
		cfg.RetryBudget.Ratio, cfg.RetryBudget.MinPerSecond, cfg.Breaker.Failures, cfg.Breaker.Cooldown)
	for _, m := range cfg.Models {
		var targets []string
		for _, t := range m.Targets {
			targets = append(targets, fmt.Sprintf("%s as %s (weight %d)", t.Provider.Name, t.UpstreamModel, t.Weight))
		}
		aliases := ""
		if len(m.Aliases) > 0 {
			aliases = " (also " + strings.Join(m.Aliases, ", ") + ")"
		}
		attempts := "no fallback"
		if m.MaxAttempts > 1 {
			attempts = fmt.Sprintf("up to %d attempts", m.MaxAttempts)
		}
		cache := ""
		if m.Cache != nil {
			cache = fmt.Sprintf("; cached for %s", m.Cache.TTL)
			if m.Cache.Shared {
				cache += ", shared by every client"
			}
		}
		fmt.Fprintf(w, "model %s%s -> %s; %s%s\n", m.Name, aliases, strings.Join(targets, ", "), attempts, cache)
	}
	printDay3(w, cfg)
}

// printDay3 prints Redis, the clients and the prices (day 3), when the file has them.
func printDay3(w io.Writer, cfg *config.Config) {
	if r := cfg.Redis; r != nil {
		onError := "allow"
		if r.RefuseWhenDown {
			onError = "refuse"
		}
		fmt.Fprintf(w, "redis %s, prefix %q, timeout %s, rate-limited requests when it is down: %s\n", r.Addr, r.Prefix,
			r.Timeout, onError)
	}
	if len(cfg.Clients) == 0 {
		fmt.Fprintln(w, "no clients: no key is asked for")
	}
	for _, c := range cfg.Clients {
		limit := "no rate limit"
		if l := c.RateLimit; l != nil {
			limit = fmt.Sprintf("%g requests a minute, %d at once", l.PerMinute, l.Burst)
		}
		fmt.Fprintf(w, "client %s: %s\n", c.Name, limit)
	}
	var priced []string
	for k := range cfg.Prices() {
		priced = append(priced, k[0]+"\x00"+k[1])
	}
	slices.Sort(priced)
	for _, k := range priced {
		provider, model, _ := strings.Cut(k, "\x00")
		p, _ := cfg.Price(provider, model)
		fmt.Fprintf(w, "price %s %s: $%s in, $%s out per million tokens\n", provider, model,
			usage.USD(p.Input*1_000_000), usage.USD(p.Output*1_000_000))
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, listening chan<- string) int {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	g := gateway.New(cfg, nil, log)
	defer g.Close()
	// Every request's context derives from base, so cancelling it (with gateway.ErrShutdown as the cause) cancels
	// every request still running when the grace is over, and their upstream requests with them.
	base, cancelAll := context.WithCancelCause(context.Background())
	defer cancelAll(nil)
	srv := newServer(g, base, log)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	g.SetReady(true)
	providers := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		providers[i] = p.Name
	}
	models := make([]string, len(cfg.Models))
	for i, m := range cfg.Models {
		models[i] = m.Name
	}
	log.Info("listening", "addr", ln.Addr().String(), "providers", providers, "models", models)
	if listening != nil {
		listening <- ln.Addr().String()
	}

	select {
	case err := <-served: // Serve returns early only if the listener fails
		log.Error("serve", "err", err)
		return 1
	case <-ctx.Done():
	}
	// /readyz turns 503 first. Shutdown closes the listener at once, so nobody could see that 503 unless the
	// gateway keeps serving a while: drain_delay is that while (found in the day-1 review: with the listener
	// closing in the same instant, a probe only ever got "connection refused").
	g.SetReady(false)
	if cfg.DrainDelay > 0 {
		log.Info("draining", "delay", cfg.DrainDelay.String())
		time.Sleep(cfg.DrainDelay)
	}
	log.Info("stopping", "grace", cfg.ShutdownGrace.String())
	grace, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	// Shutdown closes the listener and waits for requests in flight, but returns at its deadline with handlers
	// still running. Then cancel them (each writes a 503 if its client is still there), give them a moment to
	// write it, and close every connection.
	clean := srv.Shutdown(grace) == nil
	if !clean {
		cancelAll(gateway.ErrShutdown)
		finish, cancel := context.WithTimeout(context.Background(), finishWriting)
		defer cancel()
		srv.Shutdown(finish)
		srv.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
	}
	log.Info("stopped", "clean", clean)
	return 0
}

// newServer is the HTTP server serve runs: its timeouts, and every request's context derived from base.
func newServer(h http.Handler, base context.Context, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// version says which switchboard this is: the module version the go command stamps from git (a tag such as v1.0.0
// when built at it, a pseudo-version after it, "(devel)" for go run and go test), the commit, and the Go release.
func version() string {
	v, commit := "(devel)", "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				commit = s.Value[:12]
			}
		}
	}
	return fmt.Sprintf("switchboard %s (commit %s, %s %s/%s)", v, commit, runtime.Version(), runtime.GOOS,
		runtime.GOARCH)
}
