package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func hashOf(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func TestClientsRedisCachesAndPrices(t *testing.T) {
	c, err := Parse([]byte(`{
		"redis": {"addr": "127.0.0.1:6382", "prefix": "sb:", "timeout": "20ms", "on_error": "refuse",
			"password_env": "REDIS_PASSWORD"},
		"clients": [
			{"name": "alice", "key_sha256": "`+hashOf("sb-alice")+`", "rate_limit": {"requests_per_minute": 90}},
			{"name": "bob", "key_sha256": "`+strings.ToUpper(hashOf("sb-bob"))+`",
				"rate_limit": {"requests_per_minute": 0.5, "burst": 3}},
			{"name": "carol", "key_sha256": "`+hashOf("sb-carol")+`"}
		],
		"providers": [{"name": "p", "base_url": "http://h/v1"}, {"name": "q", "base_url": "http://h/v2",
			"stream_usage": false}],
		"models": [{"name": "m", "provider": "p", "cache": {"ttl": "10m"}},
			{"name": "n", "provider": "q", "cache": {"ttl": "1.5s", "shared": true}}, {"name": "o", "provider": "p"}],
		"prices": [
			{"provider": "p", "upstream_model": "m", "input_per_million": 0.15, "output_per_million": 0.6},
			{"provider": "q", "upstream_model": "n", "input_per_million": 1000, "output_per_million": 0.000001},
			{"provider": "p", "upstream_model": "free", "input_per_million": 0, "output_per_million": 0}
		]}`), env(map[string]string{"REDIS_PASSWORD": "hunter2"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Redis; r == nil || r.Addr != "127.0.0.1:6382" || r.Prefix != "sb:" || r.Timeout != 20*time.Millisecond ||
		!r.RefuseWhenDown || r.Password != "hunter2" {
		t.Errorf("redis: %+v", c.Redis)
	}
	alice, ok := c.Client("sb-alice")
	if !ok || alice.Name != "alice" || *alice.RateLimit != (RateLimit{PerMinute: 90, Burst: 90}) {
		t.Errorf("alice, with burst defaulting to a minute's requests: %+v", alice)
	}
	bob, ok := c.Client("sb-bob")
	if !ok || bob.Name != "bob" || *bob.RateLimit != (RateLimit{PerMinute: 0.5, Burst: 3}) {
		t.Errorf("bob, whose hash is in capitals: %+v", bob)
	}
	if carol, ok := c.Client("sb-carol"); !ok || carol.RateLimit != nil {
		t.Errorf("carol, unlimited: %+v", carol)
	}
	for _, key := range []string{"", "sb-alice ", "SB-ALICE", hashOf("sb-alice")} {
		if cl, ok := c.Client(key); ok {
			t.Errorf("%q is %s's key", key, cl.Name)
		}
	}
	if len(c.Clients) != 3 || c.Clients[0] != alice || c.Clients[1] != bob {
		t.Errorf("clients in the file's order: %+v", c.Clients)
	}
	if !c.Providers[0].StreamUsage || c.Providers[1].StreamUsage {
		t.Errorf("stream_usage is on by default and can be turned off: %+v %+v", c.Providers[0], c.Providers[1])
	}
	m, _ := c.Route("m")
	n, _ := c.Route("n")
	o, _ := c.Route("o")
	if *m.Cache != (Cache{TTL: 10 * time.Minute}) || *n.Cache != (Cache{TTL: 1500 * time.Millisecond, Shared: true}) ||
		o.Cache != nil {
		t.Errorf("caches: %+v %+v %+v", m.Cache, n.Cache, o.Cache)
	}
	for _, tc := range []struct {
		provider, upstream string
		want               Price
	}{{"p", "m", Price{150_000, 600_000}}, {"q", "n", Price{1_000_000_000, 1}}, {"p", "free", Price{}}} {
		if got, ok := c.Price(tc.provider, tc.upstream); !ok || got != tc.want {
			t.Errorf("%s %s: %+v %v, want %+v", tc.provider, tc.upstream, got, ok, tc.want)
		}
	}
	if _, ok := c.Price("q", "m"); ok {
		t.Error("a price is a provider's, for one of its models")
	}
}

func TestDay3KeysLeftOutTakeTheirDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"redis":{"addr":"localhost:6379"},`+minimal[1:]), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Redis; r.Prefix != DefaultRedisPrefix || r.Timeout != DefaultRedisTimeout || r.RefuseWhenDown ||
		r.Password != "" {
		t.Errorf("redis defaults: %+v", r)
	}
	if c.Clients != nil || len(c.Prices()) != 0 {
		t.Errorf("no clients, no prices: %+v %+v", c.Clients, c.Prices())
	}
	if _, ok := c.Client(""); ok {
		t.Error("with no clients, no key is anyone's")
	}
	c, err = Parse([]byte(minimal), env(nil))
	if err != nil || c.Redis != nil {
		t.Errorf("no redis: %+v %v", c.Redis, err)
	}
}

func TestDay3MistakesAreRefusedAndNamed(t *testing.T) {
	const p = `{"name":"p","base_url":"http://h/v1"}`
	const m = `{"name":"m","provider":"p"}`
	const redis = `"redis":{"addr":"127.0.0.1:6382"}`
	file := func(top, model string) string {
		if top != "" {
			top += ","
		}
		return `{` + top + `"providers":[` + p + `],"models":[` + model + `]}`
	}
	client := func(fields string) string {
		return redis + `,"clients":[{"name":"c","key_sha256":"` + hashOf("k") + `"` + fields + `}]`
	}
	for _, tc := range []struct{ name, file, want string }{
		{"redis not an object", file(`"redis":"127.0.0.1:6382"`, m), "redis: must be an object"},
		{"redis with no address", file(`"redis":{}`, m), "addr must be a host:port"},
		{"a redis address with no port", file(`"redis":{"addr":"localhost"}`, m), "addr must be a host:port"},
		{"an unknown redis key", file(`"redis":{"addr":"h:1","db":2}`, m), `unknown key "db"`},
		{"an empty prefix", file(`"redis":{"addr":"h:1","prefix":""}`, m), "prefix must be"},
		{"a prefix with a space", file(`"redis":{"addr":"h:1","prefix":"a b"}`, m), "prefix must be"},
		{"a prefix past 64", file(`"redis":{"addr":"h:1","prefix":"`+strings.Repeat("a", 65)+`"}`, m),
			"prefix must be"},
		{"a bad redis timeout", file(`"redis":{"addr":"h:1","timeout":"100"}`, m), "timeout"},
		{"a zero redis timeout", file(`"redis":{"addr":"h:1","timeout":"0s"}`, m), "timeout"},
		{"a redis timeout past 10s", file(`"redis":{"addr":"h:1","timeout":"11s"}`, m), "at most 10s"},
		{"another on_error", file(`"redis":{"addr":"h:1","on_error":"fail"}`, m), `on_error must be "allow" or`},
		{"an unset password variable", file(`"redis":{"addr":"h:1","password_env":"RP"}`, m),
			"RP is unset or empty"},
		{"a password_env that isn't a name", file(`"redis":{"addr":"h:1","password_env":"a b"}`, m),
			"password_env isn't"},
		{"clients not a list", file(redis+`,"clients":{}`, m), "clients"},
		{"no clients", file(redis+`,"clients":[]`, m), "clients is empty"},
		{"a client not an object", file(redis+`,"clients":["c"]`, m), "clients[0]: must be an object"},
		{"a client with no name", file(redis+`,"clients":[{"key_sha256":"`+hashOf("k")+`"}]`, m), `name ""`},
		{"a client name with ':'", file(redis+`,"clients":[{"name":"a:b","key_sha256":"`+hashOf("k")+`"}]`, m),
			`name "a:b"`},
		{"the anonymous name", file(redis+`,"clients":[{"name":"anonymous","key_sha256":"`+hashOf("k")+`"}]`, m),
			"is the one requests without clients"},
		{"no hash", file(redis+`,"clients":[{"name":"c"}]`, m), "key_sha256 must be 64 hexadecimal"},
		{"a short hash", file(redis+`,"clients":[{"name":"c","key_sha256":"abcd"}]`, m), "key_sha256 must be 64"},
		{"a hash that isn't hex", file(redis+`,"clients":[{"name":"c","key_sha256":"`+strings.Repeat("g", 64)+`"}]`, m),
			"key_sha256 must be 64"},
		{"an unknown client key", file(client(`,"key":"k"`), m), `unknown key "key"`},
		{"a client name used twice", file(redis+`,"clients":[{"name":"c","key_sha256":"`+hashOf("k")+`"},`+
			`{"name":"c","key_sha256":"`+hashOf("j")+`"}]`, m), `"c" is used twice`},
		{"a key shared", file(redis+`,"clients":[{"name":"c","key_sha256":"`+hashOf("k")+`"},`+
			`{"name":"d","key_sha256":"`+strings.ToUpper(hashOf("k"))+`"}]`, m), "the same key_sha256 as c"},
		{"a rate limit with no redis", file(`"clients":[{"name":"c","key_sha256":"`+hashOf("k")+
			`","rate_limit":{"requests_per_minute":1}}]`, m), "a rate_limit needs redis"},
		{"a rate limit not an object", file(client(`,"rate_limit":60`), m), "rate_limit: must be an object"},
		{"no rate", file(client(`,"rate_limit":{"burst":5}`), m), "requests_per_minute must be above 0"},
		{"a zero rate", file(client(`,"rate_limit":{"requests_per_minute":0}`), m), "requests_per_minute must be"},
		{"a negative rate", file(client(`,"rate_limit":{"requests_per_minute":-1}`), m), "requests_per_minute"},
		{"a rate past a million", file(client(`,"rate_limit":{"requests_per_minute":1000001}`), m),
			"at most 1000000"},
		{"a zero burst", file(client(`,"rate_limit":{"requests_per_minute":1,"burst":0}`), m), "burst must be"},
		{"a fractional burst", file(client(`,"rate_limit":{"requests_per_minute":1,"burst":1.5}`), m), "burst"},
		{"a burst past a million", file(client(`,"rate_limit":{"requests_per_minute":1,"burst":1000001}`), m),
			"burst must be an integer from 1 to 1000000"},
		{"an unknown rate key", file(client(`,"rate_limit":{"requests_per_second":1}`), m),
			`unknown key "requests_per_second"`},
		{"a cache with no redis", file("", `{"name":"m","provider":"p","cache":{"ttl":"1m"}}`),
			"a cache needs redis"},
		{"a cache not an object", file(redis, `{"name":"m","provider":"p","cache":true}`), "cache: must be"},
		{"a cache with no ttl", file(redis, `{"name":"m","provider":"p","cache":{}}`), "ttl"},
		{"a cache ttl under 1ms", file(redis, `{"name":"m","provider":"p","cache":{"ttl":"999us"}}`), "ttl"},
		{"a cache ttl past 30 days", file(redis, `{"name":"m","provider":"p","cache":{"ttl":"721h"}}`), "ttl"},
		{"an unknown cache key", file(redis, `{"name":"m","provider":"p","cache":{"ttl":"1m","size":3}}`),
			`unknown key "size"`},
		{"stream_usage not a boolean", `{"providers":[{"name":"p","base_url":"http://h","stream_usage":"yes"}],` +
			`"models":[` + m + `]}`, "stream_usage"},
		{"prices not a list", file(`"prices":{}`, m), "prices"},
		{"a price for no provider", file(`"prices":[{"provider":"x","upstream_model":"m","input_per_million":1,`+
			`"output_per_million":1}]`, m), `provider "x" is not one of`},
		{"a price with no model", file(`"prices":[{"provider":"p","input_per_million":1,"output_per_million":1}]`, m),
			"upstream_model is required"},
		{"a price with no input", file(`"prices":[{"provider":"p","upstream_model":"m","output_per_million":1}]`, m),
			"input_per_million is required"},
		{"a price with no output", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1}]`, m),
			"output_per_million is required"},
		{"a price as a string", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":"1",`+
			`"output_per_million":1}]`, m), "input_per_million"},
		{"a negative price", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":-1,`+
			`"output_per_million":1}]`, m), "plain decimal"},
		{"a price with an exponent", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1e-3,`+
			`"output_per_million":1}]`, m), "plain decimal"},
		{"a price with 7 places", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1,`+
			`"output_per_million":0.0000001}]`, m), "output_per_million: 0.0000001"},
		{"a price with a leading zero", file(`"prices":[{"provider":"p","upstream_model":"m",`+
			`"input_per_million":01,"output_per_million":1}]`, m), "invalid"},
		{"a price past 1000", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1000.000001,`+
			`"output_per_million":1}]`, m), "more than 1000 dollars"},
		{"a price with five digits", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":10000,`+
			`"output_per_million":1}]`, m), "plain decimal"},
		{"a model priced twice", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1,`+
			`"output_per_million":1},{"provider":"p","upstream_model":"m","input_per_million":2,`+
			`"output_per_million":2}]`, m), `p's "m" is priced twice`},
		{"an unknown price key", file(`"prices":[{"provider":"p","upstream_model":"m","input_per_million":1,`+
			`"output_per_million":1,"currency":"EUR"}]`, m), `unknown key "currency"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file), env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestAPastedKeyOrPasswordIsNeverShown(t *testing.T) {
	const p = `"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"name":"m","provider":"p"}]`
	for _, file := range []string{
		`{"redis":{"addr":"h:1"},"clients":[{"name":"c","key_sha256":"sb-hidden-42"}],` + p + `}`,
		`{"redis":{"addr":"h:1"},"clients":[{"name":"c","key_sha256":"sb-hidden-42sb-hidden-42sb-hidden-42sb-hidden-` +
			`42sb-hidden-42sb"}],` + p + `}`,
		`{"redis":{"addr":"h:1","password_env":"sb-hidden-42"},` + p + `}`,
		`{"redis":{"addr":"h:1","password_env":"sb_hidden_42"},` + p + `}`,
	} {
		_, err := Parse([]byte(file), env(nil))
		if err == nil || strings.Contains(err.Error(), "hidden") {
			t.Errorf("%v", err)
		}
	}
}

func TestMicrodollarsAreExact(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "0.15": 150_000, "2.5": 2_500_000, "0.000001": 1, "999.999999": 999_999_999, "1000": 1_000_000_000,
		"0.1": 100_000, "10.000010": 10_000_010,
	} {
		if got, err := microdollars(in); err != nil || got != want {
			t.Errorf("%s: %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".5", "5.", "-1", "+1", "1e3", "00.1", "0.0000001", "1000.000001", "10000", "0x10",
		" 1", "1,5", "NaN"} {
		if got, err := microdollars(in); err == nil {
			t.Errorf("%q: %d, want an error", in, got)
		}
	}
}

func TestARateLimitsBoundsAndDefaults(t *testing.T) {
	limit := func(rl string) (*RateLimit, error) {
		c, err := Parse([]byte(`{"redis":{"addr":"h:1"},"clients":[{"name":"c","key_sha256":"`+hashOf("k")+
			`","rate_limit":`+rl+`}],"providers":[{"name":"p","base_url":"http://h/v1"}],`+
			`"models":[{"name":"m","provider":"p"}]}`), env(nil))
		if err != nil {
			return nil, err
		}
		return c.Clients[0].RateLimit, nil
	}
	for rl, want := range map[string]RateLimit{
		`{"requests_per_minute":0.5}`:                     {0.5, 1}, // rounded up: a burst of 0 would refuse everything
		`{"requests_per_minute":1.2}`:                     {1.2, 2},
		`{"requests_per_minute":60}`:                      {60, 60},
		`{"requests_per_minute":0.0001,"burst":4}`:        {0.0001, 4}, // 40,000 minutes to refill
		`{"requests_per_minute":1000000,"burst":1000000}`: {1_000_000, 1_000_000},
	} {
		if got, err := limit(rl); err != nil || *got != want {
			t.Errorf("%s: %+v %v, want %+v", rl, got, err, want)
		}
	}
	for _, rl := range []string{`{"requests_per_minute":0.0001,"burst":5}`, `{"requests_per_minute":1e-9}`,
		`{"requests_per_minute":0.000001,"burst":1000000}`} {
		if _, err := limit(rl); err == nil || !strings.Contains(err.Error(), "more than 30 days to refill") {
			t.Errorf("%s: %v", rl, err)
		}
	}
}
