package gateway

// Bug (docs against code): README "The API" says "Each chat completion's response carries X-Request-Id (also on the
// request to the provider and in the log line)", and DESIGN "A chat completion, step by step" says the id is "also on
// every log line about the request". But a chat completion refused for its client key (401) or its Host (403) is
// answered in ServeHTTP, before chat makes the id: the response has no X-Request-Id, and the "client key refused" /
// "host not allowed" lines have no id either, so a client reporting a 401 has nothing to quote and the operator
// nothing to grep for. Either the id is made in ServeHTTP for /v1/chat/completions, or the docs say "once the key and
// Host are accepted".

import (
	"net/http"
	"testing"
)

func TestReviewD5AChatCompletionRefusedForItsKeyOrHostCarriesARequestID(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))
	srv, _, logs := gatewayFor(t, clients("alice"), []string{at(up, "")}, "fast=p0/mock-small")

	resp, _ := send(t, srv, ask)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if id := resp.Header.Get("X-Request-Id"); !requestID.MatchString(id) {
		t.Errorf("a chat completion's 401 has no X-Request-Id (%q)", id)
	}
	resp, _ = sendAs(t, srv, "attacker.example", "/v1/chat/completions")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign Host: %d", resp.StatusCode)
	}
	if id := resp.Header.Get("X-Request-Id"); !requestID.MatchString(id) {
		t.Errorf("a chat completion's 403 has no X-Request-Id (%q)", id)
	}
	for _, msg := range []string{"client key refused", "host not allowed"} {
		for _, l := range logLines(t, logs, msg) {
			if _, ok := l["id"]; !ok {
				t.Errorf("the %q line about a chat completion has no id: %v", msg, l)
			}
		}
	}
}
