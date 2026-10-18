package config

// Review 2 (at 92f8214): a provider key pasted into api_key_env, the field that is about the key and so the likeliest
// place to paste one, is printed whole in the configuration error that `switchboard serve` and `check` write to
// stderr. The day-1 round made sure no base_url error repeats a URL because "a key pasted into it" could sit there,
// and README's test table says "no key ... in any error", but api_key_env's two errors quote the value as given:
//
//   - a key with a '-' (sk-..., sk-proj-..., sk-ant-...) fails the name pattern:
//     `api_key_env "sk-proj-..." is not an environment variable's name`;
//   - a key made of letters, digits and '_' only (Groq's gsk_...) passes the pattern and is looked up:
//     `the environment variable gsk_... (api_key_env) is unset or empty`.
//
// Expected: neither error repeats the value (or at most a short prefix), as with base_url.
//
// Run: go test -race -count=1 -run TestReview2AKeyPastedIntoAPIKeyEnvIsNotPrinted -v ./internal/config

import (
	"strings"
	"testing"
)

func TestReview2AKeyPastedIntoAPIKeyEnvIsNotPrinted(t *testing.T) {
	for _, key := range []string{
		"sk-proj-Abc123Def456Ghi789",         // OpenAI style: fails envName, quoted in that error
		"gsk_Abc123Def456Ghi789Jkl012Mno345", // Groq style: passes envName, quoted as the variable's name
	} {
		_, err := Parse([]byte(`{"providers":[{"name":"p","base_url":"https://api.example.com/v1",`+
			`"api_key_env":"`+key+`"}],"models":[{"name":"m","provider":"p"}]}`), env(nil))
		if err == nil {
			t.Errorf("%s: accepted", key)
			continue
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("the error prints the key pasted into api_key_env: %v", err)
		}
	}
}
