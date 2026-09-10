// Package config loads the prober configuration. JSON is used deliberately to
// keep the binary dependency-free; swapping to YAML is a one-line change once a
// YAML dependency is acceptable.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Target is one stream to monitor.
type Target struct {
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	IntervalSeconds int     `json:"interval_seconds"`
	MaxVariants     int     `json:"max_variants"`       // 0 = probe all variants
	SegmentSample   int     `json:"segment_sample"`     // segments per variant to fetch-check (0 = none)
	ExpectLive      bool    `json:"expect_live"`        // flag if an ENDLIST appears
	MinWindowSec    float64 `json:"min_window_seconds"` // warn if live window shorter than this (0 = skip)
}

// Config is the top-level configuration.
type Config struct {
	MetricsAddr  string   `json:"metrics_addr"`
	SlackWebhook string   `json:"slack_webhook,omitempty"`
	Targets      []Target `json:"targets"`
}

// Interval returns the poll interval, defaulting to 10s.
func (t Target) Interval() time.Duration {
	if t.IntervalSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(t.IntervalSeconds) * time.Second
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9090"
	}
	if len(c.Targets) == 0 {
		return nil, fmt.Errorf("config has no targets")
	}
	return &c, nil
}
