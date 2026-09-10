package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const oneTarget = `"targets":[{"name":"c","url":"http://x/m.m3u8"}]`

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, `{`+oneTarget+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsAddr != ":9090" {
		t.Errorf("MetricsAddr = %q, want :9090", c.MetricsAddr)
	}
	if c.Alerting.ResolveAfterSeconds != 90 {
		t.Errorf("ResolveAfterSeconds = %d, want 90", c.Alerting.ResolveAfterSeconds)
	}
	if c.Alerting.SweepSeconds != 10 {
		t.Errorf("SweepSeconds = %d, want 10", c.Alerting.SweepSeconds)
	}
}

func TestLoadDailyWindow(t *testing.T) {
	c, err := Load(write(t, `{"maintenance":[{
		"name":"nightly","timezone":"America/Los_Angeles",
		"targets":["c"],"daily":{"start":"02:00","end":"04:00","days":["Sat","Sun"]}
	}],`+oneTarget+`}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 1 {
		t.Fatalf("got %d windows, want 1", len(s))
	}
	w := s[0]
	if w.Daily == nil {
		t.Fatal("Daily not populated")
	}
	if w.Daily.StartMin != 120 || w.Daily.EndMin != 240 {
		t.Errorf("minutes = %d..%d, want 120..240", w.Daily.StartMin, w.Daily.EndMin)
	}
	if len(w.Daily.Days) != 2 || w.Daily.Days[0] != time.Saturday || w.Daily.Days[1] != time.Sunday {
		t.Errorf("Days = %v, want [Saturday Sunday]", w.Daily.Days)
	}
	if w.Loc.String() != "America/Los_Angeles" {
		t.Errorf("Loc = %v, want America/Los_Angeles", w.Loc)
	}
}

func TestLoadOneOffWindow(t *testing.T) {
	c, err := Load(write(t, `{"maintenance":[{
		"name":"upgrade","start":"2026-09-15T22:00:00Z","end":"2026-09-16T02:00:00Z"
	}],`+oneTarget+`}`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.Schedule()
	w := s[0]
	if w.Daily != nil {
		t.Error("one-off window should not have a Daily spec")
	}
	if !w.Start.Equal(time.Date(2026, 9, 15, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("Start = %v", w.Start)
	}
	if !w.End.Equal(time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("End = %v", w.End)
	}
}

// A malformed window must stop startup: silently ignoring it would leave the
// operator believing they are covered.
func TestLoadRejectsBadWindows(t *testing.T) {
	cases := map[string]string{
		"unknown timezone":  `{"name":"w","timezone":"Mars/Olympus","daily":{"start":"02:00","end":"04:00"}}`,
		"bad clock":         `{"name":"w","daily":{"start":"2am","end":"04:00"}}`,
		"bad weekday":       `{"name":"w","daily":{"start":"02:00","end":"04:00","days":["Caturday"]}}`,
		"no schedule":       `{"name":"w"}`,
		"missing name":      `{"daily":{"start":"02:00","end":"04:00"}}`,
		"both forms":        `{"name":"w","start":"2026-09-15T22:00:00Z","end":"2026-09-16T02:00:00Z","daily":{"start":"02:00","end":"04:00"}}`,
		"half a one-off":    `{"name":"w","start":"2026-09-15T22:00:00Z"}`,
		"end before start":  `{"name":"w","start":"2026-09-16T02:00:00Z","end":"2026-09-15T22:00:00Z"}`,
		"identical daily":   `{"name":"w","daily":{"start":"02:00","end":"02:00"}}`,
		"unparseable start": `{"name":"w","start":"tomorrow","end":"2026-09-16T02:00:00Z"}`,
	}
	for name, win := range cases {
		_, err := Load(write(t, `{"maintenance":[`+win+`],`+oneTarget+`}`))
		if err == nil {
			t.Errorf("%s: expected an error, got none", name)
			continue
		}
		if strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: unhelpful error %v", name, err)
		}
	}
}

func TestLoadNoMaintenanceIsFine(t *testing.T) {
	c, err := Load(write(t, `{`+oneTarget+`}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 0 {
		t.Errorf("got %d windows, want 0", len(s))
	}
}

func TestLoadRejectsEmptyTargets(t *testing.T) {
	if _, err := Load(write(t, `{"targets":[]}`)); err == nil {
		t.Error("expected an error for a config with no targets")
	}
}
