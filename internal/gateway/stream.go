package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/vivekdavara/switchboard/internal/breaker"
	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/openai"
)

// Streaming (DESIGN.md, "Streaming"). A streamed answer can't be held until it is complete, and once its 200 and its
// first event are out its status can't change. So the gateway reads the provider's first data event before it writes
// anything: until then a failure is like any other and the request can fall back to another target. The first event
// commits the response: the gateway writes the headers and that event, then every event as it arrives, flushed. If
// the stream breaks after that (the provider broke off, stopped making progress, or the gateway is stopping), the
// gateway writes one last event in OpenAI's error shape, which the OpenAI SDK raises as an APIError, and drops the
// connection, so even a client that ignores the event sees a broken transfer rather than a stream that looks
// complete. A stream that ends cleanly ends with data: [DONE].

var (
	errStreamEnded  = errors.New("the stream ended before data: [DONE]")
	errStreamTooBig = errors.New("the stream is larger than max_response_bytes")
)

// sseEvent is one server-sent event, as the provider wrote it.
type sseEvent struct {
	raw  []byte // its lines and the blank line after them, byte for byte
	data []byte // its data lines' values, joined with newlines; nil if it has none (only comments or other fields)
}

func (e sseEvent) done() bool { return bytes.Equal(e.data, []byte(openai.Done)) }

// eventReader splits a stream into events: lines ending in \n (or \r\n), each event ending at a blank line. It reads
// at most limit bytes in all.
type eventReader struct {
	r     *bufio.Reader
	limit int64
	read  int64
}

func newEventReader(body io.Reader, limit int64) *eventReader {
	return newEventReaderSize(body, limit, 32<<10)
}

// newEventReaderSize is newEventReader with a buffer of size bytes (at least 16), so the fuzz test can make lines
// longer than the buffer common.
func newEventReaderSize(body io.Reader, limit int64, size int) *eventReader {
	return &eventReader{r: bufio.NewReaderSize(body, size), limit: limit}
}

// next returns the next event. At the end of the stream it returns io.EOF if nothing of an event was read, and
// errStreamEnded if the stream stopped part-way through one.
func (er *eventReader) next() (sseEvent, error) {
	var ev sseEvent
	var data [][]byte
	for {
		line, err := er.line()
		if err != nil {
			if err == io.EOF && len(ev.raw) > 0 {
				err = errStreamEnded
			}
			return ev, err
		}
		ev.raw = append(ev.raw, line...)
		text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(text) == 0 {
			if len(ev.raw) == len(line) { // a blank line with no event before it: nothing to dispatch
				ev.raw = ev.raw[:0]
				continue
			}
			if data != nil {
				// Never nil for an event with a data line, even an empty one ("data:" is an event whose data is
				// ""): bytes.Join of one empty value returns nil (FuzzEventReaderAgreesWithTheReference found it).
				ev.data = append([]byte{}, bytes.Join(data, []byte("\n"))...)
			}
			return ev, nil
		}
		if v, ok := bytes.CutPrefix(text, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(v, []byte(" ")))
		}
	}
}

// line reads one line, its \n included.
func (er *eventReader) line() ([]byte, error) {
	var line []byte
	for {
		chunk, err := er.r.ReadSlice('\n')
		er.read += int64(len(chunk))
		if er.read > er.limit {
			return nil, errStreamTooBig
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue // a long line: keep reading it, within the limit
		case err == io.EOF && len(line) > 0:
			return nil, errStreamEnded
		default:
			return nil, err
		}
	}
}

// eventStream says whether a Content-Type is text/event-stream.
func eventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// relay reads a streaming provider's answer and, once its first data event is in, writes the whole stream to the
// client. Until then it writes nothing and returns a result like any attempt's; once it has written, its result says
// so (written), and the stream has ended one way or the other.
//
// timer is the attempt's timeout, running since the request was sent. It runs only while the gateway waits for the
// provider: it is stopped while an event is written to the client and started again for the next, so a client that
// reads slowly (blocking the writes) isn't taken for a provider making no progress. (Before this, a client that paused
// for longer than the timeout got a healthy stream cut as upstream_timeout, and the provider's breaker counted it.)
//
// ticket is the attempt's breaker ticket. If it is a half-open breaker's probe, the first event reports the
// provider's success on it at the commit: the breaker mustn't stay half-open (skipping a healthy provider for every
// other request) for as long as the stream lasts, which is as long as its client cares to read (found in the day-2
// review); a break after that is a failure seen then. Any other ticket is reported when the stream ends, as a plain
// request's is, so breaks count in a row.
func (g *Gateway) relay(w http.ResponseWriter, r *http.Request, x *exchange, ctx context.Context, p *config.Provider,
	attempt int, timer *time.Timer, ticket *breaker.Ticket, resp *http.Response) result {
	defer resp.Body.Close()
	events := newEventReader(resp.Body, g.cfg.MaxResponseBytes)
	var first sseEvent
	var pending [][]byte // comments and the like before the first data event, sent with it
	for first.data == nil {
		ev, err := events.next()
		if err != nil {
			if err == io.EOF || err == errStreamEnded {
				return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
					"upstream_error", "the provider %s ended its stream before its first event", p.Name), cause: err}
			}
			if err == errStreamTooBig {
				return result{kind: answered, err: upstreamError(http.StatusBadGateway, "upstream_error",
					"the provider %s streamed more than %d bytes", p.Name, g.cfg.MaxResponseBytes)}
			}
			return g.unreached(r, ctx, p, err, true)
		}
		if ev.data == nil {
			pending = append(pending, ev.raw)
			continue
		}
		first = ev
	}
	// The first data event must be the start of an answer. An error object is the provider saying no, as a status
	// would: another target may be tried. [DONE] with nothing before it is a stream with no answer in it, and
	// anything but a JSON object is no chunk: like a 200 without JSON, the provider may have done the work. (A
	// [DONE]-only stream committed an empty 200 and told the breaker it succeeded, found in the day-2 review.)
	switch e, isError := streamError(first.data); {
	case isError:
		return result{kind: failed, health: unhealthy, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s answered its stream with an error: %s", p.Name, e)}
	case first.done():
		return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_error", "the provider %s ended its stream before its first chunk", p.Name)}
	case !jsonObject(first.data):
		return result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_error", "the provider %s streamed a first event that isn't a JSON chunk", p.Name)}
	}

	// The first event commits the response. A probe reports its provider's success now (Ticket.Answered); any
	// other attempt reports when the stream ends.
	timer.Stop()
	ticket.Answered()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Switchboard-Provider", p.Name)
	h.Set("X-Switchboard-Attempts", strconv.Itoa(attempt))
	w.WriteHeader(http.StatusOK)
	x.status, x.streamed = http.StatusOK, true
	rc := http.NewResponseController(w)
	send := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		x.responseBytes += len(b)
		return rc.Flush() == nil
	}
	for _, b := range append(pending, first.raw) {
		if !send(b) {
			return result{written: true, gone: true, cause: errClientWrite}
		}
	}
	x.events = 1
	// Only a data event is progress: the time spent waiting through comments (pings) is kept, so a provider that
	// sends pings and nothing else still times out (before, a ping restarted the whole timeout; a suspicion of the
	// day-2 review).
	remaining := p.Timeout
	for ev := first; !ev.done(); {
		timer.Reset(remaining)
		waited := time.Now()
		var err error
		ev, err = events.next()
		timer.Stop()
		if err != nil {
			return g.endStream(w, r, x, ctx, p, err)
		}
		if ev.data != nil {
			remaining = p.Timeout
		} else {
			remaining -= time.Since(waited)
		}
		if !send(ev.raw) {
			return result{written: true, gone: true, cause: errClientWrite}
		}
		if ev.data != nil && !ev.done() {
			x.events++
		}
	}
	return result{written: true, kind: answered, health: healthy}
}

var errClientWrite = errors.New("writing to the client failed")

// endStream ends a stream that broke after its first event: the client went away (drop the connection), or the
// provider broke off, stopped making progress or sent too much, or the gateway is stopping. Then the client gets one
// last event saying what happened, and the connection is dropped.
func (g *Gateway) endStream(w http.ResponseWriter, r *http.Request, x *exchange, ctx context.Context,
	p *config.Provider, err error) result {
	var res result
	switch err {
	case errStreamEnded, io.EOF:
		res = result{kind: unanswered, health: unhealthy, err: upstreamError(http.StatusBadGateway,
			"upstream_unavailable", "the provider %s ended the stream before data: [DONE]", p.Name), cause: err}
	case errStreamTooBig:
		res = result{kind: answered, err: upstreamError(http.StatusBadGateway, "upstream_error",
			"the provider %s streamed more than %d bytes", p.Name, g.cfg.MaxResponseBytes), cause: err}
	default:
		res = g.unreached(r, ctx, p, err, true)
	}
	res.written = true
	if res.gone {
		return res
	}
	b, _ := json.Marshal(res.err)
	fmt.Fprintf(w, "data: %s\n\n", b)
	http.NewResponseController(w).Flush()
	x.code, x.err = res.err.Code, res.cause
	return res
}

// streamError says whether an event's data is an error object rather than a chunk, and returns its message.
func streamError(data []byte) (string, bool) {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == nil {
		return "", false
	}
	return e.Error.Message, true
}
