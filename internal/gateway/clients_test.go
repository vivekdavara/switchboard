package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/redistest"
)

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// clients is a config's clients, each "name" or "name:requests_per_minute:burst", with the key "sb-<name>-key".
func clients(list ...string) string {
	var out []string
	for _, c := range list {
		name, limit, limited := strings.Cut(c, ":")
		entry := fmt.Sprintf(`{"name":%q,"key_sha256":%q`, name, keyHash("sb-"+name+"-key"))
		if limited {
			rate, burst, _ := strings.Cut(limit, ":")
			entry += fmt.Sprintf(`,"rate_limit":{"requests_per_minute":%s,"burst":%s}`, rate, burst)
		}
		out = append(out, entry+"}")
	}
	return `"clients":[` + strings.Join(out, ",") + `]`
}

// testRedis is a config's redis, the test Redis under a prefix of the test's own; extra is spliced into it.
func testRedis(t *testing.T, extra string) string {
	prefix := redistest.Prefix(t)
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`"redis":{"addr":%q,"prefix":%q%s}`, redistest.Addr(t), prefix, extra)
}

func as(name string) []string { return []string{"Authorization", "Bearer sb-" + name + "-key"} }

// logLines returns the log's lines with the given message.
func logLines(t *testing.T, logs *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		if line["msg"] == msg {
			out = append(out, line)
		}
	}
	return out
}

func TestWithClientsEveryAPIPathNeedsAKey(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, g, logs := gatewayFor(t, clients("alice", "bob"), []string{at(up, `"api_key_env":"P_KEY"`)},
		"fast=p0/mock-small")
	g.SetReady(true)
	for _, tc := range []struct {
		name, auth, code string
	}{
		{"no key", "", "missing_api_key"},
		{"an unknown key", "Bearer sb-carol-key", "invalid_api_key"},
		{"a key with a space inside", "Bearer sb-alice -key", "invalid_api_key"},
		{"an empty token", "Bearer ", "missing_api_key"},
		{"another scheme", "Basic sb-alice-key", "missing_api_key"},
		{"no scheme", "sb-alice-key", "missing_api_key"},
		{"alice's key's hash", "Bearer " + keyHash("sb-alice-key"), "invalid_api_key"},
	} {
		resp, b := send(t, srv, ask, "Authorization", tc.auth)
		if _, code, _, _ := errorOf(t, b); resp.StatusCode != 401 || code != tc.code ||
			resp.Header.Get("WWW-Authenticate") != `Bearer realm="switchboard"` {
			t.Errorf("%s: %d %s %q", tc.name, resp.StatusCode, b, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("the provider was asked %d times", n)
	}

	// A space after the key is no part of the header's value (RFC 9110; net/http trims it).
	for _, auth := range []string{"Bearer sb-alice-key", "bearer sb-alice-key", "BEARER sb-bob-key "} {
		resp, b := send(t, srv, ask, "Authorization", auth)
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", auth, resp.StatusCode, b)
		}
	}
	for _, r := range up.requests() {
		if got := r.header.Get("Authorization"); got != "Bearer sk-provider-key-for-tests" {
			t.Errorf("upstream Authorization %q: the provider gets its own key, never the client's", got)
		}
	}

	// Every path but the health endpoints: the models, and paths that don't exist (a 401, not a 404, so a client
	// without a key learns nothing about what is served).
	get := func(path, auth string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for path, want := range map[string][2]int{
		"/v1/models": {401, 200}, "/v1/models/fast": {401, 200}, "/v1/nothing": {401, 404},
		"/livez": {200, 200}, "/readyz": {200, 200},
	} {
		if got := [2]int{get(path, ""), get(path, "Bearer sb-alice-key")}; got != want {
			t.Errorf("GET %s without a key and with one: %v, want %v", path, got, want)
		}
	}

	lines := logLines(t, logs, "chat completion")
	if len(lines) != 3 || lines[0]["client"] != "alice" || lines[2]["client"] != "bob" {
		t.Errorf("chat completion lines: %v", lines)
	}
	refused := logLines(t, logs, "client key refused")
	if len(refused) != 10 || refused[0]["reason"] != "missing" || refused[1]["reason"] != "unknown" {
		t.Errorf("refusals logged: %v", refused)
	}
	for _, secret := range []string{"sb-alice-key", "sb-bob-key", "sb-carol-key"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log has %q", secret)
		}
	}
}

func TestWithoutClientsNoKeyIsAskedFor(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
	for _, auth := range []string{"", "Bearer anything", "Basic x"} {
		if resp, b := send(t, srv, ask, "Authorization", auth); resp.StatusCode != 200 {
			t.Errorf("%q: %d %s", auth, resp.StatusCode, b)
		}
	}
	if line := logLines(t, logs, "chat completion")[0]; line["client"] != nil {
		t.Errorf("no client in the log without clients: %v", line)
	}
}

func TestARateLimitedClientGetsA429ItsSDKWaitsFor(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, testRedis(t, "")+","+clients("alice:60:2", "bob:60:1", "carol"),
		[]string{at(up, "")}, "fast=p0/mock-small")
	for i, want := range []string{"1", "0"} {
		resp, b := send(t, srv, ask, as("alice")...)
		if resp.StatusCode != 200 || resp.Header.Get("X-Ratelimit-Limit-Requests") != "2" ||
			resp.Header.Get("X-Ratelimit-Remaining-Requests") != want {
			t.Fatalf("request %d: %d %v %s", i+1, resp.StatusCode, resp.Header, b)
		}
	}
	began := time.Now()
	resp, b := send(t, srv, ask, as("alice")...)
	typ, code, _, msg := errorOf(t, b)
	if resp.StatusCode != 429 || typ != "rate_limit_error" || code != "rate_limit_exceeded" ||
		!strings.Contains(msg, "alice") {
		t.Fatalf("the third: %d %s", resp.StatusCode, b)
	}
	// One request comes back a second after it was taken; some of that second has passed.
	ms, err := strconv.Atoi(resp.Header.Get("Retry-After-Ms"))
	if err != nil || ms > 1000 || ms < 1000-int(time.Since(began).Milliseconds())-100 {
		t.Errorf("Retry-After-Ms %q", resp.Header.Get("Retry-After-Ms"))
	}
	if resp.Header.Get("Retry-After") != "1" || resp.Header.Get("X-Ratelimit-Remaining-Requests") != "0" {
		t.Errorf("headers: %v", resp.Header)
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("the provider was asked %d times, not 2", n)
	}

	// Each client has its own bucket, and a client with no limit has none.
	if resp, _ := send(t, srv, ask, as("bob")...); resp.StatusCode != 200 {
		t.Errorf("bob: %d", resp.StatusCode)
	}
	if resp, _ := send(t, srv, ask, as("bob")...); resp.StatusCode != 429 {
		t.Errorf("bob's second: %d", resp.StatusCode)
	}
	for range 5 {
		if resp, _ := send(t, srv, ask, as("carol")...); resp.StatusCode != 200 ||
			resp.Header.Get("X-Ratelimit-Limit-Requests") != "" {
			t.Errorf("carol: %d %v", resp.StatusCode, resp.Header)
		}
	}
	// A second later alice has one request again.
	time.Sleep(time.Duration(ms+20) * time.Millisecond)
	if resp, b := send(t, srv, ask, as("alice")...); resp.StatusCode != 200 {
		t.Errorf("alice, a second later: %d %s", resp.StatusCode, b)
	}
	lines := logLines(t, logs, "chat completion")
	if lines[2]["rate_limit"] != "refused" || lines[2]["status"] != 429.0 || lines[2]["code"] != "rate_limit_exceeded" ||
		lines[0]["rate_limit"] != nil {
		t.Errorf("log: %v", lines[:3])
	}
}

func TestTheLimitCountsRequestsNotAttempts(t *testing.T) {
	bad := newProvider(t, answering(500, `{"error":{"message":"down","type":"server_error"}}`))
	good := newProvider(t, mockProvider(t, "mock-b"))
	srv, _, _ := gatewayFor(t, testRedis(t, "")+","+clients("alice:60:2"), []string{at(bad, ""), at(good, "")},
		`{"name":"fast","targets":[{"provider":"p0","upstream_model":"mock-small","weight":1},`+
			`{"provider":"p1","upstream_model":"mock-small","weight":0}]}`)
	for i, want := range []string{"1", "0"} {
		resp, b := send(t, srv, ask, as("alice")...)
		if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Attempts") != "2" ||
			resp.Header.Get("X-Ratelimit-Remaining-Requests") != want {
			t.Fatalf("request %d: %d %v %s", i+1, resp.StatusCode, resp.Header, b)
		}
	}
	if resp, _ := send(t, srv, ask, as("alice")...); resp.StatusCode != 429 {
		t.Errorf("the third request (the fifth attempt): %d", resp.StatusCode)
	}
}

func TestARefusedRequestsBodyIsNeverRead(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, `"max_request_bytes":100,`+testRedis(t, "")+","+clients("alice:60:1"),
		[]string{at(up, "")}, "fast=p0/mock-small")
	send(t, srv, ask, as("alice")...)
	big := `{"model":"fast","messages":[{"role":"user","content":"` + strings.Repeat("x", 1000) + `"}]}`
	if resp, b := send(t, srv, big, as("alice")...); resp.StatusCode != 429 {
		t.Errorf("a refused request over the size limit: %d %s; the limit comes first", resp.StatusCode, b)
	}
}

func TestWhenRedisIsDownALimitedRequestGoesOnOrIsRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := ln.Addr().String()
	ln.Close()
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("refuse=%v", refuse), func(t *testing.T) {
			up := newProvider(t, mockProvider(t, "mock-a"))
			onError := "allow"
			if refuse {
				onError = "refuse"
			}
			srv, _, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"on_error":%q},`, down, onError)+
				clients("alice:60:1"), []string{at(up, "")}, "fast=p0/mock-small")
			resp, b := send(t, srv, ask, as("alice")...)
			line := logLines(t, logs, "chat completion")[0]
			if line["rate_limit"] != "unavailable" || line["redis_err"] == nil {
				t.Errorf("log: %v", line)
			}
			if resp.Header.Get("X-Ratelimit-Limit-Requests") != "" {
				t.Errorf("no limit headers when the limit is unknown: %v", resp.Header)
			}
			if !refuse {
				if resp.StatusCode != 200 || len(up.requests()) != 1 {
					t.Errorf("allowed: %d %s", resp.StatusCode, b)
				}
				return
			}
			if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "rate_limit_unavailable" ||
				len(up.requests()) != 0 {
				t.Errorf("refused: %d %s, %d upstream", resp.StatusCode, b, len(up.requests()))
			}
		})
	}
}

func TestARedisThatNeverAnswersCostsAtMostItsTimeout(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0") // accepts connections, never answers
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"timeout":"50ms"},`, silent.Addr())+
		clients("alice:60:1"), []string{at(up, "")}, "fast=p0/mock-small")
	began := time.Now()
	resp, b := send(t, srv, ask, as("alice")...)
	if took := time.Since(began); resp.StatusCode != 200 || took > 500*time.Millisecond {
		t.Errorf("%d %s in %s: the Redis timeout is 50 ms", resp.StatusCode, b, took)
	}
	if line := logLines(t, logs, "chat completion")[0]; line["rate_limit"] != "unavailable" {
		t.Errorf("log: %v", line)
	}
}
