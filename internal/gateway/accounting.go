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

// unasked is what to send in place of a stream's event when the gateway asked for the usage the client didn't: a
// chunk that is only the usage (no choices, or an empty list of them, as OpenAI sends it last) is dropped (nil); a
// usage member on any other chunk (OpenAI puts "usage": null on every chunk when asked) is taken out, and the rest of
// the chunk written compacted on one data line. Every other event goes as it came, byte for byte.
func unasked(ev sseEvent) []byte {
	if ev.data == nil || !bytes.Contains(ev.data, []byte(`"usage"`)) {
		return ev.raw
	}
	members, err := jsonstrict.ParseObject(ev.data)
	if err != nil {
		return ev.raw
	}
	var kept []jsonstrict.Member
	hasUsage, choices := false, false
	for _, m := range members {
		switch m.Key {
		case "usage":
			hasUsage = true
			continue
		case "choices":
			var list []json.RawMessage
			choices = json.Unmarshal(m.Value, &list) != nil || len(list) > 0
		}
		kept = append(kept, m)
	}
	switch {
	case !hasUsage:
		return ev.raw
	case !choices:
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
	var b bytes.Buffer
	b.WriteString("data: ")
	json.Compact(&b, obj.Bytes()) // can't fail: the members came from a valid object
	b.WriteString("\n\n")
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
