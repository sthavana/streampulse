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

// Alerting tunes the incident lifecycle: how findings are deduplicated into
// incidents and when those incidents are declared cleared.
type Alerting struct {
	// ForSeconds is how long a condition must persist before it is notified.
	// 0 notifies on the first observation.
	ForSeconds int `json:"for_seconds"`
	// ResolveAfterSeconds is the quiet period after which a firing incident is
	// declared cleared. Defaults to 90.
	ResolveAfterSeconds int `json:"resolve_after_seconds"`
	// RepeatEverySeconds re-notifies a still-firing incident. 0 disables it.
	RepeatEverySeconds int `json:"repeat_every_seconds"`
	// SweepSeconds is how often expired incidents are checked. Defaults to 10.
	SweepSeconds int `json:"sweep_seconds"`
}

func (a Alerting) For() time.Duration    { return time.Duration(a.ForSeconds) * time.Second }
func (a Alerting) Repeat() time.Duration { return time.Duration(a.RepeatEverySeconds) * time.Second }

func (a Alerting) ResolveAfter() time.Duration {
	return time.Duration(a.ResolveAfterSeconds) * time.Second
}

func (a Alerting) Sweep() time.Duration { return time.Duration(a.SweepSeconds) * time.Second }

// Config is the top-level configuration.
type Config struct {
	MetricsAddr  string   `json:"metrics_addr"`
	SlackWebhook string   `json:"slack_webhook,omitempty"`
	Alerting     Alerting `json:"alerting"`
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
	if c.Alerting.ResolveAfterSeconds <= 0 {
		c.Alerting.ResolveAfterSeconds = 90
	}
	if c.Alerting.SweepSeconds <= 0 {
		c.Alerting.SweepSeconds = 10
	}
	if len(c.Targets) == 0 {
		return nil, fmt.Errorf("config has no targets")
	}
	return &c, nil
}
