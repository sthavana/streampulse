package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/metrics"
)

func sources(t *testing.T) Sources {
	t.Helper()
	reg := metrics.New()
	reg.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": "live"})
	reg.SetGauge("streampulse_manifest_fetch_seconds", "h", 0.042, map[string]string{"target": "live"})
	reg.SetGauge("streampulse_representation_count", "h", 2, map[string]string{"target": "live"})
	for _, v := range []string{"video/v0", "audio/en/a0"} {
		l := map[string]string{"target": "live", "variant": v}
		reg.SetGauge("streampulse_segment_count", "h", 300, l)
		reg.SetGauge("streampulse_playlist_window_seconds", "h", 600, l)
		reg.SetGauge("streampulse_stream_live", "h", 1, l)
	}
	reg.SetGauge("streampulse_probe_up", "h", 0, map[string]string{"target": "dead"})

	tr := alert.NewTracker(alert.TrackerConfig{}, alert.NewRecorder(10), nil)
	tr.Notify(alert.Finding{Target: "dead", Check: "manifest_fetch", Severity: alert.Critical, Message: "refused"})

	return Sources{
		Registry: reg, Tracker: tr, Recorder: alert.NewRecorder(10),
		Targets: []config.Target{
			{Name: "live", URL: "https://example.com/live.mpd", Type: "dash", IntervalSeconds: 6},
			{Name: "dead", URL: "http://127.0.0.1:1/x.m3u8"},
		},
		Started: time.Now().Add(-90 * time.Second),
	}
}

func TestBuildPivotsMetricsIntoStreams(t *testing.T) {
	st := Build(sources(t))

	if len(st.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(st.Targets))
	}
	live := st.Targets[1] // sorted by name: dead, live
	if live.Name != "live" {
		t.Fatalf("targets are not sorted by name: %+v", []string{st.Targets[0].Name, st.Targets[1].Name})
	}
	if live.Up == nil || !*live.Up {
		t.Errorf("live target should be up, got %v", live.Up)
	}
	if live.Live == nil || !*live.Live {
		t.Errorf("live target should read as live, got %v", live.Live)
	}
	if live.Format != "dash" || live.Interval != "6s" {
		t.Errorf("format/interval = %q/%q", live.Format, live.Interval)
	}
	// Target-level metrics stay on the target, not on a stream.
	if live.Metrics["manifest_fetch_seconds"] != 0.042 {
		t.Errorf("target metrics = %v", live.Metrics)
	}
	if len(live.Streams) != 2 || live.Streams[0].Name != "audio/en/a0" {
		t.Fatalf("streams = %+v, want two sorted by name", live.Streams)
	}
	if live.Streams[0].Metrics["segment_count"] != 300 {
		t.Errorf("stream metrics = %v", live.Streams[0].Metrics)
	}

	if st.Summary.Targets != 2 || st.Summary.TargetsUp != 1 || st.Summary.Streams != 2 {
		t.Errorf("summary = %+v", st.Summary)
	}
}

// A DASH representation has no per-stream manifest to be up or down, so
// "reachable" has to be inferred from it having segments at all.
func TestDASHStreamIsUpWithoutAVariantUpMetric(t *testing.T) {
	st := Build(sources(t))
	for _, s := range st.Targets[1].Streams {
		if s.Up == nil || !*s.Up {
			t.Errorf("stream %s should read as up from its segment count, got %v", s.Name, s.Up)
		}
	}
}

// Absent and false are different: a stream not yet probed must not render as
// down.
func TestUnobservedIsNotFalse(t *testing.T) {
	if got := boolOf(Values{}, "variant_up"); got != nil {
		t.Errorf("an unobserved gauge should be nil, got %v", *got)
	}
	if got := boolOf(Values{"variant_up": 0}, "variant_up"); got == nil || *got {
		t.Errorf("a zero gauge should be false, got %v", got)
	}
	if got := boolOf(Values{"variant_up": 1}, "variant_up"); got == nil || !*got {
		t.Errorf("a one gauge should be true, got %v", got)
	}
}

func TestIncidentsSurface(t *testing.T) {
	st := Build(sources(t))
	if len(st.Incidents) != 1 || st.Incidents[0].Check != "manifest_fetch" {
		t.Fatalf("incidents = %+v", st.Incidents)
	}
	if st.Summary.Firing != 1 {
		t.Errorf("summary.Firing = %d, want 1", st.Summary.Firing)
	}
	// and it is attributed to its target
	for _, tv := range st.Targets {
		if tv.Name == "dead" && tv.Firing != 1 {
			t.Errorf("dead target firing = %d, want 1", tv.Firing)
		}
	}
}

// Empty lists must cross into JSON as [] and never null: the page walks them
// without defending, and one null is one broken dashboard.
func TestEmptyListsAreNotNull(t *testing.T) {
	st := Build(Sources{Registry: metrics.New()})
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, field := range []string{`"targets":[]`, `"incidents":[]`, `"recent":[]`} {
		if !strings.Contains(body, field) {
			t.Errorf("expected %s in %s", field, body)
		}
	}
	if strings.Contains(body, "null") {
		t.Errorf("state contains a null: %s", body)
	}

	// And a configured target with nothing probed yet still gets a list.
	st = Build(Sources{Registry: metrics.New(), Targets: []config.Target{{Name: "x"}}})
	b, _ = json.Marshal(st)
	if !strings.Contains(string(b), `"streams":[]`) {
		t.Errorf("a target with no streams should carry [], got %s", b)
	}
}

// The page reads these field names. If Go renames one, the panel silently
// shows nothing, which no Go test would otherwise notice.
func TestJSONContractWithThePage(t *testing.T) {
	b, err := json.Marshal(Build(sources(t)))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"now", "uptime_seconds", "summary", "targets", "incidents", "recent"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("state is missing %q, which index.html reads", k)
		}
	}
	sum := raw["summary"].(map[string]any)
	for _, k := range []string{"targets", "targets_up", "streams", "streams_up", "firing", "suppressed"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("summary is missing %q", k)
		}
	}
	tgt := raw["targets"].([]any)[0].(map[string]any)
	for _, k := range []string{"name", "url", "format", "interval", "metrics", "streams", "firing"} {
		if _, ok := tgt[k]; !ok {
			t.Errorf("target is missing %q", k)
		}
	}
	inc := raw["incidents"].([]any)[0].(map[string]any)
	for _, k := range []string{"target", "check", "severity", "message", "first_seen", "count", "announced"} {
		if _, ok := inc[k]; !ok {
			t.Errorf("incident is missing %q", k)
		}
	}
}

// The metric names the page indexes by are the exported ones minus the
// prefix. A rename in the prober silently blanks a column otherwise.
func TestMetricKeysArePrefixStripped(t *testing.T) {
	st := Build(sources(t))
	if _, ok := st.Targets[1].Metrics["probe_up"]; !ok {
		t.Errorf("target metrics should be keyed without the streampulse_ prefix: %v", st.Targets[1].Metrics)
	}
	for k := range st.Targets[1].Metrics {
		if strings.HasPrefix(k, "streampulse_") {
			t.Errorf("metric key %q still carries the prefix", k)
		}
	}
}

func TestHandlerServesPageAndState(t *testing.T) {
	h := Handler(sources(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	for _, want := range []string{`id="body"`, `id="chips"`, "api/state"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("page is missing %q", want)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if len(st.Targets) != 2 {
		t.Errorf("state has %d targets", len(st.Targets))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}
