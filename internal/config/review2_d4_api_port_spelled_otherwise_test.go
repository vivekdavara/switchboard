package config

// Bug (the day-4 review's fix 5, incomplete; the platform-independent half of
// cmd/switchboard/review2_d4_metrics_port_spelling_test.go): the metrics port is compared with the API's as a string,
// but net.Listen reads a port as a number (leading zeros and a "+" allowed), so the API's port spelled another way is
// accepted, and on macOS the metrics listener then takes the API's local traffic.

import (
	"strings"
	"testing"
)

func TestReview2D4TheAPIsPortSpelledOtherwiseIsRefused(t *testing.T) {
	const rest = `"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]`
	for _, metrics := range []string{"127.0.0.1:08108", "127.0.0.1:+8108", "127.0.0.1:0008108"} {
		top := `"listen":":8108","metrics":{"listen":"` + metrics + `"}`
		if _, err := Parse([]byte(`{`+top+`,`+rest+`}`), env(nil)); err == nil || !strings.Contains(err.Error(), "has the API's port") {
			t.Errorf("metrics on %s with the API on :8108: %v", metrics, err)
		}
	}
}
