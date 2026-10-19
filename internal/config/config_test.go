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
	if !ok || len(fast.Targets) != 1 || fast.MaxAttempts != 1 {
		t.Fatalf("fast: %+v", fast)
	}
	if ft := fast.Targets[0]; ft.Provider.Name != "mock-a" || ft.Provider.BaseURL != "http://127.0.0.1:8118/v1" ||
		ft.UpstreamModel != "mock-small" || ft.Provider.Timeout != 10*time.Second || ft.Weight != 1 {
		t.Errorf("fast: %+v %+v", ft, ft.Provider)
	}
	if alias, ok := c.Route("gpt-4o-mini"); !ok || alias.Name != "fast" {
		t.Errorf("an alias routes to its model: %+v", alias)
	}
	if m, ok := c.Route("mock-small"); !ok || m.Targets[0].UpstreamModel != "mock-small" {
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
	if p := c.Providers[0]; p.APIKey != "sk-test-123" || p.Timeout != 2*time.Minute {
		t.Errorf("%+v", p)
	}
}

func TestTargetsAliasesAndTheirDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"providers":[{"name":"a","base_url":"http://h/v1"},{"name":"b","base_url":"http://h/v2"}],`+
		`"models":[`+
		`{"name":"smart","aliases":["gpt-4o","o/1"],"targets":[{"provider":"b","upstream_model":"large","weight":3},`+
		`{"provider":"a","weight":0},{"provider":"a","upstream_model":"small"}],"max_attempts":2},`+
		`{"name":"plain","provider":"a"}]}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	smart, ok := c.Route("o/1")
	if !ok || smart.Name != "smart" || smart.MaxAttempts != 2 || len(smart.Targets) != 3 {
		t.Fatalf("%+v", smart)
	}
	for i, want := range []struct {
		provider, upstream string
		weight             int
	}{{"b", "large", 3}, {"a", "smart", 0}, {"a", "small", 1}} {
		got := smart.Targets[i]
		if got.Provider.Name != want.provider || got.UpstreamModel != want.upstream || got.Weight != want.weight {
			t.Errorf("targets[%d] %s as %s weight %d, want %+v", i, got.Provider.Name, got.UpstreamModel, got.Weight,
				want)
		}
	}
	if plain, _ := c.Route("plain"); plain.MaxAttempts != 1 || len(plain.Aliases) != 0 {
		t.Errorf("plain: %+v", plain)
	}
	if c.RetryBudget != DefaultRetryBudget || c.Breaker != DefaultBreaker {
		t.Errorf("defaults: %+v %+v", c.RetryBudget, c.Breaker)
	}
	// Every target is tried by default; each of the two settings defaults field by field.
	c, err = Parse([]byte(`{"retry_budget":{"ratio":0.5},"circuit_breaker":{"cooldown":"1.5s"},`+
		`"providers":[{"name":"a","base_url":"http://h"},{"name":"b","base_url":"http://h"}],`+
		`"models":[{"name":"m","targets":[{"provider":"a"},{"provider":"b"}]}]}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Route("m"); m.MaxAttempts != 2 {
		t.Errorf("max_attempts defaults to the number of targets: %d", m.MaxAttempts)
	}
	if c.RetryBudget != (RetryBudget{Ratio: 0.5, MinPerSecond: 1}) ||
		c.Breaker != (Breaker{Failures: 5, Cooldown: 1500 * time.Millisecond}) {
		t.Errorf("%+v %+v", c.RetryBudget, c.Breaker)
	}
	c, err = Parse([]byte(`{"retry_budget":{"ratio":0,"min_per_second":0},"circuit_breaker":{"failures":0},`+
		minimal[1:]), env(nil))
	if err != nil || c.RetryBudget != (RetryBudget{}) || c.Breaker.Failures != 0 {
		t.Errorf("both can be turned off: %+v %+v %v", c.RetryBudget, c.Breaker, err)
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
	c, err = Parse([]byte(`{"allowed_hosts":["Gateway.Internal","*","my_gateway","0:0:0:0:0:0:0:1","::FFFF:127.0.0.1",`+
		`"a","x-1.example","10.0.0.5"],`+minimal[1:]), env(nil))
	if err != nil || strings.Join(c.AllowedHosts, ",") !=
		"gateway.internal,*,my_gateway,::1,127.0.0.1,a,x-1.example,10.0.0.5" {
		t.Errorf("allowed_hosts: %q %v", c.AllowedHosts, err)
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
		{"a host starting with '-'", file(`"allowed_hosts":["-gateway"]`, p, m), "allowed_hosts[0]"},
		{"a host ending with '-'", file(`"allowed_hosts":["gateway-"]`, p, m), "allowed_hosts[0]"},
		{"a host ending with '.'", file(`"allowed_hosts":["gateway."]`, p, m), "allowed_hosts[0]"},
		{"a host with a slash", file(`"allowed_hosts":["gateway/v1"]`, p, m), "allowed_hosts[0]"},
		{"a host with an @", file(`"allowed_hosts":["user@gateway"]`, p, m), "allowed_hosts[0]"},
		{"a host in Unicode", file(`"allowed_hosts":["gätewäy"]`, p, m), "allowed_hosts[0]"},
		{"a second bad host", file(`"allowed_hosts":["localhost","a b"]`, p, m), "allowed_hosts[1]"},
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
		{"a bad env name", file("", `{"name":"p","base_url":"http://h","api_key_env":"MY-KEY"}`, m),
			"api_key_env isn't an environment variable's name"},
		{"a lower-case env name, unset", file("", `{"name":"p","base_url":"http://h","api_key_env":"my_key"}`, m),
			"named in api_key_env (not shown"},
		{"an unset key", file("", `{"name":"p","base_url":"http://h","api_key_env":"NOT_SET"}`, m),
			"NOT_SET (api_key_env) is unset or empty"},
		{"no models", `{"providers":[` + p + `],"models":[]}`, "models: at least one is required"},
		{"a model without a name", file("", p, `{"provider":"p"}`), "models[0]: name is required"},
		{"a model used twice", file("", p, m+`,`+m), `models[1]: the name "m" is used twice`},
		{"an unknown provider", file("", p, `{"name":"m","provider":"q"}`), `provider "q" is not one`},
		{"a model not an object", file("", p, `[]`), "models[0]: must be an object"},
		{"a model without a provider", file("", p, `{"name":"m"}`), "a provider or a list of targets"},
		{"a provider and targets", file("", p, `{"name":"m","provider":"p","targets":[{"provider":"p"}]}`),
			"not both"},
		{"an upstream model and targets", file("", p, `{"name":"m","upstream_model":"x","targets":[{"provider":"p"}]}`),
			"not both"},
		{"no targets", file("", p, `{"name":"m","targets":[]}`), "targets is empty"},
		{"a target not an object", file("", p, `{"name":"m","targets":["p"]}`), "targets[0]: must be an object"},
		{"a target's unknown provider", file("", p, `{"name":"m","targets":[{"provider":"q"}]}`),
			`targets[0]: provider "q" is not one`},
		{"a target without a provider", file("", p, `{"name":"m","targets":[{"weight":1}]}`),
			`targets[0]: provider "" is not one`},
		{"a target key in another case", file("", p, `{"name":"m","targets":[{"Provider":"p"}]}`),
			`unknown key "Provider"`},
		{"a target twice", file("", p, `{"name":"m","targets":[{"provider":"p"},{"provider":"p","upstream_model":"m"}]}`),
			"targets[1]: p as \"m\" is a target twice"},
		{"a negative weight", file("", p, `{"name":"m","targets":[{"provider":"p","weight":-1}]}`), "weight must be"},
		{"a fractional weight", file("", p, `{"name":"m","targets":[{"provider":"p","weight":0.5}]}`), "weight"},
		{"a weight too large", file("", p, `{"name":"m","targets":[{"provider":"p","weight":1001}]}`), "weight must be"},
		{"only standbys", file("", p, `{"name":"m","targets":[{"provider":"p","weight":0}]}`), "every target has weight 0"},
		{"zero attempts", file("", p, `{"name":"m","provider":"p","max_attempts":0}`), "max_attempts must be from 1 to"},
		{"more attempts than targets", file("", p, `{"name":"m","provider":"p","max_attempts":2}`),
			"max_attempts must be from 1 to the number of targets (1)"},
		{"an empty alias", file("", p, `{"name":"m","provider":"p","aliases":[""]}`), "aliases[0] must be"},
		{"a null alias", file("", p, `{"name":"m","provider":"p","aliases":[null]}`), "aliases[0] must be"},
		{"aliases not a list", file("", p, `{"name":"m","provider":"p","aliases":"x"}`), "aliases"},
		{"an alias of its own name", file("", p, `{"name":"m","provider":"p","aliases":["m"]}`),
			`models[0]: the name "m" is used twice`},
		{"an alias that is another model", file("", p, `{"name":"m","provider":"p","aliases":["n"]},`+
			`{"name":"n","provider":"p"}`), `models[1]: the name "n" is used twice`},
		{"an alias twice", file("", p, `{"name":"m","provider":"p","aliases":["x"]},{"name":"n","provider":"p",`+
			`"aliases":["x"]}`), `models[1]: the name "x" is used twice`},
		{"a budget ratio over 1", file(`"retry_budget":{"ratio":1.5}`, p, m), "retry_budget: ratio must be"},
		{"a negative budget ratio", file(`"retry_budget":{"ratio":-0.1}`, p, m), "retry_budget: ratio must be"},
		{"a negative floor", file(`"retry_budget":{"min_per_second":-1}`, p, m), "min_per_second must be"},
		{"a budget key in another case", file(`"retry_budget":{"Ratio":0.1}`, p, m), `unknown key "Ratio"`},
		{"a budget not an object", file(`"retry_budget":0.2`, p, m), "retry_budget: must be an object"},
		{"negative breaker failures", file(`"circuit_breaker":{"failures":-1}`, p, m), "failures must be from 0"},
		{"a bad cooldown", file(`"circuit_breaker":{"cooldown":"10"}`, p, m), "cooldown"},
		{"a zero cooldown", file(`"circuit_breaker":{"cooldown":"0s"}`, p, m), "cooldown"},
		{"a breaker key in another case", file(`"circuit_breaker":{"Failures":3}`, p, m), `unknown key "Failures"`},
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

func TestAKeyWithASpaceOrAControlCharacterIsRefused(t *testing.T) {
	for _, key := range []string{"sk-never-shown\r", "sk-never-shown\n", " sk-never-shown", "sk-never shown",
		"sk-never-shown\t", "sk-never\x00shown", "sk-never-shown\x7f"} {
		_, err := Parse([]byte(`{"providers":[{"name":"p","base_url":"http://h","api_key_env":"KEY"}],`+
			`"models":[{"name":"m","provider":"p"}]}`), env(map[string]string{"KEY": key}))
		if err == nil || !strings.Contains(err.Error(), "KEY (api_key_env) holds a space or a control character") ||
			strings.Contains(err.Error(), "never") {
			t.Errorf("%q: %v", key, err)
		}
	}
	if _, err := Parse([]byte(`{"providers":[{"name":"p","base_url":"http://h","api_key_env":"KEY"}],`+
		`"models":[{"name":"m","provider":"p"}]}`), env(map[string]string{"KEY": "sk-proj_AZaz09-+/=.~"})); err != nil {
		t.Errorf("an ordinary key: %v", err)
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
