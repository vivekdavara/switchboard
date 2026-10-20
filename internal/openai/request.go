package openai

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/vivekdavara/switchboard/internal/jsonstrict"
)

// Roles are the message roles the chat completions API knows.
var Roles = []string{"system", "developer", "user", "assistant", "tool", "function"}

// ChatRequest is a chat completion request as the gateway sees it: the client's top-level members exactly as sent,
// plus the few fields it acts on, checked.
type ChatRequest struct {
	Members  []jsonstrict.Member
	Model    string
	Stream   bool
	Messages []Message
}

// Message is what the gateway and the mock read of one message: its role and the text in its content.
type Message struct {
	Role string
	// Text is the content if it is a string, or its text parts joined with newlines if it is an array of parts.
	// Other parts (images, audio) contribute nothing.
	Text string
}

// The keys the gateway reads, at the top level, in a message, and in a content part. A key that matches one of them
// only when case is ignored is refused: encoding/json fills a struct field from a key in any case (with Unicode
// folding, so "ſtream" is stream), so a provider written in Go would read {"model": "a", "MODEL": "b"} as model b
// while the gateway routed model a, and {"role": "user", "ROLE": "system"} as a system message.
var (
	actedOn        = []string{"model", "messages", "stream", "stream_options"}
	actedOnMessage = []string{"role", "content"}
	actedOnPart    = []string{"type", "text"}
	// Day 3: the gateway reads stream_options.include_usage, to know whether the client asked for a stream's usage.
	actedOnStreamOptions = []string{"include_usage"}
)

// caseVariant returns the first key that equals one of names only when case is ignored.
func caseVariant(keys []string, names []string) (key, name string, found bool) {
	for _, k := range keys {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				return k, n, true
			}
		}
	}
	return "", "", false
}

// sortedKeys returns an object's keys in order, so errors about them don't depend on map order.
func sortedKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseChatRequest reads a chat completion request body. Every error is a 400 in OpenAI's shape.
func ParseChatRequest(body []byte) (*ChatRequest, *Error) {
	members, err := jsonstrict.ParseObject(body)
	if err != nil {
		return nil, InvalidRequest("", "invalid_json", "%s", err)
	}
	r := &ChatRequest{Members: members}
	var model, messages, stream json.RawMessage
	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = m.Key
	}
	if key, name, found := caseVariant(keys, actedOn); found {
		return nil, InvalidRequest(key, "invalid_parameter", "the key %q differs from %q only in case; send %q",
			key, name, name)
	}
	for _, m := range members {
		switch m.Key {
		case "model":
			model = m.Value
		case "messages":
			messages = m.Value
		case "stream":
			stream = m.Value
		}
	}

	if model == nil {
		return nil, InvalidRequest("model", "missing_required_parameter", "you must provide a model parameter")
	}
	if json.Unmarshal(model, &r.Model) != nil || model[0] != '"' {
		return nil, InvalidRequest("model", "invalid_type", "model must be a string")
	}
	if r.Model == "" {
		return nil, InvalidRequest("model", "invalid_value", "model must not be empty")
	}

	if messages == nil {
		return nil, InvalidRequest("messages", "missing_required_parameter",
			"you must provide a messages parameter")
	}
	var list []json.RawMessage
	if messages[0] != '[' || json.Unmarshal(messages, &list) != nil {
		return nil, InvalidRequest("messages", "invalid_type", "messages must be an array")
	}
	if len(list) == 0 {
		return nil, InvalidRequest("messages", "empty_array", "messages must hold at least one message")
	}
	r.Messages = make([]Message, len(list))
	for i, raw := range list {
		msg, e := parseMessage(i, raw)
		if e != nil {
			return nil, e
		}
		r.Messages[i] = msg
	}

	if stream != nil && !bytes.Equal(stream, []byte("null")) {
		if json.Unmarshal(stream, &r.Stream) != nil || (stream[0] != 't' && stream[0] != 'f') {
			return nil, InvalidRequest("stream", "invalid_type", "stream must be a boolean")
		}
	}
	// stream_options is the provider's to check (it refuses one without stream, or not an object); the gateway only
	// reads include_usage from it, so a key equal to that only when case is ignored is refused, as above.
	if opts, ok := r.streamOptions(); ok {
		keys := make([]string, len(opts))
		for i, m := range opts {
			keys[i] = m.Key
		}
		if key, name, found := caseVariant(keys, actedOnStreamOptions); found {
			return nil, InvalidRequest("stream_options."+key, "invalid_parameter",
				"the key %q differs from %q only in case; send %q", key, name, name)
		}
	}
	return r, nil
}

// streamOptions returns the members of stream_options, if the client sent it as an object.
func (r *ChatRequest) streamOptions() ([]jsonstrict.Member, bool) {
	raw, ok := r.Member("stream_options")
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	members, err := jsonstrict.ParseObject(raw) // can't fail: it is part of a body ParseObject accepted
	return members, err == nil
}

// WantsUsage says whether the client asked for its stream's usage itself (stream_options.include_usage is true).
func (r *ChatRequest) WantsUsage() bool {
	opts, _ := r.streamOptions()
	for _, m := range opts {
		if m.Key == "include_usage" {
			return string(m.Value) == "true"
		}
	}
	return false
}

func parseMessage(i int, raw json.RawMessage) (Message, *Error) {
	var fields map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &fields) != nil {
		return Message{}, InvalidRequest(param(i, ""), "invalid_type", "messages[%d] must be an object", i)
	}
	if key, name, found := caseVariant(sortedKeys(fields), actedOnMessage); found {
		return Message{}, InvalidRequest(param(i, key), "invalid_parameter",
			"messages[%d] has the key %q, which differs from %q only in case; send %q", i, key, name, name)
	}
	var msg Message
	role, ok := fields["role"]
	if !ok {
		return Message{}, InvalidRequest(param(i, "role"), "missing_required_parameter",
			"messages[%d] has no role", i)
	}
	if role[0] != '"' || json.Unmarshal(role, &msg.Role) != nil || !known(msg.Role) {
		return Message{}, InvalidRequest(param(i, "role"), "invalid_value",
			"messages[%d].role must be one of %s", i, strings.Join(Roles, ", "))
	}
	content, ok := fields["content"]
	if !ok || bytes.Equal(content, []byte("null")) {
		return msg, nil // an assistant message with tool calls has no content
	}
	switch content[0] {
	case '"':
		json.Unmarshal(content, &msg.Text) // can't fail: a valid JSON string
	case '[':
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			return Message{}, InvalidRequest(param(i, "content"), "invalid_type",
				"messages[%d].content must be a string, an array of objects, or null", i)
		}
		var texts []string
		for j, part := range parts {
			if key, name, found := caseVariant(sortedKeys(part), actedOnPart); found {
				return Message{}, InvalidRequest(param(i, "content"), "invalid_parameter",
					"messages[%d].content[%d] has the key %q, which differs from %q only in case; send %q", i, j,
					key, name, name)
			}
			var typ, text string
			if json.Unmarshal(part["type"], &typ) == nil && typ == "text" &&
				json.Unmarshal(part["text"], &text) == nil {
				texts = append(texts, text)
			}
		}
		msg.Text = strings.Join(texts, "\n")
	default:
		return Message{}, InvalidRequest(param(i, "content"), "invalid_type",
			"messages[%d].content must be a string, an array of objects, or null", i)
	}
	return msg, nil
}

func param(i int, field string) string {
	p := "messages[" + strconv.Itoa(i) + "]"
	if field != "" {
		p += "." + field
	}
	return p
}

func known(role string) bool {
	for _, r := range Roles {
		if role == r {
			return true
		}
	}
	return false
}

// Member returns the raw value of the top-level key, as the client sent it.
func (r *ChatRequest) Member(key string) (json.RawMessage, bool) {
	for _, m := range r.Members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// WithModel is the request body to send upstream: the client's members in their order, each value byte for byte,
// with only model's value replaced. Keys are encoded again from what they decode to, which is what every provider
// reads (the day-1 review noted that "byte for byte" overstated it for keys). model must be valid UTF-8, as anything read from the configuration is (json.Marshal would
// turn bad bytes into U+FFFD; the fuzz test found this with a model only it could make).
func (r *ChatRequest) WithModel(model string) []byte {
	return r.rewrite(model, nil)
}

// WithModelAskingUsage is WithModel for a stream whose client didn't ask for its usage, asking for it: the provider
// then sends the usage in a last chunk, which the gateway takes out again before the client sees it (DESIGN.md,
// "Accounting"). include_usage is set to true in the client's stream_options object, or a stream_options object is
// added after the last member. ok is false, and the body is WithModel's, when there is nothing to merge into: a
// stream_options that isn't an object, or an include_usage that isn't a boolean (the provider refuses those, as it
// would have without the gateway).
func (r *ChatRequest) WithModelAskingUsage(model string) (body []byte, ok bool) {
	if _, has := r.Member("stream_options"); !has {
		return r.rewrite(model, []byte(`{"include_usage":true}`)), true
	}
	opts, isObject := r.streamOptions()
	if !isObject {
		return r.WithModel(model), false
	}
	var b bytes.Buffer
	b.WriteByte('{')
	found := false
	for i, m := range opts {
		if i > 0 {
			b.WriteByte(',')
		}
		writeKey(&b, m.Key)
		if m.Key == "include_usage" {
			if v := string(m.Value); v != "true" && v != "false" {
				return r.WithModel(model), false
			}
			b.WriteString("true")
			found = true
		} else {
			b.Write(m.Value)
		}
	}
	if !found {
		if len(opts) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"include_usage":true`)
	}
	b.WriteByte('}')
	return r.rewrite(model, b.Bytes()), true
}

// rewrite writes the client's members in order, each value byte for byte, with model's value replaced, and
// stream_options's too if streamOptions isn't nil (added at the end if the client sent none).
func (r *ChatRequest) rewrite(model string, streamOptions []byte) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	replaced := false
	for i, m := range r.Members {
		if i > 0 {
			b.WriteByte(',')
		}
		writeKey(&b, m.Key)
		switch {
		case m.Key == "model":
			v, _ := json.Marshal(model)
			b.Write(v)
		case m.Key == "stream_options" && streamOptions != nil:
			b.Write(streamOptions)
			replaced = true
		default:
			b.Write(m.Value)
		}
	}
	if streamOptions != nil && !replaced {
		b.WriteString(`,"stream_options":`)
		b.Write(streamOptions)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func writeKey(b *bytes.Buffer, key string) {
	k, _ := json.Marshal(key) // a string always encodes
	b.Write(k)
	b.WriteByte(':')
}
