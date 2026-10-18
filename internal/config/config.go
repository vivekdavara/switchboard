// Package config loads the gateway's configuration: where it listens, its limits, the providers it forwards to and
// the models it serves.
//
// The file is JSON, read as strictly as a request: jsonstrict first (a key given twice is an error, not a silent
// override), then every key must be spelled exactly as documented. encoding/json's DisallowUnknownFields isn't
// enough on its own: it still fills a field from a key in any case, so a misspelt "Timeout" would pass as
// "timeout" where a reader expects it to be refused, and two spellings of one key would both be accepted.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vivekdavara/switchboard/internal/jsonstrict"
)

// Defaults for what the file leaves out.
const (
	DefaultListen           = "127.0.0.1:8108"
	DefaultMaxRequestBytes  = 4 << 20
	DefaultMaxResponseBytes = 16 << 20
	DefaultShutdownGrace    = 10 * time.Second
	DefaultTimeout          = 60 * time.Second
)

// Config is a loaded, checked configuration.
type Config struct {
	Listen           string
	MaxRequestBytes  int64
	MaxResponseBytes int64
	ShutdownGrace    time.Duration
	Providers        []*Provider
	Models           []Model

	routes map[string]Model
}

// Provider is an upstream that speaks the OpenAI API.
type Provider struct {
	Name    string
	BaseURL string // absolute, without a trailing slash: requests go to BaseURL + "/chat/completions"
	Timeout time.Duration
	// APIKey is the value of the environment variable APIKeyEnv names, read at load. It is never in the file and
	// never logged.
	APIKey    string
	APIKeyEnv string
}

// Model is a model the gateway serves and where it sends it.
type Model struct {
	Name          string
	Provider      *Provider
	UpstreamModel string // the provider's name for it
}

// Route returns the model a client asked for by name.
func (c *Config) Route(name string) (Model, bool) {
	m, ok := c.routes[name]
	return m, ok
}

// Load reads and checks the file at path, taking provider keys from getenv.
func Load(path string, getenv func(string) string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b, getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

var (
	// Provider names go into a response header, so they keep to characters any header value can hold.
	providerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Parse checks a configuration file's contents, taking provider keys from getenv.
func Parse(b []byte, getenv func(string) string) (*Config, error) {
	top, err := jsonstrict.ParseObject(b)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Listen:           DefaultListen,
		MaxRequestBytes:  DefaultMaxRequestBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
		ShutdownGrace:    DefaultShutdownGrace,
		routes:           map[string]Model{},
	}
	var grace string
	var providers, models []json.RawMessage
	if err := decodeExact("the file", top, fields{
		"listen":             &c.Listen,
		"max_request_bytes":  &c.MaxRequestBytes,
		"max_response_bytes": &c.MaxResponseBytes,
		"shutdown_grace":     &grace,
		"providers":          &providers,
		"models":             &models,
	}); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nil, fmt.Errorf("listen: %q is not a host:port address", c.Listen)
	}
	if c.MaxRequestBytes < 1 || c.MaxResponseBytes < 1 {
		return nil, errors.New("max_request_bytes and max_response_bytes must be positive")
	}
	if grace != "" {
		if c.ShutdownGrace, err = time.ParseDuration(grace); err != nil || c.ShutdownGrace < 0 {
			return nil, fmt.Errorf("shutdown_grace: %q is not a duration such as 10s", grace)
		}
	}

	if len(providers) == 0 {
		return nil, errors.New("providers: at least one is required")
	}
	byName := map[string]*Provider{}
	for i, raw := range providers {
		p, err := parseProvider(raw, getenv)
		if err != nil {
			return nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
		if byName[p.Name] != nil {
			return nil, fmt.Errorf("providers[%d]: the name %q is used twice", i, p.Name)
		}
		byName[p.Name] = p
		c.Providers = append(c.Providers, p)
	}

	if len(models) == 0 {
		return nil, errors.New("models: at least one is required")
	}
	for i, raw := range models {
		var m Model
		var provider string
		members, err := object(raw)
		if err == nil {
			err = decodeExact("the model", members, fields{
				"name": &m.Name, "provider": &provider, "upstream_model": &m.UpstreamModel,
			})
		}
		switch {
		case err != nil:
		case m.Name == "":
			err = errors.New("name is required")
		case c.routes[m.Name].Name != "":
			err = fmt.Errorf("the name %q is used twice", m.Name)
		case byName[provider] == nil:
			err = fmt.Errorf("provider %q is not one of the providers", provider)
		}
		if err != nil {
			return nil, fmt.Errorf("models[%d]: %w", i, err)
		}
		m.Provider = byName[provider]
		if m.UpstreamModel == "" {
			m.UpstreamModel = m.Name
		}
		c.Models = append(c.Models, m)
		c.routes[m.Name] = m
	}
	return c, nil
}

func parseProvider(raw json.RawMessage, getenv func(string) string) (*Provider, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	p := &Provider{Timeout: DefaultTimeout}
	var base, timeout string
	if err := decodeExact("the provider", members, fields{
		"name": &p.Name, "base_url": &base, "api_key_env": &p.APIKeyEnv, "timeout": &timeout,
	}); err != nil {
		return nil, err
	}
	if !providerName.MatchString(p.Name) {
		return nil, fmt.Errorf("name %q: use 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or "+
			"digit", p.Name)
	}
	u, err := url.Parse(base)
	switch {
	case base == "":
		return nil, errors.New("base_url is required")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, fmt.Errorf("base_url %q: want an absolute http or https URL", base)
	case u.User != nil:
		// A password in the URL would be a secret in the file, and in every log line that prints the URL.
		return nil, fmt.Errorf("base_url %q: credentials don't belong in the URL; use api_key_env", u.Redacted())
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(base, "?") || strings.Contains(base, "#"):
		return nil, fmt.Errorf("base_url %q: no query or fragment, please", base)
	}
	p.BaseURL = strings.TrimRight(base, "/")
	if timeout != "" {
		if p.Timeout, err = time.ParseDuration(timeout); err != nil || p.Timeout <= 0 {
			return nil, fmt.Errorf("timeout: %q is not a positive duration such as 30s", timeout)
		}
	}
	if p.APIKeyEnv != "" {
		if !envName.MatchString(p.APIKeyEnv) {
			return nil, fmt.Errorf("api_key_env %q is not an environment variable's name", p.APIKeyEnv)
		}
		if p.APIKey = getenv(p.APIKeyEnv); p.APIKey == "" {
			return nil, fmt.Errorf("the environment variable %s (api_key_env) is unset or empty", p.APIKeyEnv)
		}
	}
	return p, nil
}

// fields maps each key an object may have to where its value goes.
type fields map[string]any

// decodeExact decodes each member into its field, refusing keys that aren't spelled exactly as one of the fields.
func decodeExact(what string, members []jsonstrict.Member, into fields) error {
	for _, m := range members {
		dst, ok := into[m.Key]
		if !ok {
			known := make([]string, 0, len(into))
			for k := range into {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("%s has an unknown key %q (known: %s)", what, m.Key, strings.Join(known, ", "))
		}
		if string(m.Value) == "null" {
			return fmt.Errorf("%s: %s is null; leave it out for the default", what, m.Key)
		}
		if err := json.Unmarshal(m.Value, dst); err != nil {
			return fmt.Errorf("%s: %s: %w", what, m.Key, err)
		}
	}
	return nil
}

// object reads a nested value that must be an object. jsonstrict has already checked the whole file, so only the
// type can be wrong.
func object(raw json.RawMessage) ([]jsonstrict.Member, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("must be an object")
	}
	return jsonstrict.ParseObject(raw)
}
