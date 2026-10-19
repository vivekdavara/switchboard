// Found by the day-2 review (at b3b5e1f): relay takes any first data event as the start of an answer, data: [DONE]
// included. A stream that is nothing but [DONE] commits a 200 with no chunk in it (an SDK returns an empty answer as
// a success), tells the breaker the provider succeeded, and logs one event; the same stream without its [DONE] falls
// back to the next target.
package gateway

import (
	"strings"
	"testing"
)

func TestReviewD2AStreamOfOnlyDoneIsNotAnAnswer(t *testing.T) {
	p0 := newProvider(t, streaming([]string{"data: [DONE]\n\n"}, 0, false))
	p1 := newProvider(t, pacedMock(t, "mock-b", 0, nil))
	srv, _, logs := gatewayFor(t, "", []string{at(p0, `"timeout":"300ms"`), at(p1, "")},
		inOrder("m", "p0/mock-small", "p1/mock-small"))
	resp, events, err := readStream(t, srv, askStream)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-Switchboard-Provider") != "p1" || len(events) != 35 {
		t.Errorf("%d from %s, %d events, %v; want p1's answer", resp.StatusCode,
			resp.Header.Get("X-Switchboard-Provider"), len(events), err)
	}
	if strings.Contains(logs.String(), `"streamed":true,"events":1}`) {
		t.Errorf("an empty stream was logged as one event:\n%s", logs)
	}
}
