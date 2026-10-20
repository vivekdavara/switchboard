package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/jsonstrict"
	"github.com/vivekdavara/switchboard/internal/openai"
	"github.com/vivekdavara/switchboard/internal/usage"
)

// Accounting (DESIGN.md, "Accounting"): every completion's usage, as its provider reported it, priced at the
// provider's price for its upstream model, in the request's log line and, with Redis, in the client's daily totals.
// A plain answer always carries its usage. A stream carries it only if stream_options.include_usage asks, so for a
// client that didn't ask, the gateway asks on its behalf (unless the provider's stream_usage is off) and takes the
// usage out of the stream again before the client sees it: the client gets the stream it asked for, and the gateway
// still knows what it cost.

// upstreamBody is the body to send a target: the client's, with the target's upstream model, and asking for a
// stream's usage if the client didn't and the provider may be asked. strip says the gateway asked, so the relay must
// take the usage out.
func upstreamBody(req *openai.ChatRequest, t config.Target) (body []byte, strip bool) {
	if req.Stream && t.Provider.StreamUsage && !req.WantsUsage() {
		if b, ok := req.WithModelAskingUsage(t.UpstreamModel); ok {
			return b, true
		}
	}
	return req.WithModel(t.UpstreamModel), false
}

// unasked is what to send in place of a stream's event when the gateway asked for the usage the client didn't: that
// usage taken out, and nothing else. A chunk that carries a usage object and no choices (OpenAI's last chunk, which a
// client that didn't ask never gets) is dropped (nil). A usage member on any other chunk (the "usage": null OpenAI
// puts on every chunk when asked, or a usage that comes with the last choices) is removed: the event's data line is
// written again without it, its other lines (id:, event:, comments) as they were. Every other event goes as it came,
// byte for byte. (The first version dropped every chunk with a usage member and no choices, even "usage": null, which
// lost Azure's first chunk, and wrote every rewritten event as a lone data line; both found in the day-3 review.)
func unasked(ev sseEvent) []byte {
	if ev.data == nil || !bytes.Contains(ev.data, []byte(`"usage"`)) {
		return ev.raw
	}
	members, err := jsonstrict.ParseObject(ev.data)
	if err != nil {
		return ev.raw
	}
	var kept []jsonstrict.Member
	var usage json.RawMessage
	choices := false
	for _, m := range members {
		switch m.Key {
		case "usage":
			usage = m.Value
			continue
		case "choices":
			var list []json.RawMessage
			choices = json.Unmarshal(m.Value, &list) != nil || len(list) > 0
		}
		kept = append(kept, m)
	}
	switch {
	case usage == nil:
		return ev.raw
	case !choices && !bytes.Equal(usage, []byte("null")):
		return nil
	}
	var obj bytes.Buffer
	obj.WriteByte('{')
	for i, m := range kept {
		if i > 0 {
			obj.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		obj.Write(k)
		obj.WriteByte(':')
		obj.Write(m.Value)
	}
	obj.WriteByte('}')
	data := obj.Bytes()
	if bytes.ContainsAny(data, "\r\n") { // a value spread over several data lines: one line can't hold its newlines
		var compact bytes.Buffer
		json.Compact(&compact, data) // can't fail: the members came from a valid object
		data = compact.Bytes()
	}
	return withData(ev.raw, data)
}

// withData is an event with its data lines replaced by one line holding data, where the first of them was; every other
// line is kept as it was.
func withData(raw, data []byte) []byte {
	var b bytes.Buffer
	wrote := false
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line = raw[:i+1]
		}
		raw = raw[len(line):]
		text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		lead := []byte(nil)
		if t, ok := bytes.CutPrefix(text, bom); ok { // only the stream's first line can have one
			lead, text = bom, t
		}
		if !bytes.HasPrefix(text, []byte("data:")) && !bytes.Equal(text, []byte("data")) {
			b.Write(line)
			continue
		}
		if !wrote {
			b.Write(lead)
			b.WriteString("data: ")
			b.Write(data)
			b.WriteByte('\n')
			wrote = true
		}
	}
	return b.Bytes()
}

// account counts a completion once the client has it: its usage (read from the answer, or from the stream's chunks
// as they passed), its cost, and the client's daily totals.
func (g *Gateway) account(r *http.Request, x *exchange, answer []byte) {
	if answer != nil {
		x.usage, x.hasUsage = usage.FromAnswer(answer)
	}
	e := usage.Entry{Completions: 1}
	switch p, priced := g.cfg.Price(x.provider, x.upstreamModel); {
	case !x.hasUsage:
		e.NoUsage = 1
	case !priced:
		e.Tokens, e.Unpriced = x.usage, 1
	default:
		x.cost, x.priced = usage.Cost(x.usage, p), true
		e.Tokens, e.Cost = x.usage, x.cost
	}
	g.addTotals(r, x, e)
}

// addTotals adds to the client's totals for the request's model, if there is a Redis. It runs after the answer, so
// a client that has left by then doesn't stop it; Redis's timeout bounds it.
func (g *Gateway) addTotals(r *http.Request, x *exchange, e usage.Entry) {
	if g.totals == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), g.cfg.Redis.Timeout)
	defer cancel()
	if err := g.totals.Add(ctx, x.began, clientName(clientOf(r)), x.route, e); err != nil {
		x.totalsErr = err
	}
}
