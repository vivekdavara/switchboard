package config

import (
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestTheCheckedInConfigsLoad(t *testing.T) {
	c, err := Load("../../configs/local.json", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8108" || len(c.Providers) != 2 || len(c.Models) != 3 {
		t.Fatalf("%+v", c)
	}
	fast, ok := c.Route("fast")
	if !ok || fast.Provider.Name != "mock-a" || fast.Provider.BaseURL != "http://127.0.0.1:8118/v1" ||
		fast.UpstreamModel != "mock-small" || fast.Provider.Timeout != 10*time.Second {
		t.Errorf("fast: %+v %+v", fast, fast.Provider)
	}
	if m, ok := c.Route("mock-small"); !ok || m.UpstreamModel != "mock-small" {
		t.Errorf("an upstream model left out is the model's own name: %+v", m)
	}
	if _, ok := c.Route("Fast"); ok {
		t.Error("routes are case-sensitive")
	}

	// The live example needs its key, and only from the environment.
	if _, err := Load("../../configs/openai.example.json", env(nil)); err == nil ||
		!strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("without the key: %v", err)
	}
	c, err = Load("../../configs/openai.example.json", env(map[string]string{"OPENAI_API_KEY": "sk-test-123"}))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Route("gpt-4o-mini"); m.Provider.APIKey != "sk-test-123" || m.Provider.Timeout != 2*time.Minute {
		t.Errorf("%+v", m.Provider)
	}
}

const minimal = `{"providers":[{"name":"p","base_url":"http://127.0.0.1:1/v1/"}],"models":[{"name":"m","provider":"p"}]}`

func TestWhatTheFileLeavesOutTakesItsDefault(t *testing.T) {
	c, err := Parse([]byte(minimal), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers[0]
	if c.Listen != DefaultListen || c.MaxRequestBytes != DefaultMaxRequestBytes ||
		c.MaxResponseBytes != DefaultMaxResponseBytes || c.ShutdownGrace != DefaultShutdownGrace ||
		p.Timeout != DefaultTimeout || p.APIKey != "" || p.BaseURL != "http://127.0.0.1:1/v1" {
		t.Errorf("%+v %+v", c, p)
	}
	c, err = Parse([]byte(`{"shutdown_grace":"0s",`+minimal[1:]), env(nil))
	if err != nil || c.ShutdownGrace != 0 || c.DrainDelay != 0 {
		t.Errorf("a zero grace is allowed, and no drain is the default: %v %v", c, err)
	}
	if strings.Join(c.AllowedHosts, ",") != "localhost,127.0.0.1,::1" {
		t.Errorf("default allowed hosts %q", c.AllowedHosts)
	}
	c, err = Parse([]byte(`{"allowed_hosts":["Gateway.Internal","*"],`+minimal[1:]), env(nil))
	if err != nil || strings.Join(c.AllowedHosts, ",") != "gateway.internal,*" {
		t.Errorf("allowed_hosts: %v %v", c, err)
	}
	c, err = Parse([]byte(`{"drain_delay":"1.5s",`+minimal[1:]), env(nil))
	if err != nil || c.DrainDelay != 1500*time.Millisecond {
		t.Errorf("drain_delay: %v %v", c, err)
	}
}

func TestMistakesAreRefusedAndNamed(t *testing.T) {
	const p = `{"name":"p","base_url":"http://h/v1"}`
	const m = `{"name":"m","provider":"p"}`
	file := func(top, provider, model string) string {
		if top != "" {
			top += ","
		}
		return `{` + top + `"providers":[` + provider + `],"models":[` + model + `]}`
	}
	for _, tc := range []struct{ name, file, want string }{
		{"not JSON", `{"listen":`, "ends too soon"},
		{"a repeated key", `{"listen":"a:1","listen":"b:2"}`, `"listen" appears twice`},
		{"a repeated nested key", file("", `{"name":"p","name":"q","base_url":"http://h"}`, m), `"name" appears twice`},
		{"an unknown key", file(`"port":8108`, p, m), `unknown key "port"`},
		{"a key in another case", file(`"Listen":"x:1"`, p, m), `unknown key "Listen"`},
		{"a provider key in another case", file("", `{"name":"p","Base_URL":"http://h"}`, m), `unknown key "Base_URL"`},
		{"a model key in another case", file("", p, `{"Name":"m","provider":"p"}`), `unknown key "Name"`},
		{"a null", file(`"listen":null`, p, m), "listen is null"},
		{"a wrong type", file(`"listen":8108`, p, m), "listen"},
		{"a fractional size", file(`"max_request_bytes":1.5`, p, m), "max_request_bytes"},
		{"a zero size", file(`"max_response_bytes":0`, p, m), "max_response_bytes must be from 1"},
		{"a negative size", file(`"max_request_bytes":-1`, p, m), "max_request_bytes must be from 1"},
		{"a size past 1 GiB", file(`"max_request_bytes":1073741825`, p, m), "max_request_bytes must be from 1"},
		{"the largest int64", file(`"max_response_bytes":9223372036854775807`, p, m), "1 GiB"},
		{"a bad address", file(`"listen":"8108"`, p, m), "host:port"},
		{"a bad grace", file(`"shutdown_grace":"10"`, p, m), "shutdown_grace"},
		{"a negative grace", file(`"shutdown_grace":"-1s"`, p, m), "shutdown_grace"},
		{"a bad drain delay", file(`"drain_delay":"5"`, p, m), "drain_delay"},
		{"no allowed hosts", file(`"allowed_hosts":[]`, p, m), "allowed_hosts is empty"},
		{"a host with a port", file(`"allowed_hosts":["localhost:8108"]`, p, m), "without a port"},
		{"a bracketed host", file(`"allowed_hosts":["[::1]"]`, p, m), "without a port or brackets"},
		{"a host with a space", file(`"allowed_hosts":["my host"]`, p, m), "allowed_hosts[0]"},
		{"hosts not a list", file(`"allowed_hosts":"localhost"`, p, m), "allowed_hosts"},
		{"a negative drain delay", file(`"drain_delay":"-5s"`, p, m), "drain_delay"},
		{"no providers", `{"providers":[],"models":[` + m + `]}`, "at least one is required"},
		{"providers not a list", `{"providers":{},"models":[` + m + `]}`, "providers"},
		{"a provider not an object", file("", `"p"`, m), "providers[0]: must be an object"},
		{"no name", file("", `{"base_url":"http://h"}`, m), `name ""`},
		{"a name with a space", file("", `{"name":"my p","base_url":"http://h"}`, m), `name "my p"`},
		{"a name with a newline", file("", `{"name":"p\nX: y","base_url":"http://h"}`, m), "name"},
		{"a name used twice", file("", p+`,`+p, m), `"p" is used twice`},
		{"no base URL", file("", `{"name":"p"}`, m), "base_url is required"},
		{"a relative base URL", file("", `{"name":"p","base_url":"/v1"}`, m), "absolute http or https"},
		{"another scheme", file("", `{"name":"p","base_url":"ftp://h/v1"}`, m), "absolute http or https"},
		{"no host", file("", `{"name":"p","base_url":"http:///v1"}`, m), "no host"},
		{"credentials in the URL", file("", `{"name":"p","base_url":"https://u:sk-hidden-42@h/v1"}`, m),
			"credentials before an @"},
		{"a key as the user", file("", `{"name":"p","base_url":"https://sk-hidden-42@h/v1"}`, m),
			"credentials before an @"},
		{"an unparseable URL", file("", `{"name":"p","base_url":"https://u:sk-hidden-42@h:4 4/v1"}`, m),
			"can't be parsed"},
		{"a query", file("", `{"name":"p","base_url":"http://h/v1?key=sk-hidden-42"}`, m), "must not have a query"},
		{"an empty query", file("", `{"name":"p","base_url":"http://h/v1?"}`, m), "must not have a query"},
		{"a fragment", file("", `{"name":"p","base_url":"http://h/v1#x"}`, m), "or a fragment"},
		{"an empty fragment", file("", `{"name":"p","base_url":"http://h/v1#"}`, m), "or a fragment"},
		{"a bad timeout", file("", `{"name":"p","base_url":"http://h","timeout":"soon"}`, m), "timeout"},
		{"a zero timeout", file("", `{"name":"p","base_url":"http://h","timeout":"0s"}`, m), "timeout"},
		{"a bad env name", file("", `{"name":"p","base_url":"http://h","api_key_env":"MY-KEY"}`, m), "MY-KEY"},
		{"an unset key", file("", `{"name":"p","base_url":"http://h","api_key_env":"NOT_SET"}`, m),
			"NOT_SET (api_key_env) is unset or empty"},
		{"no models", `{"providers":[` + p + `],"models":[]}`, "models: at least one is required"},
		{"a model without a name", file("", p, `{"provider":"p"}`), "models[0]: name is required"},
		{"a model used twice", file("", p, m+`,`+m), `models[1]: the name "m" is used twice`},
		{"an unknown provider", file("", p, `{"name":"m","provider":"q"}`), `provider "q" is not one`},
		{"a model not an object", file("", p, `[]`), "models[0]: must be an object"},
	} {
		_, err := Parse([]byte(tc.file), env(map[string]string{"KEY": "sk-never-in-an-error"}))
		if err == nil {
			t.Errorf("%s: accepted %s", tc.name, tc.file)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %q, want it to contain %q", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "sk-hidden-42") {
			t.Errorf("%s: the error shows a secret from the URL: %q", tc.name, err)
		}
	}
}

func TestAKeyNeverAppearsInAnError(t *testing.T) {
	const key = "sk-never-in-an-error"
	for _, provider := range []string{
		`{"name":"p","base_url":"http://h","api_key_env":"KEY","timeout":"x"}`,
		`{"name":"p","base_url":"http://h","api_key_env":"KEY","bogus":1}`,
		`{"name":"p","base_url":"http://h","api_key_env":"KEY"},{"name":"p","base_url":"http://h","api_key_env":"KEY"}`,
	} {
		_, err := Parse([]byte(`{"providers":[`+provider+`],"models":[{"name":"m","provider":"p"}]}`),
			env(map[string]string{"KEY": key}))
		if err == nil || strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", provider, err)
		}
	}
}
