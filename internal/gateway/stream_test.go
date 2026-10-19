package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/mock"
)

const askStream = `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`

// streamEvent is one event as the client read it: its bytes, and when it arrived.
type streamEvent struct {
	raw string
	at  time.Duration
}

// readStream posts body through the gateway and reads the answer event by event; err is how the body ended (nil for
// a clean end).
func readStream(t *testing.T, srv *httptest.Server, body string) (*http.Response, []streamEvent, error) {
	t.Helper()
	return readStreamCtx(t, context.Background(), srv, body)
}

func readStreamCtx(t *testing.T, ctx context.Context, srv *httptest.Server, body string) (*http.Response,
	[]streamEvent, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	began := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []streamEvent
	r := bufio.NewReader(resp.Body)
	var ev strings.Builder
	for {
		line, err := r.ReadString('\n')
		ev.WriteString(line)
		if err != nil {
			if err == io.EOF && ev.Len() == 0 {
				err = nil
			}
			return resp, events, err
		}
		if line == "\n" {
			events = append(events, streamEvent{ev.String(), time.Since(began)})
			ev.Reset()
		}
	}
}

// pacedMock is a mock provider with a fixed clock, so two of them stream the same bytes.
func pacedMock(t *testing.T, name string, perToken time.Duration, change func(*mock.Config)) *mock.Provider {
	t.Helper()
	cfg := mock.Defaults()
	cfg.Name, cfg.TTFT, cfg.PerToken = name, 0, perToken
	cfg.Now = func() time.Time { return time.Unix(1_760_000_000, 0) }
	if change != nil {
		change(&cfg)
	}
	m, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestAStreamIsRelayedEventByEventAsItArrives(t *testing.T) {
	direct := httptest.NewServer(pacedMock(t, "mock-a", 0, nil))
	defer direct.Close()
	_, want, err := readStream(t, direct, strings.Replace(askStream, `"m"`, `"mock-small"`, 1))
	if err != nil || len(want) != 1+32+1+1 {
		t.Fatalf("%d events, %v", len(want), err)
	}

	up := newProvider(t, pacedMock(t, "mock-a", 20*time.Millisecond, nil))
	srv, _, logs := gatewayFor(t, "", []string{at(up, "")}, "m=p0/mock-small")
	resp, got, err := readStream(t, srv, askStream)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("X-Switchboard-Provider") != "p0" || resp.Header.Get("X-Switchboard-Attempts") != "1" ||
		!requestID.MatchString(resp.Header.Get("X-Request-Id")) || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].raw != want[i].raw {
			t.Fatalf("event %d:\n%q\nwant\n%q", i, got[i].raw, want[i].raw)
		}
	}
	// Flushed per event: the 32 words, 20 ms apart, arrive spread over about 640 ms, not all at the end.
	if spread := got[len(got)-1].at - got[0].at; spread < 500*time.Millisecond {
		t.Errorf("the events arrived within %s of each other: buffered, not relayed", spread)
	}
	if mid := got[16].at - got[0].at; mid < 250*time.Millisecond || mid > got[len(got)-1].at-got[0].at-200*time.Millisecond {
		t.Errorf("the 16th event came %s after the first", mid)
	}
	if a := up.requests()[0].header.Get("Accept"); a != "text/event-stream" {
		t.Errorf("upstream Accept %q", a)
	}
	x := lastExchange(t, logs)
	if x.Status != 200 || x.Code != "" || !strings.Contains(logs.String(), `"streamed":true,"events":34`) {
		t.Errorf("%+v\n%s", x, logs)
	}
}

// streaming answers 200 text/event-stream with the given events, flushing each, then closes the connection without
// ending the body cleanly if cut, after waiting hold.
func streaming(events []string, hold time.Duration, cut bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		for _, e := range events {
			io.WriteString(w, e)
			rc.Flush()
		}
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
		}
		if cut {
			panic(http.ErrAbortHandler)
		}
	})
}

const chunk = `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"x","choices":[]}` + "\n\n"

func TestAStreamFallsBackUntilItsFirstEvent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		first   http.Handler
		outcome string
	}{
		{"a 500", answering(500, `{}`), "failed"},
		{"a 429", answering(429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`), "failed"},
		{"no event at all", streaming(nil, 0, false), "unanswered"},
		{"a comment, then nothing", streaming([]string{": ping\n\n"}, 0, true), "unanswered"},
		{"half an event", streaming([]string{"data: {\"id\""}, 0, true), "unanswered"},
		{"an error as the first event", streaming([]string{`data: {"error":{"message":"overloaded"}}` + "\n\n"}, 0,
			false), "failed"},
		{"no event within the timeout", streaming(nil, 2*time.Second, false), "unanswered"},
		{"JSON instead of a stream", answering(200, `{"id":"c","object":"chat.completion","choices":[]}`),
			"unanswered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p0 := newProvider(t, tc.first)
			p1 := newProvider(t, pacedMock(t, "mock-b", 0, nil))
			srv, _, logs := gatewayFor(t, "", []string{at(p0, `"timeout":"300ms"`), at(p1, "")},
				inOrder("m", "p0/mock-small", "p1/mock-small"))
			resp, events, err := readStream(t, srv, askStream)
			if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p1" ||
				resp.Header.Get("X-Switchboard-Attempts") != "2" || len(events) != 35 ||
				events[len(events)-1].raw != "data: [DONE]\n\n" {
				t.Fatalf("%d %v %d events, %v", resp.StatusCode, resp.Header, len(events), err)
			}
			for _, e := range events {
				if strings.Contains(e.raw, "ping") || strings.Contains(e.raw, "overloaded") {
					t.Errorf("p0's event reached the client: %q", e.raw)
				}
			}
			if x := lastExchange(t, logs); len(x.Tried) != 2 || x.Tried[0].Outcome != tc.outcome {
				t.Errorf("%+v", x.Tried)
			}
		})
	}
}

func TestBeforeItsFirstEventAStreamsFailureIsAnErrorLikeAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name   string
		h      http.Handler
		status int
		code   string
	}{
		{"a 400 passes through", answering(400, `{"error":{"message":"bad","type":"invalid_request_error"}}`), 400, ""},
		{"an empty stream is not an empty 200", streaming(nil, 0, false), 502, "upstream_error"},
		{"a stream over the limit", streaming([]string{"data: " + strings.Repeat("x", 3000) + "\n\n"}, 0, false),
			502, "upstream_error"},
		{"no event within the timeout", streaming(nil, 2*time.Second, false), 504, "upstream_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p0 := newProvider(t, tc.h)
			srv, _, _ := gatewayFor(t, `"max_response_bytes":2000`, []string{at(p0, `"timeout":"300ms"`)},
				"m=p0/mock-small")
			resp, b := send(t, srv, askStream)
			if resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, b)
			}
			if _, code, _, _ := errorOf(t, b); code != tc.code {
				t.Errorf("code %q, want %q", code, tc.code)
			}
		})
	}
}

// lastEventError reads the error in a stream's last event.
func lastEventError(t *testing.T, events []streamEvent) string {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := strings.TrimSuffix(strings.TrimPrefix(events[len(events)-1].raw, "data: "), "\n\n")
	_, code, _, _ := errorOf(t, []byte(last))
	return code
}

func TestAStreamThatBreaksAfterItsFirstEventEndsWithAnErrorEvent(t *testing.T) {
	up := newProvider(t, pacedMock(t, "mock-a", 0, func(c *mock.Config) { c.CutRate = 1 }))
	spare := newProvider(t, pacedMock(t, "mock-b", 0, nil))
	srv, g, logs := gatewayFor(t, "", []string{at(up, ""), at(spare, "")}, inOrder("m", "p0/mock-small",
		"p1/mock-small"))
	resp, events, err := readStream(t, srv, askStream)
	if resp.StatusCode != 200 || err == nil {
		t.Fatalf("%d, the body ended with %v; want a broken transfer", resp.StatusCode, err)
	}
	if code := lastEventError(t, events); code != "upstream_unavailable" {
		t.Errorf("the last event's code is %q", code)
	}
	for _, e := range events {
		if strings.Contains(e.raw, "[DONE]") {
			t.Error("a broken stream said [DONE]")
		}
	}
	// No fallback once the first event is out: the client has part of p0's answer.
	if len(spare.requests()) != 0 {
		t.Error("the spare was asked after the stream started")
	}
	x := lastExchange(t, logs)
	if x.Status != 200 || x.Code != "upstream_unavailable" || x.Attempts != 1 {
		t.Errorf("%+v", x)
	}
	if g.breakers["p0"].State().String() != "closed" { // one failure of five
		t.Errorf("breaker %s", g.breakers["p0"].State())
	}
}

func TestAStreamsTimeoutIsBetweenEventsNotForTheWhole(t *testing.T) {
	// 8 events 100 ms apart take 800 ms, longer than the 300 ms timeout: fine, each came within it.
	slow := newProvider(t, pacedMock(t, "mock-a", 100*time.Millisecond, nil))
	srv, _, _ := gatewayFor(t, "", []string{at(slow, `"timeout":"300ms"`)}, "m=p0/mock-small")
	resp, events, err := readStream(t, srv,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true,"max_tokens":8}`)
	if err != nil || resp.StatusCode != 200 || len(events) != 1+8+1+1 || events[len(events)-1].at < 700*time.Millisecond {
		t.Fatalf("%d, %d events, %v", resp.StatusCode, len(events), err)
	}

	// Two events, then nothing for longer than the timeout.
	stall := newProvider(t, streaming([]string{chunk, chunk}, 5*time.Second, false))
	srv, _, logs := gatewayFor(t, "", []string{at(stall, `"timeout":"300ms"`)}, "m=p0/mock-small")
	began := time.Now()
	resp, events, err = readStream(t, srv, askStream)
	if resp.StatusCode != 200 || err == nil || len(events) != 3 {
		t.Fatalf("%d, %d events, %v", resp.StatusCode, len(events), err)
	}
	if code := lastEventError(t, events); code != "upstream_timeout" {
		t.Errorf("code %q", code)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("the stall took %s to end", took)
	}
	if x := lastExchange(t, logs); x.Status != 200 || x.Code != "upstream_timeout" {
		t.Errorf("%+v", x)
	}
}

func TestAClientThatLeavesMidStreamCancelsTheProvider(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	up := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunk)
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	srv, g, logs := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x")
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(askStream))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line %q", line)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's request outlived the client")
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"status":499`); {
		if time.Now().After(deadline) {
			t.Fatalf("no 499:\n%s", logs)
		}
		time.Sleep(time.Millisecond)
	}
	if g.breakers["p0"].State().String() != "closed" {
		t.Error("a client leaving moved the breaker")
	}
}

func TestAStreamAtTheStopEndsWithShuttingDown(t *testing.T) {
	up := newProvider(t, streaming([]string{chunk}, 10*time.Second, false))
	_, g, logs := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x")
	base, stop := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(g)
	srv.Config.BaseContext = func(net.Listener) context.Context { return base }
	srv.Start()
	defer srv.Close()
	go func() {
		for deadline := time.Now().Add(10 * time.Second); len(up.requests()) == 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // the first event is out
		stop(ErrShutdown)
	}()
	resp, events, err := readStream(t, srv, askStream)
	if resp.StatusCode != 200 || err == nil || len(events) != 2 {
		t.Fatalf("%d, %d events, %v", resp.StatusCode, len(events), err)
	}
	if code := lastEventError(t, events); code != "shutting_down" {
		t.Errorf("code %q", code)
	}
	if x := lastExchange(t, logs); x.Code != "shutting_down" {
		t.Errorf("%+v", x)
	}
}

func TestTheEventReader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		events []string // each event's data, or "-" for one with none
		end    error
	}{
		{"two events", "data: a\n\ndata: b\n\n", []string{"a", "b"}, io.EOF},
		{"CRLF", "data: a\r\n\r\ndata: b\r\n\r\n", []string{"a", "b"}, io.EOF},
		{"several data lines", "data: a\ndata: b\n\n", []string{"a\nb"}, io.EOF},
		{"no space after the colon", "data:a\n\n", []string{"a"}, io.EOF},
		{"a comment is an event without data", ": ping\n\ndata: a\n\n", []string{"-", "a"}, io.EOF},
		{"other fields", "event: x\nid: 1\ndata: a\n\n", []string{"a"}, io.EOF},
		{"blank lines between events", "\n\ndata: a\n\n\n\n", []string{"a"}, io.EOF},
		{"cut mid-line", "data: a\n\ndata: b", []string{"a"}, errStreamEnded},
		{"cut before the blank line", "data: a\n\ndata: b\n", []string{"a"}, errStreamEnded},
		{"a line longer than the buffer", "data: " + strings.Repeat("y", 100<<10) + "\n\n",
			[]string{strings.Repeat("y", 100<<10)}, io.EOF},
	} {
		er := newEventReader(strings.NewReader(tc.in), 1<<20)
		var got []string
		var err error
		for {
			var ev sseEvent
			if ev, err = er.next(); err != nil {
				break
			}
			if ev.data == nil {
				got = append(got, "-")
			} else {
				got = append(got, string(ev.data))
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.events) || err != tc.end {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, err, tc.events, tc.end)
		}
	}
	// The raw bytes are kept as they came, line endings and all.
	er := newEventReader(strings.NewReader("event: x\r\ndata: a\r\n\r\n"), 1<<20)
	if ev, err := er.next(); err != nil || string(ev.raw) != "event: x\r\ndata: a\r\n\r\n" {
		t.Errorf("%q %v", ev.raw, err)
	}
	// The limit counts every byte read.
	er = newEventReader(strings.NewReader("data: a\n\ndata: "+strings.Repeat("z", 100)+"\n\n"), 50)
	if _, err := er.next(); err != nil {
		t.Fatal(err)
	}
	if _, err := er.next(); err != errStreamTooBig {
		t.Errorf("over the limit: %v", err)
	}
	if !bytes.Equal([]byte(chunk[len(chunk)-2:]), []byte("\n\n")) {
		t.Fatal("chunk must end an event")
	}
}

func TestASlowClientIsNotTheProvidersTimeout(t *testing.T) {
	// The provider streams 8 MB at once; the client takes the first event, then reads nothing for longer than the
	// provider's timeout, so the gateway's writes block. The timeout is the provider's: waiting on the client isn't
	// the provider making no progress, and mustn't cut the stream or count against its breaker.
	big := strings.Repeat("y", 4000)
	var events []string
	for range 2000 {
		events = append(events, "data: "+big+"\n\n")
	}
	events = append(events, "data: [DONE]\n\n")
	up := newProvider(t, streaming(events, 0, false))
	srv, g, logs := gatewayFor(t, `"max_response_bytes":67108864,"circuit_breaker":{"failures":1}`,
		[]string{at(up, `"timeout":"200ms"`)}, "m=p0/x")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(askStream))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "data: y") {
		t.Fatalf("first line %.40q", line)
	}
	time.Sleep(600 * time.Millisecond)
	rest, err := io.ReadAll(r)
	if err != nil || !bytes.HasSuffix(rest, []byte("data: [DONE]\n\n")) || bytes.Contains(rest, []byte(`"error"`)) {
		t.Errorf("after a pause, the rest (%d bytes) ended with %q, %v", len(rest), rest[max(0, len(rest)-120):], err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), `"msg":"chat completion"`); {
		if time.Now().After(deadline) {
			t.Fatal("no log line")
		}
		time.Sleep(time.Millisecond)
	}
	if x := lastExchange(t, logs); x.Code != "" || g.breakers["p0"].State().String() != "closed" {
		t.Errorf("%+v, breaker %s", x, g.breakers["p0"].State())
	}
}

func TestCommentsBeforeTheFirstEventReachTheClientWithIt(t *testing.T) {
	up := newProvider(t, streaming([]string{": ping\n\n", ": still here\n\n", chunk, "data: [DONE]\n\n"}, 0, false))
	srv, _, _ := gatewayFor(t, "", []string{at(up, "")}, "m=p0/x")
	resp, events, err := readStream(t, srv, askStream)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.raw)
	}
	if want := []string{": ping\n\n", ": still here\n\n", chunk, "data: [DONE]\n\n"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the client got %q, want the provider's events byte for byte, %q", got, want)
	}
}

func TestAStreamThatEndsWithoutDoneIsBroken(t *testing.T) {
	// The provider ends its body cleanly after two chunks, without data: [DONE]: the answer may be incomplete, so
	// the client must not be able to take it for a finished one.
	up := newProvider(t, streaming([]string{chunk, chunk}, 0, false))
	srv, g, logs := gatewayFor(t, `"circuit_breaker":{"failures":1}`, []string{at(up, "")}, "m=p0/x")
	resp, events, err := readStream(t, srv, askStream)
	if resp.StatusCode != 200 || err == nil || len(events) != 3 {
		t.Fatalf("%d, %d events, %v", resp.StatusCode, len(events), err)
	}
	if code := lastEventError(t, events); code != "upstream_unavailable" {
		t.Errorf("code %q", code)
	}
	if x := lastExchange(t, logs); x.Code != "upstream_unavailable" || g.breakers["p0"].State().String() != "open" {
		t.Errorf("%+v, breaker %s", x, g.breakers["p0"].State())
	}
}

func TestStreamingWorksOverHTTP2BothWays(t *testing.T) {
	// The provider over https and HTTP/2, and the client too: flushing and the dropped connection are HTTP/2
	// frames there (DATA frames, and RST_STREAM for the abort), not chunks of an HTTP/1.1 body.
	var proto atomic.Value
	cut := pacedMock(t, "mock-a", 0, func(c *mock.Config) { c.CutRate, c.Models = 1, nil })
	whole := pacedMock(t, "mock-a", 0, nil)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		if strings.Contains(string(mustRead(r)), `"model":"cut"`) {
			cut.ServeHTTP(w, r)
			return
		}
		whole.ServeHTTP(w, r)
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	t.Cleanup(up.Close)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{"providers":[{"name":"tls","base_url":%q}],`+
		`"models":[{"name":"m","provider":"tls","upstream_model":"mock-small"},`+
		`{"name":"c","provider":"tls","upstream_model":"cut"}]}`, up.URL+"/v1")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTransport()
	tr.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	logs := &syncBuffer{}
	srv := httptest.NewUnstartedServer(New(cfg, tr, slog.New(slog.NewJSONHandler(logs, nil))))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	defer func() {
		if t.Failed() {
			t.Log(logs)
		}
	}()

	resp, events, err := readStream(t, srv, askStream)
	if err != nil || resp.ProtoMajor != 2 || resp.StatusCode != 200 || len(events) != 35 ||
		events[34].raw != "data: [DONE]\n\n" || proto.Load() != "HTTP/2.0" {
		t.Fatalf("%s %d, %d events, %v; upstream %v", resp.Proto, resp.StatusCode, len(events), err, proto.Load())
	}
	resp, events, err = readStream(t, srv, strings.Replace(askStream, `"m"`, `"c"`, 1))
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 || err == nil {
		t.Fatalf("%s %d, the body ended with %v; want a reset stream", resp.Proto, resp.StatusCode, err)
	}
	if code := lastEventError(t, events); code != "upstream_unavailable" {
		t.Errorf("code %q", code)
	}
}
