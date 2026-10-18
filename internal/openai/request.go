package openai

import (
	"bytes"
	"encoding/json"
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

// actedOn are the top-level keys the gateway reads. A key that matches one of them only when case is ignored is
// refused: encoding/json fills a struct field from a key in any case, so a provider written in Go would read
// {"model": "a", "MODEL": "b"} as model b while the gateway routed model a.
var actedOn = []string{"model", "messages", "stream"}

// ParseChatRequest reads a chat completion request body. Every error is a 400 in OpenAI's shape.
func ParseChatRequest(body []byte) (*ChatRequest, *Error) {
	members, err := jsonstrict.ParseObject(body)
	if err != nil {
		return nil, InvalidRequest("", "invalid_json", "%s", err)
	}
	r := &ChatRequest{Members: members}
	var model, messages, stream json.RawMessage
	for _, m := range members {
		for _, name := range actedOn {
			if m.Key != name && strings.EqualFold(m.Key, name) {
				return nil, InvalidRequest(m.Key, "invalid_parameter",
					"the key %q differs from %q only in case; send %q", m.Key, name, name)
			}
		}
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
	return r, nil
}

func parseMessage(i int, raw json.RawMessage) (Message, *Error) {
	var fields map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &fields) != nil {
		return Message{}, InvalidRequest(param(i, ""), "invalid_type", "messages[%d] must be an object", i)
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
		for _, part := range parts {
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

// WithModel is the request body to send upstream: the client's members in their order and byte for byte, with only
// model's value replaced.
func (r *ChatRequest) WithModel(model string) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range r.Members {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(m.Key) // a string always encodes
		b.Write(key)
		b.WriteByte(':')
		if m.Key == "model" {
			v, _ := json.Marshal(model)
			b.Write(v)
		} else {
			b.Write(m.Value)
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}
