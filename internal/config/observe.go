package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
)

// Metrics says where the Prometheus metrics are served (day 4): GET /metrics on a listener of its own, so scraping
// needs no client key and can be firewalled apart from the API (DESIGN.md, "Metrics (day 4)").
type Metrics struct {
	Listen string // host:port
}

// Log is how the gateway logs (day 4): JSON lines (the default) or logfmt-style text, on stderr, from Level up. At
// info (the default) there is a line per chat completion; at warn, only failures and refusals.
type Log struct {
	Level slog.Level
	Text  bool
}

// parseDay4 reads the metrics and log sections, if the file has them.
func (c *Config) parseDay4(metrics, logging json.RawMessage) error {
	if metrics != nil {
		m, err := parseMetrics(metrics)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		// The same port is refused whatever the hosts: ":8108" and "127.0.0.1:8108" both bind on macOS, and the more
		// specific one then takes every local connection to the API's port (found in the day-4 review: the chat
		// completions got the metrics handler's 404). Ports compare as the numbers net.Listen reads, so "08108",
		// "+8108" and a service name are caught too, and "", "0" and "00" are each a free port, which never clash
		// (both found in the day-4 second review).
		if port, apiPort := portNumber(m.Listen), portNumber(c.Listen); port == apiPort && port > 0 {
			return fmt.Errorf("metrics: listen %q has the API's port (listen %q); give the metrics a port of their own",
				m.Listen, c.Listen)
		}
		c.Metrics = m
	}
	if logging != nil {
		if err := parseLog(logging, &c.Log); err != nil {
			return fmt.Errorf("log: %w", err)
		}
	}
	return nil
}

// portNumber is the port a host:port address listens on, as net.Listen reads it; -1 if it has none.
func portNumber(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil {
		return -1
	}
	return n
}

func parseMetrics(raw json.RawMessage) (*Metrics, error) {
	members, err := object(raw)
	if err != nil {
		return nil, err
	}
	m := &Metrics{}
	if err := decodeExact("metrics", members, fields{"listen": &m.Listen}); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(m.Listen); err != nil {
		return nil, errors.New("listen must be a host:port address, such as 127.0.0.1:9108")
	}
	return m, nil
}

func parseLog(raw json.RawMessage, l *Log) error {
	members, err := object(raw)
	if err != nil {
		return err
	}
	var level, format string
	if err := decodeExact("log", members, fields{"level": &level, "format": &format}); err != nil {
		return err
	}
	switch level {
	case "", "info":
		l.Level = slog.LevelInfo
	case "warn":
		l.Level = slog.LevelWarn
	case "error":
		l.Level = slog.LevelError
	default:
		return fmt.Errorf(`level must be "info", "warn" or "error", not %q`, level)
	}
	switch format {
	case "", "json":
	case "text":
		l.Text = true
	default:
		return fmt.Errorf(`format must be "json" or "text", not %q`, format)
	}
	return nil
}
