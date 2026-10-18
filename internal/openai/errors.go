// Package openai holds the parts of the OpenAI chat completions API that Switchboard speaks: the error shape, the
// request fields the gateway acts on, and the response shapes the mock provider writes.
package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Error types, as OpenAI uses them.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeServer         = "server_error"
	TypeRateLimit      = "rate_limit_error"
	TypeAuthentication = "authentication_error"
)

// Error is an API error: an HTTP status and OpenAI's error object. Param and Code are null in JSON when empty, as
// OpenAI writes them.
type Error struct {
	Status  int
	Message string
	Type    string
	Param   string
	Code    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Type, e.Message)
}

// MarshalJSON writes the error inside its envelope: {"error": {"message", "type", "param", "code"}}.
func (e *Error) MarshalJSON() ([]byte, error) {
	type body struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	}
	b := body{Message: e.Message, Type: e.Type}
	if e.Param != "" {
		b.Param = &e.Param
	}
	if e.Code != "" {
		b.Code = &e.Code
	}
	return json.Marshal(struct {
		Error body `json:"error"`
	}{b})
}

// Errorf makes an Error with a formatted message.
func Errorf(status int, typ, param, code, format string, args ...any) *Error {
	return &Error{Status: status, Type: typ, Param: param, Code: code, Message: fmt.Sprintf(format, args...)}
}

// InvalidRequest is a 400 about the request's parameter param.
func InvalidRequest(param, code, format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, TypeInvalidRequest, param, code, format, args...)
}

// WriteError writes e as the whole response.
func WriteError(w http.ResponseWriter, e *Error) {
	b, err := json.Marshal(e)
	if err != nil { // can't happen: strings only
		b = []byte(`{"error":{"message":"internal error","type":"server_error","param":null,"code":null}}`)
	}
	WriteJSON(w, e.Status, b)
}

// WriteJSON writes status and body as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(body)
}
