package config

// Bug (minor; the day-4 review's fix 5 compares port strings): only the spelling "0" is exempt as "a free port each",
// but net.Listen reads "" and "00" as port 0 too, so a configuration listening on two free ports spelled that way
// (say "127.0.0.1:" for both) is refused as "the API's port" although the two can never clash.

import "testing"

func TestReview2D4TwoFreePortsAreNeverTheSamePort(t *testing.T) {
	const rest = `"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]`
	for _, port := range []string{"", "00"} {
		top := `"listen":"127.0.0.1:` + port + `","metrics":{"listen":"127.0.0.1:` + port + `"}`
		if _, err := Parse([]byte(`{`+top+`,`+rest+`}`), env(nil)); err != nil {
			t.Errorf("port %q for both: %v", port, err)
		}
	}
}
