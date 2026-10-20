package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/jsonstrict"
	"github.com/vivekdavara/switchboard/internal/redistest"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// redisTotals is a config's redis (the test Redis, under a prefix of the test's own) and the totals it keeps.
func redisTotals(t *testing.T) (string, *usage.Totals) {
	rdb, prefix := redistest.Client(t)
	return fmt.Sprintf(`"redis":{"addr":%q,"prefix":%q}`, redistest.Addr(t), prefix), usage.NewTotals(rdb, prefix)
}

// today returns today's totals.
func today(t *testing.T, totals *usage.Totals) []usage.Row {
	t.Helper()
	rows, err := totals.Report(context.Background(), usage.Day(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// mini is gpt-4o-mini's price, $0.15 and $0.60 per million tokens, for p0's mock-small.
const mini = `"prices":[{"provider":"p0","upstream_model":"mock-small","input_per_million":0.15,` +
	`"output_per_million":0.6}]`

func TestAnAnswersUsageAndCostAreLoggedAndAddedUp(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	other := newProvider(t, mockProvider(t, "mock-b"))
	srv, _, logs := gatewayFor(t, redis+","+clients("alice", "bob")+","+mini, []string{at(up, ""), at(other, "")},
		`{"name":"fast","aliases":["gpt-4o-mini"],"provider":"p0","upstream_model":"mock-small"}`,
		"unpriced=p1/mock-small")
	for _, req := range []struct{ who, body string }{
		{"alice", ask},
		{"alice", strings.Replace(ask, `"fast"`, `"gpt-4o-mini"`, 1)},
		{"bob", ask},
		{"bob", strings.Replace(ask, `"fast"`, `"unpriced"`, 1)},
	} {
		if resp, b := send(t, srv, req.body, as(req.who)...); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	lines := logLines(t, logs, "chat completion")
	// The mock counts words as tokens: "What is two plus two?" is 5, and it writes 32. 5 × 0.15 + 32 × 0.6
	// microdollars is 19.95 microdollars.
	if l := lines[0]; l["prompt_tokens"] != 5.0 || l["completion_tokens"] != 32.0 || l["cost_usd"] != "0.00001995" {
		t.Errorf("log: %v", l)
	}
	if l := lines[3]; l["prompt_tokens"] != 5.0 || l["cost_usd"] != nil {
		t.Errorf("an unpriced model has tokens and no cost: %v", l)
	}
	rows := today(t, totals)
	want := []usage.Row{
		{Client: "alice", Model: "fast", Entry: usage.Entry{Completions: 2, Tokens: usage.Usage{Prompt: 10,
			Completion: 64}, Cost: 39_900_000}},
		{Client: "bob", Model: "fast", Entry: usage.Entry{Completions: 1, Tokens: usage.Usage{Prompt: 5,
			Completion: 32}, Cost: 19_950_000}},
		{Client: "bob", Model: "unpriced", Entry: usage.Entry{Completions: 1, Tokens: usage.Usage{Prompt: 5,
			Completion: 32}, Unpriced: 1}},
	}
	if len(rows) != len(want) {
		t.Fatalf("%+v", rows)
	}
	for i := range want {
		want[i].Day = usage.Day(time.Now())
		if rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v (an alias counts as its model)", i, rows[i], want[i])
		}
	}
}

func TestOnlyCompletionsAreCounted(t *testing.T) {
	redis, totals := redisTotals(t)
	noUsage := newProvider(t, answering(200, `{"id":"x","object":"chat.completion","choices":[]}`))
	refusing := newProvider(t, answering(400, `{"error":{"message":"bad","type":"invalid_request_error"}}`))
	failing := newProvider(t, answering(500, `{"error":{"message":"down","type":"server_error"}}`))
	srv, _, _ := gatewayFor(t, redis, []string{at(noUsage, ""), at(refusing, ""), at(failing, "")},
		"a=p0/x", "b=p1/x", "c=p2/x")
	for _, model := range []string{"a", "b", "c"} {
		send(t, srv, strings.Replace(ask, `"fast"`, `"`+model+`"`, 1))
	}
	rows := today(t, totals)
	if len(rows) != 1 || rows[0].Client != "anonymous" || rows[0].Model != "a" ||
		rows[0].Entry != (usage.Entry{Completions: 1, NoUsage: 1}) {
		t.Errorf("a 200 without usage counts as a completion with no usage, and nothing else counts: %+v", rows)
	}
}

func TestWhenRedisIsDownTheAnswerStillGoesOut(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q},`, closedAddr(t))+mini, []string{at(up, "")},
		"fast=p0/mock-small")
	if resp, b := send(t, srv, ask); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if l := logLines(t, logs, "chat completion")[0]; l["totals_err"] == nil || l["cost_usd"] != "0.00001995" {
		t.Errorf("log: %v", l)
	}
}

// A stream's usage. The client that asked for it gets it as the provider sent it. For one that didn't, the gateway
// asks (stream_options.include_usage) and takes it out again, so the client's stream is the one it asked for.
func TestAStreamsUsageIsCountedWhetherOrNotTheClientAskedForIt(t *testing.T) {
	asking := func(body string) string { return body[:len(body)-1] + `,"stream_options":{"include_usage":true}}` }
	direct := newProvider(t, pacedMock(t, "mock-a", 0, nil))
	toMock := strings.Replace(askStream, `"m"`, `"mock-small"`, 1)
	_, plain, _ := readStream(t, direct.Server, toMock)
	_, withUsage, _ := readStream(t, direct.Server, asking(toMock))
	if len(withUsage) != len(plain)+1 {
		t.Fatalf("the mock's stream: %d events, and %d with its usage", len(plain), len(withUsage))
	}

	redis, totals := redisTotals(t)
	up := newProvider(t, pacedMock(t, "mock-a", 0, nil))
	off := newProvider(t, pacedMock(t, "mock-a", 0, nil))
	srv, _, logs := gatewayFor(t, redis+","+mini, []string{at(up, ""), at(off, `"stream_usage":false`)},
		"m=p0/mock-small", "off=p1/mock-small")
	for _, tc := range []struct {
		name, body string
		want       []streamEvent
		upstream   string // the stream_options the provider was sent
	}{
		{"not asked", askStream, plain, `{"include_usage":true}`},
		{"asked", asking(askStream), withUsage, `{"include_usage":true}`},
		{"stream_usage off", strings.Replace(askStream, `"m"`, `"off"`, 1), plain, ""},
	} {
		_, got, err := readStream(t, srv, tc.body)
		if err != nil || len(got) != len(tc.want) {
			t.Fatalf("%s: %d events, want %d, %v", tc.name, len(got), len(tc.want), err)
		}
		for i := range got {
			if got[i].raw != tc.want[i].raw {
				t.Errorf("%s: event %d:\n%q\nwant\n%q", tc.name, i, got[i].raw, tc.want[i].raw)
			}
		}
	}
	reqs := append(up.requests(), off.requests()...)
	for i, want := range []string{`{"include_usage":true}`, `{"include_usage":true}`, ""} {
		var body struct {
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		json.Unmarshal(reqs[i].body, &body)
		if string(body.StreamOptions) != want {
			t.Errorf("request %d upstream: stream_options %s, want %s", i, body.StreamOptions, want)
		}
	}
	lines := logLines(t, logs, "chat completion")
	for i, want := range []any{"0.00001935", "0.00001935", nil} { // 1 prompt token ("x") and 32
		if lines[i]["cost_usd"] != want {
			t.Errorf("log %d: %v", i, lines[i])
		}
	}
	if lines[0]["events"] != 34.0 {
		t.Errorf("the usage chunk taken out isn't an event the client got: %v", lines[0])
	}
	rows := today(t, totals)
	if len(rows) != 2 || rows[0].Model != "m" || rows[0].Entry != (usage.Entry{Completions: 2,
		Tokens: usage.Usage{Prompt: 2, Completion: 64}, Cost: 38_700_000}) ||
		rows[1].Entry != (usage.Entry{Completions: 1, NoUsage: 1}) {
		t.Errorf("%+v", rows)
	}
}

// OpenAI, asked for the usage, also puts "usage": null on every chunk, and may send the usage with the last choices;
// both come out for a client that didn't ask.
func TestUsageTakenOutOfEveryChunk(t *testing.T) {
	events := []string{
		`data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""}}],"usage":null}` + "\n\n",
		": a comment about \"usage\"\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hi \"usage\""}}],` + "\n" +
			`data: "usage":null}` + "\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,` +
			`"completion_tokens":2}}` + "\n\n",
		`data: {"id":"c","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n",
		`data: {"id":"c","usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	up := newProvider(t, streaming(events, 0, false))
	srv, _, logs := gatewayFor(t, `"prices":[{"provider":"p0","upstream_model":"x","input_per_million":1,`+
		`"output_per_million":1}]`, []string{at(up, "")}, "m=p0/x")
	_, got, err := readStream(t, srv, askStream)
	want := []string{
		`data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		events[1],
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hi \"usage\""}}]}` + "\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	if err != nil || len(got) != len(want) {
		t.Fatalf("%d events %v: %v", len(got), err, got)
	}
	for i := range want {
		if got[i].raw != want[i] {
			t.Errorf("event %d:\n%q\nwant\n%q", i, got[i].raw, want[i])
		}
	}
	if l := logLines(t, logs, "chat completion")[0]; l["prompt_tokens"] != 3.0 || l["cost_usd"] != "0.000005" {
		t.Errorf("log: %v", l)
	}
}

func TestABrokenStreamIsNotCounted(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, streaming([]string{chunk, chunk}, 0, true))
	srv, _, _ := gatewayFor(t, redis, []string{at(up, "")}, "m=p0/x")
	readStream(t, srv, askStream)
	if rows := today(t, totals); len(rows) != 0 {
		t.Errorf("%+v", rows)
	}
}

func TestAChunkThatMentionsUsageButIsntAnObjectPassesAsItCame(t *testing.T) {
	for _, data := range []string{
		`{"choices":[],"usage":{"prompt_tokens":1},"usage":null}`, // a key twice: not one meaning, so not touched
		`["usage"]`,
		`"usage"`,
	} {
		ev := sseEvent{data: []byte(data), raw: []byte("data: " + data + "\n\n")}
		if got := unasked(ev); string(got) != string(ev.raw) {
			t.Errorf("%s became %q", data, got)
		}
	}
}

func TestUnaskedKeepsAChunkWithChoicesOfAnyKindAndCompactsOnlyWhatSpansLines(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		// choices that aren't a list are choices: only the usage goes.
		{`data: {"choices":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n",
			`data: {"choices":"x"}` + "\n\n"},
		// A value spread over two data lines can't go on one as it is: compacted.
		{"data: {\"choices\":[\ndata: {\"index\":0}],\"usage\":null}\n\n", `data: {"choices":[{"index":0}]}` + "\n\n"},
		// One line: everything but the usage as it came, spaces included.
		{`data: {"choices":[{"index": 0}], "usage":null}` + "\n\n", `data: {"choices":[{"index": 0}]}` + "\n\n"},
		// A CRLF event keeps its other lines' endings; the data line is written with \n.
		{"id: 1\r\ndata: {\"choices\":[1],\"usage\":null}\r\n\r\n", "id: 1\r\ndata: {\"choices\":[1]}\n\r\n"},
	} {
		r := newEventReader(strings.NewReader(tc.raw), 1<<20)
		ev, err := r.next()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(unasked(ev)); got != tc.want {
			t.Errorf("%q:\n got %q\nwant %q", tc.raw, got, tc.want)
		}
	}
}

func TestTheLastUsageInAStreamCounts(t *testing.T) {
	events := []string{
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":3,` +
			`"completion_tokens":1}}` + "\n\n", // a running count, as vLLM's continuous usage sends
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,` +
			`"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	up := newProvider(t, streaming(events, 0, false))
	srv, _, logs := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x")
	readStream(t, srv, askStream[:len(askStream)-1]+`,"stream_options":{"include_usage":true}}`)
	if l := logLines(t, logs, "chat completion")[0]; l["completion_tokens"] != 2.0 {
		t.Errorf("log: %v", l)
	}
}

// The totals are added, and an answer kept, even when the client has gone by then: it has its answer, and they are
// the gateway's.
func TestTheTotalsAndTheCacheOutliveTheClient(t *testing.T) {
	redis, totals := redisTotals(t)
	up := newProvider(t, mockProvider(t, "mock-a"))
	_, g, _ := gatewayFor(t, redis, []string{at(up, "")}, cached("fast", `{"ttl":"1m"}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", nil)
	x := &exchange{began: time.Now(), route: "fast", provider: "p0", upstreamModel: "mock-small"}
	g.addTotals(r, x, usage.Entry{Completions: 1})
	g.store(r, x, "k", time.Minute, []byte(`{"id":"x"}`))
	if x.totalsErr != nil || x.cacheErr != nil || !x.cacheStored {
		t.Errorf("%v %v %v", x.totalsErr, x.cacheErr, x.cacheStored)
	}
	if rows := today(t, totals); len(rows) != 1 || rows[0].Completions != 1 {
		t.Errorf("%+v", rows)
	}
}

func TestAStopEndsATotalsWriteToARedisThatNeverAnswers(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0") // accepts connections, never answers
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	up := newProvider(t, mockProvider(t, "mock-a"))
	_, g, logs := gatewayFor(t, fmt.Sprintf(`"redis":{"addr":%q,"timeout":"5s"}`, silent.Addr()), []string{at(up, "")},
		"fast=p0/mock-small")
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	go func() { // stop once the provider has answered, while the totals wait on Redis
		for deadline := time.Now().Add(5 * time.Second); len(up.requests()) == 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		stop(ErrShutdown)
	}()
	began := time.Now()
	resp, b := send(t, srv, ask)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "mock-a answering") {
		t.Errorf("the answer was lost: %d %s", resp.StatusCode, b)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("took %s: the stop should end the totals' wait, not Redis's 5 s timeout", took)
	}
	if l := logLines(t, logs, "chat completion")[0]; l["totals_err"] == nil {
		t.Errorf("log: %v", l)
	}
}

// FuzzUnaskedsFastPathAgreesWithTheParse checks unasked's fast path (OpenAI's ,"usage":null} cut out of the bytes)
// against the parse it saves: whenever it applies to a chunk the parse accepts, both give one data line, and the two
// chunks decode to the same value, with no usage in it.
func FuzzUnaskedsFastPathAgreesWithTheParse(f *testing.F) {
	for _, s := range []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,` +
			`"delta":{"content":" word"},"logprobs":null,"finish_reason":null}],"usage":null}`,
		`{"choices":[],"usage":null}`,
		`{"usage":null}`,
		`{"a":"\",\"usage\":null}","usage":null}`,
		`{"a":{"b":1,"usage":null},"usage":null}`,
		`{"usage":{"x":1},"usage":null}`,
		`{"a": 1 ,"usage":null}`,
		`{"choices":[{"delta":{"content":"usage"}}],"x":"y","usage":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if bytes.ContainsAny(data, "\r\n") {
			return // one data line
		}
		ev := sseEvent{data: data, raw: append(append([]byte("data: "), data...), "\n\n"...)}
		fast, ok := unaskedNull(ev)
		if !ok {
			return
		}
		if _, err := jsonstrict.ParseObject(data); err != nil {
			return // the parse leaves what it can't read as it came; the fast path's cut is still exact
		}
		slow := unaskedParsed(ev)
		if slow == nil {
			t.Fatalf("the parse drops %q, the fast path keeps it", data)
		}
		decode := func(event []byte) any {
			line, ok := bytes.CutPrefix(event, []byte("data: "))
			line, ok2 := bytes.CutSuffix(line, []byte("\n\n"))
			if !ok || !ok2 || bytes.ContainsAny(line, "\r\n") {
				t.Fatalf("not one data line: %q", event)
			}
			var v any
			d := json.NewDecoder(bytes.NewReader(line))
			d.UseNumber()
			if err := d.Decode(&v); err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			if m, isObject := v.(map[string]any); !isObject || m["usage"] != nil {
				t.Fatalf("usage left in, or not an object: %q", line)
			}
			return v
		}
		if a, b := decode(fast), decode(slow); !reflect.DeepEqual(a, b) {
			t.Fatalf("for %q the fast path gives %q, the parse %q", data, fast, slow)
		}
	})
}
