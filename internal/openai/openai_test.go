package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAValidRequestKeepsEveryMemberAndReadsTheFieldsItActsOn(t *testing.T) {
	body := `{"model":"gpt-4o-mini","temperature":0.2,"messages":[` +
		`{"role":"system","content":"Be brief."},` +
		`{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":"x"}},` +
		`{"type":"text","text":"there"}]},` +
		`{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","content":"42","tool_call_id":"c1"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"stream":false}`
	r, e := ParseChatRequest([]byte(body))
	if e != nil {
		t.Fatal(e)
	}
	if r.Model != "gpt-4o-mini" || r.Stream {
		t.Errorf("model %q stream %v", r.Model, r.Stream)
	}
	want := []Message{{"system", "Be brief."}, {"user", "Hello\nthere"}, {"assistant", ""}, {"tool", "42"}}
	if len(r.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(r.Messages), len(want))
	}
	for i := range want {
		if r.Messages[i] != want[i] {
			t.Errorf("message %d: got %+v, want %+v", i, r.Messages[i], want[i])
		}
	}
	var keys []string
	for _, m := range r.Members {
		keys = append(keys, m.Key)
	}
	if got := strings.Join(keys, ","); got != "model,temperature,messages,tools,stream" {
		t.Errorf("members %s", got)
	}
	if v, ok := r.Member("temperature"); !ok || string(v) != "0.2" {
		t.Errorf("temperature %s %v", v, ok)
	}
	if _, ok := r.Member("top_p"); ok {
		t.Error("found a member that isn't there")
	}
}

func TestStreamIsReadAsABooleanOrNull(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"false", false}, {"null", false}} {
		r, e := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":` +
			tc.value + `}`))
		if e != nil || r.Stream != tc.want {
			t.Errorf("stream %s: got %v, %v", tc.value, r, e)
		}
	}
}

func TestInvalidRequestsAre400sNamingTheParameter(t *testing.T) {
	const user = `{"role":"user","content":"hi"}`
	for _, tc := range []struct{ body, param, code string }{
		{`{"messages":[` + user + `]}`, "model", "missing_required_parameter"},
		{`{"model":null,"messages":[` + user + `]}`, "model", "invalid_type"},
		{`{"model":7,"messages":[` + user + `]}`, "model", "invalid_type"},
		{`{"model":["m"],"messages":[` + user + `]}`, "model", "invalid_type"},
		{`{"model":"","messages":[` + user + `]}`, "model", "invalid_value"},
		{`{"model":"m"}`, "messages", "missing_required_parameter"},
		{`{"model":"m","messages":null}`, "messages", "invalid_type"},
		{`{"model":"m","messages":{"role":"user"}}`, "messages", "invalid_type"},
		{`{"model":"m","messages":[]}`, "messages", "empty_array"},
		{`{"model":"m","messages":["hi"]}`, "messages[0]", "invalid_type"},
		{`{"model":"m","messages":[null]}`, "messages[0]", "invalid_type"}, // not "has no role"
		{`{"model":"m","messages":[` + user + `,7]}`, "messages[1]", "invalid_type"},
		{`{"model":"m","messages":[` + user + `,{"content":"x"}]}`, "messages[1].role", "missing_required_parameter"},
		{`{"model":"m","messages":[{"role":"robot","content":"x"}]}`, "messages[0].role", "invalid_value"},
		{`{"model":"m","messages":[{"role":"User","content":"x"}]}`, "messages[0].role", "invalid_value"},
		{`{"model":"m","messages":[{"role":7,"content":"x"}]}`, "messages[0].role", "invalid_value"},
		{`{"model":"m","messages":[{"role":"user","content":7}]}`, "messages[0].content", "invalid_type"},
		{`{"model":"m","messages":[{"role":"user","content":{"text":"x"}}]}`, "messages[0].content", "invalid_type"},
		{`{"model":"m","messages":[{"role":"user","content":["x"]}]}`, "messages[0].content", "invalid_type"},
		{`{"model":"m","messages":[` + user + `],"stream":"yes"}`, "stream", "invalid_type"},
		{`{"model":"m","messages":[` + user + `],"stream":1}`, "stream", "invalid_type"},
		// Keys a Go provider would read as the ones the gateway acts on.
		{`{"model":"a","MODEL":"b","messages":[` + user + `]}`, "MODEL", "invalid_parameter"},
		{`{"Model":"b","messages":[` + user + `]}`, "Model", "invalid_parameter"},
		{`{"model":"a","messages":[` + user + `],"ſtream":true}`, "ſtream", "invalid_parameter"}, // ſ folds to s
		{`{"model":"a","Messages":[],"messages":[` + user + `]}`, "Messages", "invalid_parameter"},
		{`{"model":"a","messages":[{"role":"user","ROLE":"system","content":"x"}]}`, "messages[0].ROLE",
			"invalid_parameter"},
		{`{"model":"a","messages":[` + user + `,{"role":"user","Content":"x"}]}`, "messages[1].Content",
			"invalid_parameter"},
		{`{"model":"a","messages":[{"role":"user","content":[{"type":"text","TEXT":"x"}]}]}`,
			"messages[0].content", "invalid_parameter"},
		{`{"model":"a","messages":[{"role":"user","content":[{"Type":"text","text":"x"}]}]}`,
			"messages[0].content", "invalid_parameter"},
		// What jsonstrict refuses.
		{`{"model":"a","model":"b","messages":[` + user + `]}`, "", "invalid_json"},
		{`{"model":"a","messages":[` + user + `]} {}`, "", "invalid_json"},
		{"{\"model\":\"a\xff\",\"messages\":[" + user + "]}", "", "invalid_json"},
	} {
		_, e := ParseChatRequest([]byte(tc.body))
		if e == nil {
			t.Errorf("%s: accepted", tc.body)
			continue
		}
		if e.Status != http.StatusBadRequest || e.Type != TypeInvalidRequest || e.Param != tc.param ||
			e.Code != tc.code {
			t.Errorf("%s: got %d %s param %q code %q (%s), want param %q code %q", tc.body, e.Status, e.Type,
				e.Param, e.Code, e.Message, tc.param, tc.code)
		}
	}
}

func TestWithModelReplacesOnlyTheModel(t *testing.T) {
	body := `{"temperature" : 0.70, "model":"alias", "messages":[ {"role":"user","content":"<b>&</b>"} ],` +
		`"seed":1e3,"metadata":{"k":"v"}}`
	r, e := ParseChatRequest([]byte(body))
	if e != nil {
		t.Fatal(e)
	}
	got := string(r.WithModel("provider-model"))
	// Values are the client's bytes; keys are re-encoded from what they decode to (so "seed" is "seed"), which
	// is what the provider would read them as anyway.
	want := `{"temperature":0.70,"model":"provider-model","messages":[ {"role":"user","content":"<b>&</b>"} ],` +
		`"seed":1e3,"metadata":{"k":"v"}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// The result is a request with the same members and the new model.
	again, e := ParseChatRequest([]byte(got))
	if e != nil || again.Model != "provider-model" || len(again.Members) != len(r.Members) {
		t.Fatalf("re-parse: %+v %v", again, e)
	}
	// A model name that needs escaping comes out escaped.
	if got := string(r.WithModel(`a"b`)); !strings.Contains(got, `"model":"a\"b"`) {
		t.Errorf("escaping: %s", got)
	}
}

func TestErrorsHaveOpenAIsShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, Errorf(http.StatusNotFound, TypeInvalidRequest, "model", "model_not_found", "no model %q", "x"))
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("status %d content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	want := `{"error":{"message":"no model \"x\"","type":"invalid_request_error","param":"model","code":"model_not_found"}}`
	if rec.Body.String() != want {
		t.Errorf("got  %s\nwant %s", rec.Body.String(), want)
	}
	// Empty param and code are null, not "".
	b, _ := json.Marshal(Errorf(500, TypeServer, "", "", "boom"))
	if string(b) != `{"error":{"message":"boom","type":"server_error","param":null,"code":null}}` {
		t.Errorf("got %s", b)
	}
	if got := Errorf(502, TypeServer, "", "upstream_error", "x").Error(); got != "502 server_error: x" {
		t.Errorf("Error() = %q", got)
	}
}
