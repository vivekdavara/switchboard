package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/cache"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// cached is a model served by p0 as mock-small, with a cache.
func cached(name, cache string, aliases ...string) string {
	a := ""
	if len(aliases) > 0 {
		a = `,"aliases":["` + strings.Join(aliases, `","`) + `"]`
	}
	return fmt.Sprintf(`{"name":%q,"provider":"p0","upstream_model":"mock-small","cache":%s%s}`, name, cache, a)
}

func TestASecondIdenticalRequestIsAnsweredFromTheCache(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, redis+","+mini, []string{at(up, "")}, cached("fast", `{"ttl":"1m"}`, "gpt-4o-mini"),
		"plain=p0/mock-small")
	resp1, first := send(t, srv, ask)
	resp2, second := send(t, srv, ask)
	if resp1.StatusCode != 200 || resp1.Header.Get("X-Switchboard-Cache") != "miss" ||
		resp1.Header.Get("X-Switchboard-Attempts") != "1" {
		t.Fatalf("first: %d %v", resp1.StatusCode, resp1.Header)
	}
	if resp2.StatusCode != 200 || resp2.Header.Get("X-Switchboard-Cache") != "hit" ||
		resp2.Header.Get("X-Switchboard-Attempts") != "0" || resp2.Header.Get("X-Switchboard-Provider") != "p0" ||
		resp2.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("second: %d %v", resp2.StatusCode, resp2.Header)
	}
	if string(first) != string(second) {
		t.Errorf("the hit isn't the first answer byte for byte:\n%s\n%s", first, second)
	}
	// Whitespace doesn't matter, and an alias is its model; another parameter is another request.
	resp3, third := send(t, srv, "{\n \"model\": \"gpt-4o-mini\",\n \"messages\": [{\"role\": \"user\", "+
		"\"content\": \"What is two plus two?\"}]\n}")
	if resp3.Header.Get("X-Switchboard-Cache") != "hit" || string(third) != string(first) {
		t.Errorf("reformatted, through the alias: %v %s", resp3.Header, third)
	}
	resp4, fourth := send(t, srv, strings.Replace(ask, `}]}`, `}],"temperature":0.5}`, 1))
	if resp4.Header.Get("X-Switchboard-Cache") != "miss" || string(fourth) == string(first) {
		t.Errorf("another temperature: %v", resp4.Header)
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("the provider was asked %d times, not 2", n)
	}
	// A model without a cache says nothing about one.
	if resp, _ := send(t, srv, strings.Replace(ask, `"fast"`, `"plain"`, 1)); resp.Header.Get("X-Switchboard-Cache") != "" {
		t.Errorf("no cache: %v", resp.Header)
	}

	lines := logLines(t, logs, "chat completion")
	if lines[0]["cache"] != "miss" || lines[0]["cache_stored"] != true || lines[1]["cache"] != "hit" ||
		lines[1]["cost_usd"] != nil || lines[1]["prompt_tokens"] != nil || lines[1]["upstream_status"] != nil ||
		lines[1]["provider"] != "p0" {
		t.Errorf("log: %v\n%v", lines[0], lines[1])
	}
	rows := today(t, totals)
	if len(rows) != 2 || rows[0].Model != "fast" || rows[0].Entry != (usage.Entry{Completions: 2, CacheHits: 2,
		Tokens: usage.Usage{Prompt: 10, Completion: 64}, Cost: 39_900_000}) {
		t.Errorf("a hit costs nothing: %+v", rows)
	}
}

func TestACacheIsEachClientsOwnUnlessShared(t *testing.T) {
	redis, _ := redisTotals(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, redis+","+clients("alice", "bob"), []string{at(up, "")},
		cached("own", `{"ttl":"1m"}`), cached("shared", `{"ttl":"1m","shared":true}`))
	for _, tc := range []struct{ model, who, want string }{
		{"own", "alice", "miss"}, {"own", "alice", "hit"}, {"own", "bob", "miss"}, {"own", "bob", "hit"},
		{"shared", "alice", "miss"}, {"shared", "bob", "hit"},
	} {
		resp, b := send(t, srv, strings.Replace(ask, `"fast"`, `"`+tc.model+`"`, 1), as(tc.who)...)
		if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Cache") != tc.want {
			t.Errorf("%s as %s: %d %s %s, want %s", tc.model, tc.who, resp.StatusCode,
				resp.Header.Get("X-Switchboard-Cache"), b, tc.want)
		}
	}
}

func TestOnlyAnswersAreKept(t *testing.T) {
	redis, _ := redisTotals(t)
	refusing := newProvider(t, answering(400, `{"error":{"message":"bad","type":"invalid_request_error"}}`))
	failing := newProvider(t, answering(500, `{"error":{"message":"down","type":"server_error"}}`))
	srv, _, _ := gatewayFor(t, redis, []string{at(refusing, ""), at(failing, "")},
		`{"name":"a","provider":"p0","cache":{"ttl":"1m"}}`, `{"name":"b","provider":"p1","cache":{"ttl":"1m"}}`)
	for _, model := range []string{"a", "b"} {
		for range 2 {
			resp, _ := send(t, srv, strings.Replace(ask, `"fast"`, `"`+model+`"`, 1))
			if resp.Header.Get("X-Switchboard-Cache") != "miss" {
				t.Errorf("%s: %d %s", model, resp.StatusCode, resp.Header.Get("X-Switchboard-Cache"))
			}
		}
	}
	if len(refusing.requests()) != 2 || len(failing.requests()) != 2 {
		t.Errorf("asked %d and %d times", len(refusing.requests()), len(failing.requests()))
	}
}

func TestAClientCanAskForAFreshAnswer(t *testing.T) {
	redis, _ := redisTotals(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, _ := gatewayFor(t, redis, []string{at(up, "")}, cached("fast", `{"ttl":"1m"}`))
	var answers []string
	for _, tc := range []struct{ cacheControl, want string }{
		{"no-store", "bypass"}, // not read, not kept
		{"", "miss"},           // so this is a miss, and is kept
		{"", "hit"},
		{"No-Cache", "refresh"}, // not read, kept: the next hit is this answer
		{"max-age=0, no-store", "bypass"},
		{"", "hit"},
	} {
		resp, b := send(t, srv, ask, "Cache-Control", tc.cacheControl)
		if resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Cache") != tc.want {
			t.Errorf("Cache-Control %q: %s, want %s", tc.cacheControl, resp.Header.Get("X-Switchboard-Cache"), tc.want)
		}
		answers = append(answers, string(b))
	}
	if answers[2] != answers[1] || answers[5] != answers[3] || answers[3] == answers[1] {
		t.Errorf("a refresh replaces the entry: %q", answers)
	}
	if n := len(up.requests()); n != 4 {
		t.Errorf("the provider was asked %d times, not 4", n)
	}
}

func TestAFinishedStreamIsKeptAndABrokenOneIsNot(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, pacedMock(t, "mock-a", 0, nil))
	broken := newProvider(t, streaming([]string{chunk, chunk}, 0, true))
	srv, _, logs := gatewayFor(t, redis, []string{at(up, ""), at(broken, "")},
		`{"name":"m","provider":"p0","upstream_model":"mock-small","cache":{"ttl":"1m"}}`,
		`{"name":"b","provider":"p1","cache":{"ttl":"1m"}}`)
	resp1, first, err1 := readStream(t, srv, askStream)
	resp2, second, err2 := readStream(t, srv, askStream)
	if err1 != nil || err2 != nil || resp1.Header.Get("X-Switchboard-Cache") != "miss" ||
		resp2.Header.Get("X-Switchboard-Cache") != "hit" || resp2.Header.Get("Content-Type") != "text/event-stream" ||
		resp2.Header.Get("Cache-Control") != "no-cache" || resp2.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%v %v %v %v", err1, err2, resp1.Header, resp2.Header)
	}
	if len(first) != 35 || len(second) != len(first) {
		t.Fatalf("%d and %d events", len(first), len(second))
	}
	for i := range first {
		if first[i].raw != second[i].raw {
			t.Errorf("event %d: %q, then %q", i, first[i].raw, second[i].raw)
		}
	}
	if lines := logLines(t, logs, "chat completion"); lines[1]["events"] != 34.0 || lines[1]["streamed"] != true {
		t.Errorf("log: %v", lines[1])
	}
	for range 2 {
		readStream(t, srv, strings.Replace(askStream, `"m"`, `"b"`, 1))
	}
	if n := len(broken.requests()); n != 2 {
		t.Errorf("a broken stream was kept: the provider was asked %d times", n)
	}
	if rows := today(t, totals); len(rows) != 1 || rows[0].CacheHits != 1 || rows[0].Completions != 1 {
		t.Errorf("%+v", rows)
	}
}

func TestWhenRedisIsDownTheCacheIsAMiss(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q}`, closedAddr(t)), []string{at(up, "")},
		cached("fast", `{"ttl":"1m"}`))
	for range 2 {
		if resp, b := send(t, srv, ask); resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Cache") != "miss" {
			t.Errorf("%d %v %s", resp.StatusCode, resp.Header, b)
		}
	}
	if l := logLines(t, logs, "chat completion")[0]; l["cache"] != "unavailable" || l["cache_err"] == nil ||
		l["cache_stored"] != nil {
		t.Errorf("log: %v", l)
	}
}

func TestAStreamOverTheEntryLimitIsntKept(t *testing.T) {
	redis, _ := redisTotals(t)
	// 1,100 chunks of about 1 KB: over cache.MaxEntryBytes (1 MiB) in all, each one far below it.
	content := strings.Repeat("x", 1000)
	events := make([]string, 0, 1101)
	for range 1100 {
		events = append(events, `data: {"id":"c","choices":[{"index":0,"delta":{"content":"`+content+`"}}]}`+"\n\n")
	}
	events = append(events, "data: [DONE]\n\n")
	up := newProvider(t, streaming(events, 0, false))
	small := newProvider(t, streaming(events[len(events)-2:], 0, false))
	srv, _, logs := gatewayFor(t, redis, []string{at(up, ""), at(small, "")},
		`{"name":"m","provider":"p0","cache":{"ttl":"1m"}}`, `{"name":"s","provider":"p1","cache":{"ttl":"1m"}}`)
	for i := range 2 {
		_, got, err := readStream(t, srv, askStream)
		if err != nil || len(got) != 1101 {
			t.Fatalf("request %d: %d events, %v", i+1, len(got), err)
		}
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("a stream over the limit was kept: the provider was asked %d times", n)
	}
	if l := logLines(t, logs, "chat completion")[0]; l["cache"] != "miss" || l["cache_stored"] != nil {
		t.Errorf("log: %v", l)
	}
	// A small stream through the same code is kept, so the limit is what stopped the big one.
	for range 2 {
		readStream(t, srv, strings.Replace(askStream, `"m"`, `"s"`, 1))
	}
	if n := len(small.requests()); n != 1 {
		t.Errorf("a small stream: the provider was asked %d times", n)
	}
}

// keeping stops holding a stream's bytes once they pass the limit, rather than holding them all and having the
// cache refuse them at the end (which TestAStreamOverTheEntryLimitIsntKept can't tell apart).
func TestKeepingAStreamStopsAtTheLimit(t *testing.T) {
	k := keeping{b: &bytes.Buffer{}}
	k.add(make([]byte, cache.MaxEntryBytes-1))
	k.add([]byte("x"))
	if k.b == nil || k.b.Len() != cache.MaxEntryBytes {
		t.Fatalf("exactly the limit is kept: %v", k.b)
	}
	k.add([]byte("y"))
	if k.b != nil {
		t.Errorf("one byte past the limit still kept")
	}
	k.add([]byte("z")) // and nothing after
	if k.b != nil {
		t.Error("kept again")
	}
}

func TestA200ThatIsAnErrorIsntKept(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, answering(200, `{"error":{"message":"overloaded","type":"server_error"}}`))
	srv, _, _ := gatewayFor(t, redis, []string{at(up, "")}, `{"name":"fast","provider":"p0","cache":{"ttl":"1m"}}`)
	for range 2 {
		if resp, _ := send(t, srv, ask); resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Cache") != "miss" {
			t.Errorf("%d %s", resp.StatusCode, resp.Header.Get("X-Switchboard-Cache"))
		}
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("the provider was asked %d times", n)
	}
	if rows := today(t, totals); len(rows) != 0 {
		t.Errorf("a 200 that is an error was counted as a completion: %+v", rows)
	}
}

func TestAStopDuringTheCacheLookupEndsTheWait(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0") // accepts connections, never answers
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	up := newProvider(t, mockProvider(t, "mock-a"))
	_, g, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"timeout":"5s"}`, silent.Addr()), []string{at(up, "")},
		cached("fast", `{"ttl":"1m"}`))
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	time.AfterFunc(100*time.Millisecond, func() { stop(ErrShutdown) })
	began := time.Now()
	resp, b := send(t, srv, ask)
	if _, code, _, _ := errorOf(t, b); resp.StatusCode != 503 || code != "shutting_down" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("took %s: the stop should end the lookup, not Redis's 5 s timeout", took)
	}
	if len(up.requests()) != 0 {
		t.Error("a stopping gateway asked the provider")
	}
	if l := logLines(t, logs, "chat completion")[0]; l["cache"] == "unavailable" || l["attempts"] != nil {
		t.Errorf("the stop was logged as Redis's failure, or an attempt followed: %v", l)
	}
}
