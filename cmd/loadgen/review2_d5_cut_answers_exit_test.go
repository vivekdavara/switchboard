package main

// Bug: loadgen's package comment (day-5 review) says "a run in which no request got any HTTP answer (nothing listening,
// every one timed out) exits 1 after its summary ... Answers of any status are results, and exit 0", and CHANGELOG.md
// says "`loadgen` exits 1 when no request got any answer". But the check is len(Summary.Statuses) == 0, and Summarize
// leaves out of Statuses every sample with an Err, including one whose status line and headers did arrive and whose
// body then broke (a stream the provider cut, which the gateway relays as a 200 that breaks off: DESIGN "Streaming",
// mockprovider -cut-rate). So a run whose every answer was a 200 cut short exits 1 and prints "none of the 3 requests
// got an answer", although each got one: a script under set -e (load.sh, chaos.sh) stops on a measurement.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewD5AnswersCutShortAreAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":`))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack() // the answer breaks off after its status line
		conn.Close()
	}))
	defer srv.Close()
	var out, errs bytes.Buffer
	code := run(context.Background(), []string{"-url", srv.URL + "/v1/chat/completions", "-n", "3", "-warmup", "0"},
		func(string) string { return "" }, &out, &errs)
	if code != 0 || strings.Contains(errs.String(), "got an answer") {
		t.Errorf("three 200s cut short: exit %d (answers of any status exit 0):\n%s%s", code, out.String(), errs.String())
	}
}

// A run stopped before its first measured request has no samples: nothing was measured, but nothing failed either, so
// it exits 0 (a surviving mutant of the day-5 second review dropped that case's guard).
func TestAStoppedRunWithNoSamplesExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errs bytes.Buffer
	if code := run(ctx, []string{"-url", srv.URL, "-n", "3", "-warmup", "0"}, func(string) string { return "" }, &out,
		&errs); code != 0 || !strings.Contains(out.String(), "requests 0 in") {
		t.Errorf("exit %d:\n%s%s", code, out.String(), errs.String())
	}
}
