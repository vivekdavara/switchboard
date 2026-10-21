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
		// Port 0 is a free port each, so two of them never clash (the tests listen that way).
		if _, port, _ := net.SplitHostPort(m.Listen); m.Listen == c.Listen && port != "0" {
			return fmt.Errorf("metrics: listen %q is the API's own address; give the metrics a port of their own",
				m.Listen)
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
