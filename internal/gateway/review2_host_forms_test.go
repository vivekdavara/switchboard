package gateway

// Review 2 (at 92f8214): two kinds of request the Host check refuses although neither can come from a DNS-rebinding
// page (a browser always sends a Host, and sends the page's own domain in it), and neither can be allowed by any
// allowed_hosts entry short of ["*"], which turns the check off:
//
//  1. No Host at all: an HTTP/1.0 request (net/http requires Host only from HTTP/1.1 on) has r.Host == "", which
//     never equals an entry, and config refuses "" as an entry. HTTP/1.0 probes without Host are the default of
//     common health checkers (HAProxy's "option httpchk" sends "OPTIONS / HTTP/1.0" with no Host), so /readyz,
//     whose 503 drain_delay exists to show a load balancer, answers them 403 instead. Seen:
//     GET /readyz HTTP/1.0 (no Host) -> 403 {"error":{"message":"the Host \"\" is not one this gateway answers to ...
//  2. An IP address written another way: hostAllowed compares text, not addresses, and config keeps an IP entry as
//     written. allowed_hosts ["0:0:0:0:0:0:0:1"] loads without complaint, then refuses [::1]:8108, the same address
//     (and the default "::1" refuses [0:0:0:0:0:0:0:1]:8108 and [0::1]).
//
// Expected: 1 answered (an absent Host is not a foreign one); 2 answered (the entry and the Host name one address).
//
// Run: go test -race -count=1 -run TestReview2HostFormsNoPageCanSendAreAnswered -v ./internal/gateway

import "testing"

func TestReview2HostFormsNoPageCanSendAreAnswered(t *testing.T) {
	up := newProvider(t, mockProvider(t, "mock-a"))

	t.Run("an HTTP/1.0 probe without a Host", func(t *testing.T) {
		srv, g, _ := gatewayFor(t, "", []string{at(up, "")}, "fast=p0/mock-small")
		g.SetReady(true)
		resp, body := rawExchange(t, srv, "GET /readyz HTTP/1.0\r\n\r\n", false)
		if resp == nil || resp.StatusCode != 200 {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Errorf("GET /readyz HTTP/1.0 without a Host: %d %s, want 200", status, body)
		}
	})

	t.Run("an allowed IP address written another way", func(t *testing.T) {
		srv, _, _ := gatewayFor(t, `"allowed_hosts":["0:0:0:0:0:0:0:1"]`, []string{at(up, "")}, "fast=p0/mock-small")
		if resp, b := sendAs(t, srv, "[::1]:8108", "/v1/chat/completions"); resp.StatusCode != 200 {
			t.Errorf(`allowed_hosts ["0:0:0:0:0:0:0:1"], Host [::1]:8108: %d %s, want 200`, resp.StatusCode, b)
		}
	})
}
