package openai

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkParseAndRewrite is the gateway's own work on a request body: the strict read, the checks, and the
// rewrite of model. README "Results" compares it with the whole overhead of a request through the gateway.
func BenchmarkParseAndRewrite(b *testing.B) {
	var msgs []string
	for i := range 20 {
		role := []string{"user", "assistant"}[i%2]
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":%q}`, role,
			strings.Repeat(fmt.Sprintf("turn %d says something about the weather. ", i), 4)))
	}
	for name, body := range map[string]string{
		"small": `{"model":"fast","messages":[{"role":"user","content":"What is two plus two?"}]}`,
		"chat":  `{"model":"fast","temperature":0.7,"messages":[` + strings.Join(msgs, ",") + `]}`,
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for range b.N {
				r, e := ParseChatRequest([]byte(body))
				if e != nil {
					b.Fatal(e)
				}
				if len(r.WithModel("mock-small")) == 0 {
					b.Fatal("empty")
				}
			}
		})
	}
}
