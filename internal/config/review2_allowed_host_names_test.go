package config

// Review 2 (at 92f8214): allowed_hosts refuses host names that a Host header can hold and that real deployments use.
// The entry pattern ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ has no '_', but net/http accepts '_' in Host (validHostByte),
// and Docker's embedded DNS resolves Compose service and container names with underscores (my_gateway,
// project_gateway_1), so a gateway reached as http://llm_gateway:8108 cannot be allowed by name. The only way out is
// ["*"], which switches the DNS-rebinding check off. DESIGN.md: "allowed_hosts lists the names the Host header may
// hold ... An empty list, a port or brackets in an entry are errors" (nothing about '_').
//
// Expected: an entry with '_' loads (the gateway side already compares it as a plain lower-case string).
//
// Run: go test -race -count=1 -run TestReview2AnAllowedHostMayHoldAnUnderscore -v ./internal/config

import "testing"

func TestReview2AnAllowedHostMayHoldAnUnderscore(t *testing.T) {
	for _, host := range []string{"llm_gateway", "switchboard_gateway_1"} {
		c, err := Parse([]byte(`{"allowed_hosts":["`+host+`"],"providers":[{"name":"p","base_url":"http://h/v1"}],`+
			`"models":[{"name":"m","provider":"p"}]}`), env(nil))
		if err != nil {
			t.Errorf("allowed_hosts [%q]: %v", host, err)
			continue
		}
		if len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != host {
			t.Errorf("allowed_hosts [%q] loaded as %q", host, c.AllowedHosts)
		}
	}
}
