package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestTheMetricsAndLogSectionsAreRead(t *testing.T) {
	const rest = `"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]`
	c, err := Parse([]byte(`{`+rest+`}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics != nil || c.Log != (Log{Level: slog.LevelInfo}) {
		t.Errorf("defaults: %+v %+v", c.Metrics, c.Log)
	}
	c, err = Parse([]byte(`{"metrics":{"listen":"127.0.0.1:9108"},"log":{"level":"warn","format":"text"},`+rest+`}`),
		env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics == nil || c.Metrics.Listen != "127.0.0.1:9108" || c.Log != (Log{Level: slog.LevelWarn, Text: true}) {
		t.Errorf("%+v %+v", c.Metrics, c.Log)
	}
	if c, err := Parse([]byte(`{"listen":"127.0.0.1:0","metrics":{"listen":"127.0.0.1:0"},`+rest+`}`), env(nil)); err != nil ||
		c.Metrics.Listen != "127.0.0.1:0" {
		t.Errorf("two free ports: %v", err)
	}
	for in, want := range map[string]slog.Level{`"info"`: slog.LevelInfo, `"error"`: slog.LevelError} {
		if c, err := Parse([]byte(`{"log":{"level":`+in+`},`+rest+`}`), env(nil)); err != nil || c.Log.Level != want {
			t.Errorf("%s: %v %v", in, c, err)
		}
	}
}

func TestDay4MistakesAreRefusedAndNamed(t *testing.T) {
	const rest = `"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]`
	for _, tc := range []struct{ name, top, want string }{
		{"metrics not an object", `"metrics":"127.0.0.1:9108"`, "metrics: must be an object"},
		{"metrics with no address", `"metrics":{}`, "metrics: listen must be a host:port"},
		{"a metrics port past 65535", `"metrics":{"listen":"127.0.0.1:99999"}`, "a port from 0 to 65535"},
		{"a metrics port that is no service", `"metrics":{"listen":"127.0.0.1:nosuchservice"}`, "a port from 0 to 65535"},
		{"an API port past 65535, the metrics' too", `"listen":":99999","metrics":{"listen":":99999"}`,
			`listen: ":99999" is not a host:port address with a port from 0 to 65535`},
		{"a metrics address with no port", `"metrics":{"listen":"localhost"}`, "listen must be a host:port"},
		{"an unknown metrics key", `"metrics":{"listen":"h:1","path":"/m"}`, `unknown key "path"`},
		{"metrics on the API's address", `"listen":"127.0.0.1:8108","metrics":{"listen":"127.0.0.1:8108"}`,
			"has the API's port"},
		{"metrics on the API's default address", `"metrics":{"listen":"127.0.0.1:8108"}`, "has the API's port"},
		{"metrics on the API's port, another host", `"listen":":8108","metrics":{"listen":"127.0.0.1:8108"}`,
			"has the API's port"},
		{"metrics on the API's port, by address", `"listen":"localhost:8108","metrics":{"listen":"[::1]:8108"}`,
			"has the API's port"},
		{"log not an object", `"log":"warn"`, "log: must be an object"},
		{"another level", `"log":{"level":"debug"}`, `level must be "info", "warn" or "error", not "debug"`},
		{"a level in capitals", `"log":{"level":"WARN"}`, `level must be`},
		{"another format", `"log":{"format":"logfmt"}`, `format must be "json" or "text"`},
		{"an unknown log key", `"log":{"file":"/tmp/x"}`, `unknown key "file"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(`{`+tc.top+`,`+rest+`}`), env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
