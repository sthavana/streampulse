// Package config loads the prober configuration. JSON is used deliberately to
// keep the binary dependency-free; swapping to YAML is a one-line change once a
// YAML dependency is acceptable.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"streampulse/internal/alert"
)

// Target is one stream to monitor.
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Type selects the manifest parser: "hls", "dash", or empty to detect it
	// from the response body. Detection is reliable enough to be the default,
	// but naming the type turns "this is not a manifest" into a parse error
	// that says what was actually wrong.
	Type            string   `json:"type,omitempty"`
	IntervalSeconds int      `json:"interval_seconds"`
	MaxVariants     int      `json:"max_variants"`    // 0 = probe all variants
	MaxRenditions   int      `json:"max_renditions"`  // 0 = probe all EXT-X-MEDIA renditions that have a URI
	RenditionTypes  []string `json:"rendition_types"` // e.g. ["AUDIO"]; empty = every type
	SegmentSample   int      `json:"segment_sample"`  // segments per variant to fetch-check (0 = none)

	// NoCache sends Cache-Control: no-cache, bypassing the CDN edge. Off by
	// default on purpose: viewers do not watch the origin, so a stale edge is
	// a real outage and a prober that never sees one is measuring the wrong
	// thing. Turn it on for a second target pointed past the cache, and
	// compare the two.
	NoCache bool              `json:"no_cache,omitempty"`
	Headers map[string]string `json:"headers,omitempty"` // extra request headers (auth tokens, CDN overrides)

	// DASH. MaxRepresentations is per adaptation set rather than per
	// manifest: a flat cap on a ladder that lists video before audio would
	// silently stop probing audio altogether, which is the break this tool
	// exists to catch. 1 probes the top rung of every track.
	MaxRepresentations  int      `json:"max_representations,omitempty"`  // 0 = probe every representation
	RepresentationTypes []string `json:"representation_types,omitempty"` // e.g. ["video","audio"]; empty = every type

	ExpectLive   bool    `json:"expect_live"`        // flag if an ENDLIST appears
	MinWindowSec float64 `json:"min_window_seconds"` // warn if live window shorter than this (0 = skip)

	// DRM / EXT-X-KEY.
	ExpectEncrypted   bool `json:"expect_encrypted"`         // flag any segment served in the clear
	FetchKeys         bool `json:"fetch_keys"`               // retrieve key URIs to prove availability
	KeyRotationMaxSec int  `json:"key_rotation_max_seconds"` // warn if the key has not rotated (0 = skip)
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
	// StateFile persists open incidents across a restart, so a redeploy does
	// not re-announce faults everyone has already been told about. Empty
	// disables it, which is the default: it needs somewhere writable to live,
	// and that is a deployment decision rather than something to guess at.
	StateFile string `json:"state_file,omitempty"`
}

func (a Alerting) For() time.Duration    { return time.Duration(a.ForSeconds) * time.Second }
func (a Alerting) Repeat() time.Duration { return time.Duration(a.RepeatEverySeconds) * time.Second }

func (a Alerting) ResolveAfter() time.Duration {
	return time.Duration(a.ResolveAfterSeconds) * time.Second
}

func (a Alerting) Sweep() time.Duration { return time.Duration(a.SweepSeconds) * time.Second }

// DailySpec is a maintenance window that recurs every day, in "15:04"
// wall-clock time in the window's timezone.
type DailySpec struct {
	Start string   `json:"start"`
	End   string   `json:"end"`
	Days  []string `json:"days,omitempty"` // e.g. ["Sat","Sun"]; empty = every day
}

// MaintenanceWindow is one configured period of suppressed alerting. Give it
// either Start/End (one-off, RFC3339) or Daily (recurring), not both.
type MaintenanceWindow struct {
	Name     string     `json:"name"`
	Targets  []string   `json:"targets,omitempty"` // empty = all targets
	Checks   []string   `json:"checks,omitempty"`  // empty = all checks
	Timezone string     `json:"timezone,omitempty"`
	Start    string     `json:"start,omitempty"`
	End      string     `json:"end,omitempty"`
	Daily    *DailySpec `json:"daily,omitempty"`
}

// window converts the config shape into the runtime type, rejecting anything
// malformed. A silently-ignored window is worse than a refusal to start: the
// operator believes they are covered and finds out during the maintenance.
func (m MaintenanceWindow) window() (alert.Window, error) {
	w := alert.Window{Name: m.Name, Targets: m.Targets, Checks: m.Checks, Loc: time.UTC}
	if m.Name == "" {
		return w, fmt.Errorf("maintenance window needs a name")
	}
	if m.Timezone != "" {
		loc, err := time.LoadLocation(m.Timezone)
		if err != nil {
			return w, fmt.Errorf("window %q: %w", m.Name, err)
		}
		w.Loc = loc
	}

	hasOneOff := m.Start != "" || m.End != ""
	if hasOneOff && m.Daily != nil {
		return w, fmt.Errorf("window %q: set either start/end or daily, not both", m.Name)
	}

	switch {
	case m.Daily != nil:
		start, err := alert.ParseClock(m.Daily.Start)
		if err != nil {
			return w, fmt.Errorf("window %q: %w", m.Name, err)
		}
		end, err := alert.ParseClock(m.Daily.End)
		if err != nil {
			return w, fmt.Errorf("window %q: %w", m.Name, err)
		}
		if start == end {
			return w, fmt.Errorf("window %q: daily start and end are identical", m.Name)
		}
		d := &alert.Daily{StartMin: start, EndMin: end}
		for _, name := range m.Daily.Days {
			wd, err := alert.ParseWeekday(name)
			if err != nil {
				return w, fmt.Errorf("window %q: %w", m.Name, err)
			}
			d.Days = append(d.Days, wd)
		}
		w.Daily = d

	case hasOneOff:
		if m.Start == "" || m.End == "" {
			return w, fmt.Errorf("window %q: a one-off window needs both start and end", m.Name)
		}
		start, err := time.ParseInLocation(time.RFC3339, m.Start, w.Loc)
		if err != nil {
			return w, fmt.Errorf("window %q start: %w", m.Name, err)
		}
		end, err := time.ParseInLocation(time.RFC3339, m.End, w.Loc)
		if err != nil {
			return w, fmt.Errorf("window %q end: %w", m.Name, err)
		}
		if !end.After(start) {
			return w, fmt.Errorf("window %q: end is not after start", m.Name)
		}
		w.Start, w.End = start, end

	default:
		return w, fmt.Errorf("window %q: needs either start/end or daily", m.Name)
	}
	return w, nil
}

// Config is the top-level configuration.
type Config struct {
	MetricsAddr  string              `json:"metrics_addr"`
	SlackWebhook string              `json:"slack_webhook,omitempty"`
	Alerting     Alerting            `json:"alerting"`
	Maintenance  []MaintenanceWindow `json:"maintenance,omitempty"`
	Targets      []Target            `json:"targets"`
}

// Schedule builds the runtime maintenance schedule, validating every window.
func (c *Config) Schedule() (alert.Schedule, error) {
	var s alert.Schedule
	for _, m := range c.Maintenance {
		w, err := m.window()
		if err != nil {
			return nil, err
		}
		s = append(s, w)
	}
	return s, nil
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
	for _, t := range c.Targets {
		switch strings.ToLower(t.Type) {
		case "", "hls", "dash":
		default:
			return nil, fmt.Errorf("target %q: unknown type %q, want \"hls\", \"dash\", or empty to auto-detect", t.Name, t.Type)
		}
	}
	// Validate maintenance windows at load time so a typo fails at startup
	// rather than during the maintenance it was meant to cover.
	if _, err := c.Schedule(); err != nil {
		return nil, err
	}
	return &c, nil
}
