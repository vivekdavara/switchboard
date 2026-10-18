package config

// Found by the day-1 review (at 07b8b14): parseProvider refused a base_url with credentials so that a secret would
// never sit in the file or a log line, but its error messages, which `switchboard serve` and `check` print to
// stderr, still carried the secret:
//
//   - every branch before the credentials check printed the URL verbatim: a typo in the scheme, a bad port, a
//     missing host, anything url.Parse refuses, showed "user:password@" in full;
//   - the credentials branch printed u.Redacted(), which hides only the password, so a key sent as the user name
//     (https://KEY@host, https://KEY:@host) was printed as is;
//   - the query branch printed the URL with its query, so "?key=..." was printed as is.
//
// Now no base_url error repeats the URL.
//
// Run: go test -race -count=1 -run TestReviewABaseURLsSecretNeverReachesTheError -v ./internal/config

import (
	"strings"
	"testing"
)

func TestReviewABaseURLsSecretNeverReachesTheError(t *testing.T) {
	const secret = "sk-live-0123456789"
	for _, base := range []string{
		"htps://user:" + secret + "@api.example.com/v1",          // a typo in the scheme
		"ftp://user:" + secret + "@api.example.com/v1",           // another scheme
		"https://user:" + secret + "@api.example.com:44 3/v1",    // url.Parse fails (a bad port)
		"https://user:" + secret + "@/v1",                        // no host
		"https://" + secret + "@api.example.com/v1",              // the key as the user name
		"https://" + secret + ":@api.example.com/v1",             // the key as the user name, empty password
		"https://api.example.com/v1?key=" + secret,               // the key in the query
		"https://user:" + secret + "@api.example.com/v1?x=1#top", // credentials and a query
		// Added after the second review: with the credentials check first, the URLs above never reach the scheme
		// and host checks with a secret in them, so a mutant printing the URL there went unnoticed.
		"htps://api.example.com/v1?key=" + secret, // a bad scheme, the key in the query
		"http:///v1?key=" + secret,                // no host, the key in the query
		"https://api.example.com/v1#" + secret,    // the key in a fragment
	} {
		_, err := Parse([]byte(`{"providers":[{"name":"p","base_url":"`+base+`"}],`+
			`"models":[{"name":"m","provider":"p"}]}`), env(nil))
		if err == nil {
			t.Errorf("%s: accepted", base)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error shows the secret: %v", err)
		}
	}
}
