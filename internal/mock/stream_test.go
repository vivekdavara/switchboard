package mock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/openai"
)

// event is one server-sent event as the client read it, and when.
type event struct {
	data string
	at   time.Duration
}

// streamFrom posts body and reads the answer as server-sent events, until the stream ends; err is how it ended
// (nil for a clean end of the body).
func streamFrom(t *testing.T, srv *httptest.Server, body string) (*http.Response, []event, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	began := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []event
	r := bufio.NewReader(resp.Body)
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				err = nil
			}
			return resp, events, err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			events = append(events, event{strings.Join(data, "\n"), time.Since(began)})
			data = nil
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		default:
			t.Fatalf("not an SSE line: %q", line)
		}
	}
}

func chunkOf(t *testing.T, data string) openai.ChatCompletionChunk {
	t.Helper()
	var c openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil || c.Object != "chat.completion.chunk" {
		t.Fatalf("not a chunk: %s (%v)", data, err)
	}
	return c
}

func TestAStreamIsTheAnswerChunkByChunkThenDone(t *testing.T) {
	_, plain := start(t, fast())
	_, b := post(t, plain, hello+`,"n":2}`)
	whole := completion(t, b)

	p, srv := start(t, fast())
	resp, events, err := streamFrom(t, srv, hello+`,"n":2,"stream":true,"stream_options":{"include_usage":true}}`)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, err)
	}
	// Two role chunks, two chunks per word, two finish chunks, the usage, [DONE].
	if want := 2 + 2*32 + 2 + 2; len(events) != want {
		t.Fatalf("%d events, want %d", len(events), want)
	}
	if events[len(events)-1].data != "[DONE]" {
		t.Errorf("the last event is %q", events[len(events)-1].data)
	}
	content := map[int]string{}
	for i, e := range events[:len(events)-2] {
		c := chunkOf(t, e.data)
		if c.ID != whole.ID || c.Model != "mock-small" || c.Created != whole.Created || len(c.Choices) != 1 ||
			c.Usage != nil {
			t.Fatalf("event %d: %s", i, e.data)
		}
		ch := c.Choices[0]
		switch {
		case i < 2:
			if ch.Index != i || ch.Delta.Role != "assistant" || ch.Delta.Content == nil || *ch.Delta.Content != "" {
				t.Errorf("role chunk %d: %s", i, e.data)
			}
		case i >= len(events)-4:
			if ch.FinishReason == nil || *ch.FinishReason != "stop" || ch.Delta.Content != nil {
				t.Errorf("finish chunk %d: %s", i, e.data)
			}
		default:
			if ch.Index != i%2 || ch.Delta.Content == nil || ch.FinishReason != nil {
				t.Errorf("chunk %d: %s", i, e.data)
			}
			content[ch.Index] += *ch.Delta.Content
		}
	}
	// Put together, each choice is the answer the same request gets unstreamed.
	if content[0] != whole.Choices[0].Message.Content || content[1] != whole.Choices[1].Message.Content {
		t.Errorf("streamed %q\nplain    %q", content[0], whole.Choices[0].Message.Content)
	}
	usage := chunkOf(t, events[len(events)-2].data)
	if usage.Usage == nil || *usage.Usage != whole.Usage || len(usage.Choices) != 0 ||
		!strings.Contains(events[len(events)-2].data, `"choices":[]`) {
		t.Errorf("usage chunk %s, want the usage %+v", events[len(events)-2].data, whole.Usage)
	}
	if s := p.Stats(); s.Answered != 1 || s.Streamed != 1 {
		t.Errorf("%+v", s)
	}
}

func TestAStreamWithoutIncludeUsageHasNoUsage(t *testing.T) {
	_, srv := start(t, fast())
	for _, opts := range []string{"", `,"stream_options":null`, `,"stream_options":{}`,
		`,"stream_options":{"include_usage":false}`} {
		_, events, err := streamFrom(t, srv, hello+`,"stream":true,"max_tokens":3`+opts+`}`)
		if err != nil || len(events) != 1+3+1+1 {
			t.Fatalf("%s: %d events, %v", opts, len(events), err)
		}
		finish := chunkOf(t, events[4].data)
		if finish.Usage != nil || *finish.Choices[0].FinishReason != "length" {
			t.Errorf("%s: %s", opts, events[4].data)
		}
	}
}

func TestAStreamKeepsTheModelsPace(t *testing.T) {
	cfg := fast()
	cfg.TTFT, cfg.PerToken = 150*time.Millisecond, 30*time.Millisecond
	_, srv := start(t, cfg)
	_, events, err := streamFrom(t, srv, hello+`,"stream":true,"max_tokens":4}`)
	if err != nil || len(events) != 1+4+1+1 {
		t.Fatalf("%d events, %v", len(events), err)
	}
	// The role chunk at the time to first token, then a token's time apart: lower bounds only, since a busy machine
	// can only make it later.
	if events[0].at < 150*time.Millisecond {
		t.Errorf("the first event came after %s, before the time to first token", events[0].at)
	}
	for i := 1; i <= 4; i++ {
		if gap := events[i].at - events[i-1].at; gap < 25*time.Millisecond {
			t.Errorf("token %d came %s after the one before, want about 30ms", i, gap)
		}
	}
	if events[6].at > 2*time.Second {
		t.Errorf("the stream took %s, want about 270ms", events[6].at)
	}
}

func TestACutStreamStopsWithoutDone(t *testing.T) {
	cfg := fast()
	cfg.CutRate = 1
	p, srv := start(t, cfg)
	for i := range 20 {
		_, events, err := streamFrom(t, srv, hello+`,"stream":true}`)
		if err == nil {
			t.Fatalf("stream %d ended cleanly", i)
		}
		if len(events) < 1 || len(events) > 1+31 {
			t.Fatalf("stream %d: %d events before the cut", i, len(events))
		}
		for _, e := range events {
			if e.data == "[DONE]" {
				t.Fatalf("stream %d: [DONE] before the cut", i)
			}
		}
	}
	if s := p.Stats(); s.Cut != 20 || s.Answered != 0 {
		t.Errorf("%+v", s)
	}
}

func TestACutAnswerIsShorterThanItsLength(t *testing.T) {
	cfg := fast()
	cfg.CutRate = 1
	p, srv := start(t, cfg)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(hello+`}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength <= 0 {
		t.Fatalf("%d, length %d", resp.StatusCode, resp.ContentLength)
	}
	b, err := io.ReadAll(resp.Body)
	if err != io.ErrUnexpectedEOF || int64(len(b)) >= resp.ContentLength {
		t.Errorf("read %d of %d bytes, then %v", len(b), resp.ContentLength, err)
	}
	if s := p.Stats(); s.Cut != 1 || s.Answered != 0 {
		t.Errorf("%+v", s)
	}
}

func cutsAt(t *testing.T, cutRate, errorRate float64, n int) []string {
	t.Helper()
	cfg := fast()
	cfg.CutRate, cfg.ErrorRate, cfg.Seed = cutRate, errorRate, 7
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for range n {
		switch f := p.draw(); {
		case f.fail:
			out = append(out, "fail")
		case f.cut:
			out = append(out, fmt.Sprintf("cut at %.3f", f.cutAt))
		default:
			out = append(out, "answer")
		}
	}
	return out
}

func TestARaisedCutRateOnlyAddsCuts(t *testing.T) {
	low, high := cutsAt(t, 0.1, 0.2, 1000), cutsAt(t, 0.4, 0.2, 1000)
	more := 0
	for i := range low {
		if low[i] != high[i] {
			if !strings.HasPrefix(high[i], "cut") || (low[i] != "answer" && !strings.HasPrefix(low[i], "cut")) {
				t.Fatalf("request %d: %s at cut rate 0.1, %s at 0.4", i, low[i], high[i])
			}
			if low[i] == "answer" {
				more++
			}
		}
	}
	// About 0.8 x 0.3 x 1,000 = 240 answers become cuts (failures take precedence over cuts).
	if more < 180 || more > 300 {
		t.Errorf("%d answers became cuts", more)
	}
	// And the cut rate moves nothing else: the failures are where they were with no cuts at all.
	none := cutsAt(t, 0, 0.2, 1000)
	for i := range none {
		if (none[i] == "fail") != (high[i] == "fail") {
			t.Fatalf("request %d: %s without cuts, %s with", i, none[i], high[i])
		}
	}
}

func TestACutIsDecidedByTheFourthDraw(t *testing.T) {
	// DESIGN.md: each request takes four numbers from a PCG seeded with the seed; the fourth decides a cut (below
	// the cut rate) and, divided by the rate, where it breaks. Draw the same numbers here and compare.
	cfg := fast()
	cfg.CutRate, cfg.Seed = 0.3, 11
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ref := rand.New(rand.NewPCG(11, 0x5157))
	cuts := 0
	for i := range 2000 {
		ref.Float64()
		ref.Float64()
		ref.Float64()
		c := ref.Float64()
		f := p.draw()
		if f.cut != (c < 0.3) || (f.cut && f.cutAt != c/0.3) {
			t.Fatalf("request %d: cut %v at %v; the fourth draw %v says %v at %v", i, f.cut, f.cutAt, c, c < 0.3, c/0.3)
		}
		if f.cut {
			cuts++
		}
	}
	if cuts < 500 || cuts > 700 {
		t.Errorf("%d cuts in 2,000 at 0.3", cuts)
	}
}
