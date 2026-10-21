// Command loadgen sends chat completions to a gateway (or straight to a provider) and prints what came back: the
// throughput, the statuses and transport errors, latency percentiles, and optionally a timeline (day 4; DESIGN.md,
// "Load and chaos (day 4)").
//
//	loadgen [-url URL] [-model NAME] [-n N] [-warmup N] [-c N] [-rate R] [-stream] [-unique] [-key-env NAME]
//	        [-timeline 1s] [-timeline-header H] [-timeout D] [-json]
//
// With -rate 0 (the default) it runs a closed loop: -c workers, each sending as soon as its last answer is in. With
// -rate R it runs an open loop: a request is due every 1/R seconds and its latency counts from then, so a stall
// shows in every request it delays (at most -c in flight; a request waiting for a worker is waiting). A client key
// comes from the environment variable -key-env names, never from the command line.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/vivekdavara/switchboard/internal/load"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:8108/v1/chat/completions", "where to POST the chat completions")
	model := fs.String("model", "fast", "the model to ask for")
	n := fs.Int("n", 10000, "requests to measure")
	warmup := fs.Int("warmup", 500, "requests to send first, unmeasured")
	c := fs.Int("c", 32, "at most this many requests in flight (the closed loop's workers)")
	rate := fs.Float64("rate", 0, "requests a second (an open loop); 0 for a closed loop")
	stream := fs.Bool("stream", false, "ask for streamed answers")
	unique := fs.Bool("unique", false, "make every request's message different, so no cache can answer it")
	keyEnv := fs.String("key-env", "", "the environment variable holding the client key (sent as a Bearer token)")
	timeout := fs.Duration("timeout", 30*time.Second, "a request's limit")
	interval := fs.Duration("timeline", 0, "print a timeline in steps of this (0 for none)")
	header := fs.String("timeline-header", "X-Switchboard-Provider", "count this answer header's values per step")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *n < 1 || *c < 1 || *warmup < 0 || *rate < 0 {
		fmt.Fprintln(stderr, "loadgen: -n and -c must be at least 1, -warmup and -rate at least 0, and no arguments")
		return 2
	}
	h := http.Header{}
	if *keyEnv != "" {
		key := getenv(*keyEnv)
		if key == "" {
			fmt.Fprintf(stderr, "loadgen: %s is unset or empty\n", *keyEnv)
			return 2
		}
		h.Set("Authorization", "Bearer "+key)
	}
	body := func(i int) []byte {
		content := "What is two plus two?"
		if *unique {
			content = fmt.Sprintf("Request %d: what is two plus two?", i)
		}
		b, _ := json.Marshal(map[string]any{"model": *model, "stream": *stream,
			"messages": []map[string]string{{"role": "user", "content": content}}})
		return b
	}
	res, err := load.Run(ctx, load.Options{URL: *url, Body: body, Header: h, Requests: *n, Warmup: *warmup,
		Concurrency: *c, Rate: *rate, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	s := load.Summarize(res.Samples, res.Elapsed)
	if *asJSON {
		json.NewEncoder(stdout).Encode(jsonSummary(s, res.Elapsed))
	} else {
		printSummary(stdout, s, res.Elapsed)
	}
	if *interval > 0 {
		printTimeline(stdout, load.Timeline(res.Samples, *interval, *header), *header)
	}
	return 0
}

func ms(d time.Duration) string { return fmt.Sprintf("%.3fms", float64(d.Microseconds())/1000) }

func printSummary(w io.Writer, s load.Summary, elapsed time.Duration) {
	var statuses []string
	for _, code := range sortedKeys(s.Statuses) {
		statuses = append(statuses, fmt.Sprintf("%d: %d", code, s.Statuses[code]))
	}
	for _, e := range sortedKeys(s.Errors) {
		statuses = append(statuses, fmt.Sprintf("%s: %d", e, s.Errors[e]))
	}
	fmt.Fprintf(w, "requests %d in %s: %.1f/s; %s\n", s.Requests, elapsed.Round(time.Millisecond), s.PerSecond,
		strings.Join(statuses, ", "))
	fmt.Fprintf(w, "latency p50 %s, p90 %s, p99 %s, p99.9 %s, max %s; first byte p50 %s\n", ms(s.P50), ms(s.P90),
		ms(s.P99), ms(s.P999), ms(s.Max), ms(s.FirstByte))
}

func printTimeline(w io.Writer, steps []load.Step, header string) {
	fmt.Fprintf(w, "%8s %7s %7s %7s %11s %11s  %s\n", "from", "sent", "ok", "failed", "p50", "p99", header)
	for _, st := range steps {
		var values []string
		for _, v := range sortedKeys(st.Header) {
			name := v
			if name == "" {
				name = "-"
			}
			values = append(values, fmt.Sprintf("%s %d", name, st.Header[v]))
		}
		fmt.Fprintf(w, "%7.1fs %7d %7d %7d %11s %11s  %s\n", st.From.Seconds(), st.Requests, st.OK, st.Failed,
			ms(st.P50), ms(st.P99), strings.Join(values, ", "))
	}
}

func jsonSummary(s load.Summary, elapsed time.Duration) map[string]any {
	us := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }
	statuses := map[string]int{}
	for code, n := range s.Statuses {
		statuses[fmt.Sprint(code)] = n
	}
	return map[string]any{"requests": s.Requests, "elapsed_s": elapsed.Seconds(), "per_second": s.PerSecond,
		"statuses": statuses, "errors": s.Errors, "p50_us": us(s.P50), "p90_us": us(s.P90), "p99_us": us(s.P99),
		"p999_us": us(s.P999), "max_us": us(s.Max), "first_byte_p50_us": us(s.FirstByte)}
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
