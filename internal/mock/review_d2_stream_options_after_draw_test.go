// Found by the day-2 review (at b3b5e1f): chatCompletions checks stream_options only after it has drawn the request's
// fate, unlike every other refusal. So a request OpenAI refuses with a 400 (stream_options without stream) gets the
// injected failure, or hangs, when its fate says so; and such a refusal takes a draw, shifting the fate of every
// request after it, where a refusal for max_tokens takes none.
package mock

import (
	"strings"
	"testing"
	"time"
)

func TestReviewD2AnInvalidStreamOptionsIsRefusedBeforeTheDraw(t *testing.T) {
	cfg := fast()
	cfg.ErrorRate = 1
	_, srv := start(t, cfg)
	if status, b := post(t, srv, hello+`,"stream_options":{"include_usage":true}}`); status != 400 {
		t.Errorf("stream_options without stream, at error rate 1: %d %s; want OpenAI's 400", status, b)
	}

	// Two runs with the same seed, each after one refused request: the fates that follow must be the same.
	cfg = fast()
	cfg.ErrorRate, cfg.Seed = 0.5, 7
	_, a := start(t, cfg)
	_, b := start(t, cfg)
	post(t, a, hello+`,"max_tokens":0}`)
	post(t, b, hello+`,"stream_options":{"include_usage":true}}`)
	x, y := outcomes(t, a, 30, time.Second), outcomes(t, b, 30, time.Second)
	if strings.Join(x, ",") != strings.Join(y, ",") {
		t.Errorf("a refused stream_options moved the fates after it:\n%v\n%v", x, y)
	}
}
