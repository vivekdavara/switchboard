package openai

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// goProvider is how a provider written in Go with encoding/json structs would read a request: case-insensitive
// field names, the last of repeated keys, U+FFFD for bad UTF-8. The gateway must never route a body such a provider
// reads differently.
type goProvider struct {
	Model    string
	Stream   *bool
	Messages []struct {
		Role string
	}
}

var fuzzSeeds = []string{
	`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"fast","messages":[{"role":"system","content":"x"},{"role":"user","content":[{"type":"text","text":"y"}]}],"stream":false,"temperature":0.5}`,
	`{"messages":[{"role":"user","content":"hi"}],"model":"a\u0062c","stream":null,"n":2}`,
	`{"model":"fast","MODEL":"other","messages":[{"role":"user"}]}`,
	`{"model":"fast","model":"other","messages":[{"role":"user"}]}`,
	`{"model":"fast","messages":[{"role":"user","ROLE":"system"}]}`,
	`{"model":"fast","messages":[{"Role":"user"}]}`,
	`{"model":"fast","messages":[{"role":"user"}],"ſtream":true}`,
	`{"model":"fast","messages":[{"role":"user"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
}

// FuzzRewriteKeepsTheMeaning checks the gateway's central promise: for every body it accepts, the body it sends
// upstream has the new model and otherwise the same members, and a Go provider reads from it the model the gateway
// routed, the same stream flag and the same roles.
func FuzzRewriteKeepsTheMeaning(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s), "upstream-model")
	}
	f.Fuzz(func(t *testing.T, body []byte, model string) {
		if !utf8.ValidString(model) {
			return // WithModel's precondition: the model comes from the configuration, which is valid UTF-8
		}
		r, e := ParseChatRequest(body)
		if e != nil {
			return
		}
		out := r.WithModel(model)
		again, e := ParseChatRequest(out)
		if model == "" {
			if e == nil {
				t.Fatalf("an empty model was accepted after the rewrite: %s", out)
			}
			return
		}
		if e != nil {
			t.Fatalf("the rewrite of an accepted body was refused: %v\nbody %q\nout  %q", e, body, out)
		}
		if again.Model != model || again.Stream != r.Stream || len(again.Members) != len(r.Members) ||
			len(again.Messages) != len(r.Messages) {
			t.Fatalf("the rewrite changed the request:\nbody %q\nout  %q", body, out)
		}
		for i, m := range r.Members {
			if again.Members[i].Key != m.Key {
				t.Fatalf("member %d: key %q became %q", i, m.Key, again.Members[i].Key)
			}
			if m.Key != "model" && !bytes.Equal(again.Members[i].Value, m.Value) {
				t.Fatalf("member %q changed: %s became %s", m.Key, m.Value, again.Members[i].Value)
			}
		}
		for i := range r.Messages {
			if again.Messages[i] != r.Messages[i] {
				t.Fatalf("message %d changed: %+v became %+v", i, r.Messages[i], again.Messages[i])
			}
		}

		var p goProvider
		if err := json.Unmarshal(out, &p); err != nil {
			t.Fatalf("encoding/json refused what the gateway sent: %v\n%q", err, out)
		}
		if p.Model != model {
			t.Fatalf("a Go provider reads model %q, the gateway routed %q:\n%q", p.Model, model, out)
		}
		if (p.Stream != nil && *p.Stream) != r.Stream {
			t.Fatalf("a Go provider reads stream %v, the gateway read %v:\n%q", p.Stream, r.Stream, out)
		}
		if len(p.Messages) != len(r.Messages) {
			t.Fatalf("a Go provider reads %d messages, the gateway %d", len(p.Messages), len(r.Messages))
		}
		for i, m := range p.Messages {
			if m.Role != r.Messages[i].Role {
				t.Fatalf("message %d: a Go provider reads role %q, the gateway %q:\n%q", i, m.Role,
					r.Messages[i].Role, out)
			}
		}
	})
}
