package openai

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

const streamed = `{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true`

func TestWantsUsageReadsIncludeUsage(t *testing.T) {
	for opts, want := range map[string]bool{
		``: false, `,"stream_options":{"include_usage":true}`: true, `,"stream_options":{"include_usage":false}`: false,
		`,"stream_options":{"include_usage":"true"}`: false, `,"stream_options":{}`: false,
		`,"stream_options":true`: false, `,"stream_options":null`: false,
		`,"stream_options":{"x":{"include_usage":true}}`: false,
	} {
		r, e := ParseChatRequest([]byte(streamed + opts + `}`))
		if e != nil {
			t.Fatalf("%s: %v", opts, e)
		}
		if got := r.WantsUsage(); got != want {
			t.Errorf("%s: %v, want %v", opts, got, want)
		}
	}
}

func TestAskingForUsageChangesOnlyIncludeUsage(t *testing.T) {
	const up = `{"model":"up","messages":[{"role":"user","content":"hi"}],"stream":true`
	for _, tc := range []struct {
		opts, want string
		ok         bool
	}{
		{``, up + `,"stream_options":{"include_usage":true}}`, true},
		{`,"stream_options":{}`, up + `,"stream_options":{"include_usage":true}}`, true},
		{`,"stream_options":{"include_usage":false}`, up + `,"stream_options":{"include_usage":true}}`, true},
		{`,"stream_options":{"include_usage":false,"x":[1, 2]}`,
			up + `,"stream_options":{"include_usage":true,"x":[1, 2]}}`, true},
		{`,"stream_options":{"x":1}`, up + `,"stream_options":{"x":1,"include_usage":true}}`, true},
		{`,"stream_options":{ "x" : 1 }`, up + `,"stream_options":{"x":1,"include_usage":true}}`, true},
		{`,"stream_options":"yes"`, up + `,"stream_options":"yes"}`, false},
		{`,"stream_options":null`, up + `,"stream_options":null}`, false},
		{`,"stream_options":{"include_usage":"yes"}`, up + `,"stream_options":{"include_usage":"yes"}}`, false},
		{`,"stream_options":{"include_usage":1}`, up + `,"stream_options":{"include_usage":1}}`, false},
	} {
		r, e := ParseChatRequest([]byte(streamed + tc.opts + `}`))
		if e != nil {
			t.Fatalf("%s: %v", tc.opts, e)
		}
		got, ok := r.WithModelAskingUsage("up")
		if string(got) != tc.want || ok != tc.ok {
			t.Errorf("%s:\n got %s %v\nwant %s %v", tc.opts, got, ok, tc.want, tc.ok)
		}
	}
	// stream_options in the middle stays where it was.
	r, _ := ParseChatRequest([]byte(`{"stream_options":{},"model":"fast","stream":true,"messages":[{"role":"user"}]}`))
	if got, _ := r.WithModelAskingUsage("up"); string(got) !=
		`{"stream_options":{"include_usage":true},"model":"up","stream":true,"messages":[{"role":"user"}]}` {
		t.Errorf("%s", got)
	}
}

func TestIncludeUsageInAnotherCaseIsRefused(t *testing.T) {
	for opts, param := range map[string]string{
		`,"stream_options":{"Include_Usage":true}`:  "stream_options.Include_Usage",
		`,"stream_options":{"INCLUDE_USAGE":false}`: "stream_options.INCLUDE_USAGE",
		`,"Stream_Options":{"include_usage":true}`:  "Stream_Options",
	} {
		_, e := ParseChatRequest([]byte(streamed + opts + `}`))
		if e == nil || e.Status != 400 || e.Param != param || e.Code != "invalid_parameter" {
			t.Errorf("%s: %+v", opts, e)
		}
	}
}

// goStreamOptions is how a Go provider reads the request's stream options.
type goStreamOptions struct {
	Model         string
	StreamOptions *struct {
		IncludeUsage *bool `json:"include_usage"`
	} `json:"stream_options"`
}

// FuzzAskingForUsageKeepsTheRest checks that asking for a stream's usage changes nothing else: every member but model
// and stream_options is byte for byte the client's, stream_options keeps its other members, and a Go provider reads
// include_usage true (and the model routed).
func FuzzAskingForUsageKeepsTheRest(f *testing.F) {
	for _, s := range append(fuzzSeeds,
		streamed+`}`, streamed+`,"stream_options":{"include_usage":false,"x":1}}`,
		streamed+`,"stream_options":{}}`, `{"stream_options":{"a":[]},"model":"m","messages":[{"role":"user"}]}`,
		streamed+`,"stream_options":{"include_usage":"no"}}`) {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r, e := ParseChatRequest(body)
		if e != nil {
			return
		}
		out, ok := r.WithModelAskingUsage("up")
		if !ok {
			if !bytes.Equal(out, r.WithModel("up")) {
				t.Fatalf("not ok, yet not WithModel's body: %q", out)
			}
			return
		}
		again, e := ParseChatRequest(out)
		if e != nil {
			t.Fatalf("refused after asking for usage: %v\nbody %q\nout  %q", e, body, out)
		}
		if !again.WantsUsage() || again.Model != "up" || again.Stream != r.Stream {
			t.Fatalf("the request asks for no usage, or changed: %q", out)
		}
		before := map[string]json.RawMessage{}
		for _, m := range r.Members {
			before[m.Key] = m.Value
		}
		for _, m := range again.Members {
			if m.Key != "model" && m.Key != "stream_options" && !bytes.Equal(before[m.Key], m.Value) {
				t.Fatalf("member %q changed: %s became %s", m.Key, before[m.Key], m.Value)
			}
		}
		oldOpts, _ := r.streamOptions()
		newOpts, _ := again.streamOptions()
		kept := map[string]json.RawMessage{}
		for _, m := range newOpts {
			kept[m.Key] = m.Value
		}
		for _, m := range oldOpts {
			if m.Key != "include_usage" && !bytes.Equal(kept[m.Key], m.Value) {
				t.Fatalf("stream_options.%s changed: %s became %s", m.Key, m.Value, kept[m.Key])
			}
		}
		var p goStreamOptions
		if err := json.Unmarshal(out, &p); err != nil || !utf8.Valid(out) {
			t.Fatalf("encoding/json refused it: %v\n%q", err, out)
		}
		if p.Model != "up" || p.StreamOptions == nil || p.StreamOptions.IncludeUsage == nil ||
			!*p.StreamOptions.IncludeUsage {
			t.Fatalf("a Go provider doesn't read include_usage true: %q", out)
		}
	})
}
