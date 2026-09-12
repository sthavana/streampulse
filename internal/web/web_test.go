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
	"streampulse/internal/inspect"
	"streampulse/internal/metrics"
)

// fixed is a target set that does not change, which is what a test wants and
// what the running prober deliberately is not.
func fixed(ts ...config.Target) func() []config.Target {
	return func() []config.Target { return ts }
}

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
		Targets: fixed(
			config.Target{Name: "live", URL: "https://example.com/live.mpd", Type: "dash", IntervalSeconds: 6},
			config.Target{Name: "dead", URL: "http://127.0.0.1:1/x.m3u8"},
		),
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
	st = Build(Sources{Registry: metrics.New(), Targets: fixed(config.Target{Name: "x"})})
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

// The media column is decided per table, not per row: a row whose capture
// failed this cycle must still render the cell or its columns shift left out
// from under the header. The page needs `thumb` and `audio` to be reliably
// absent-or-present per stream to make that decision.
func TestFramesSurfacePerStream(t *testing.T) {
	src := sources(t)
	frames := inspect.NewFrames(10)
	frames.Put(inspect.FrameKey("live", "video/v0"), inspect.Frame{
		JPEG: []byte("\xff\xd8jpeg"), At: time.Unix(1700000000, 0),
	})
	frames.Put(inspect.FrameKey("live", "audio/en/a0"), inspect.Frame{
		HasAudio: true, PeakDBFS: -12, MeanDBFS: -24, At: time.Unix(1700000000, 0),
	})
	src.Frames = frames

	st := Build(src)
	byName := map[string]Stream{}
	for _, s := range st.Targets[1].Streams {
		byName[s.Name] = s
	}

	video := byName["video/v0"]
	if video.Thumb == "" || !strings.Contains(video.Thumb, "target=live") {
		t.Errorf("video stream thumb = %q", video.Thumb)
	}
	// The capture time is in the URL so a browser fetches the new picture
	// rather than the one it already has.
	if !strings.Contains(video.Thumb, "t=1700000000000000000") {
		t.Errorf("thumb URL should carry the capture time, got %q", video.Thumb)
	}
	if video.Audio != nil {
		t.Errorf("a video-only stream should report no audio, got %+v", video.Audio)
	}

	audio := byName["audio/en/a0"]
	if audio.Thumb != "" {
		t.Errorf("an audio stream has no picture, got %q", audio.Thumb)
	}
	if audio.Audio == nil || audio.Audio.PeakDBFS != -12 || audio.Audio.Silent {
		t.Errorf("audio level = %+v, want -12 dBFS and not silent", audio.Audio)
	}
}

func TestSilentAudioIsFlaggedInTheAPI(t *testing.T) {
	src := sources(t)
	frames := inspect.NewFrames(10)
	frames.Put(inspect.FrameKey("live", "audio/en/a0"), inspect.Frame{
		HasAudio: true, PeakDBFS: -91, MeanDBFS: -91,
	})
	src.Frames = frames

	for _, s := range Build(src).Targets[1].Streams {
		if s.Name == "audio/en/a0" && (s.Audio == nil || !s.Audio.Silent) {
			t.Errorf("digital silence should be flagged, got %+v", s.Audio)
		}
	}
}

// No capture configured means no pictures and no crash.
func TestNoFramesStoreIsFine(t *testing.T) {
	st := Build(sources(t)) // Frames is nil
	for _, tg := range st.Targets {
		for _, s := range tg.Streams {
			if s.Thumb != "" || s.Audio != nil {
				t.Errorf("%s reported media with no frame store", s.Name)
			}
		}
	}
}

// The frame endpoint serves the bytes, and 404s for a stream that has none.
func TestFrameEndpoint(t *testing.T) {
	src := sources(t)
	frames := inspect.NewFrames(10)
	frames.Put(inspect.FrameKey("live", "video/v0"), inspect.Frame{JPEG: []byte("\xff\xd8jpeg")})
	src.Frames = frames
	h := Handler(src)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/frame?target=live&variant=video/v0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET frame = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content type = %q", ct)
	}
	if rec.Body.String() != "\xff\xd8jpeg" {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/frame?target=live&variant=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a stream with no frame should 404, got %d", rec.Code)
	}
}

// The UI reads the live target set, so a stream added by a config reload
// appears without a restart -- and one removed stops being listed.
func TestTargetsAreReadLive(t *testing.T) {
	current := []config.Target{{Name: "a", URL: "http://a/x.m3u8"}}
	src := Sources{Registry: metrics.New(), Targets: func() []config.Target { return current }}

	if got := Build(src).Targets; len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("targets = %+v", got)
	}
	current = append(current, config.Target{Name: "b", URL: "http://b/x.m3u8"})
	if got := Build(src).Targets; len(got) != 2 {
		t.Errorf("a target added after startup did not appear: %+v", got)
	}
	current = nil
	if got := Build(src).Targets; len(got) != 0 {
		t.Errorf("a removed target is still listed: %+v", got)
	}
}

// The sparklines are what make the page worth glancing at rather than reading,
// and they arrive through the same pivot as the values.
func TestTrendsReachTargetsAndStreams(t *testing.T) {
	reg := metrics.New()
	reg.TrackHistory(60, "streampulse_manifest_fetch_seconds", "streampulse_segment_ttfb_seconds")
	for i := 0; i < 3; i++ {
		reg.SetGauge("streampulse_manifest_fetch_seconds", "h", 0.01*float64(i), map[string]string{"target": "live"})
		reg.SetGauge("streampulse_segment_ttfb_seconds", "h", 0.1*float64(i),
			map[string]string{"target": "live", "variant": "video/v0"})
		// Not registered for history, so it must arrive as a value alone.
		reg.SetGauge("streampulse_segment_count", "h", float64(i),
			map[string]string{"target": "live", "variant": "video/v0"})
	}
	src := Sources{Registry: reg, Targets: fixed(config.Target{Name: "live", URL: "http://x/y.m3u8"})}

	tgt := Build(src).Targets[0]
	if got := tgt.Trends["manifest_fetch_seconds"]; len(got) != 3 {
		t.Fatalf("target trend = %v, want three points", got)
	}
	if len(tgt.Streams) != 1 {
		t.Fatalf("streams = %+v", tgt.Streams)
	}
	st := tgt.Streams[0]
	if got := st.Trends["segment_ttfb_seconds"]; len(got) != 3 || got[2] != 0.2 {
		t.Fatalf("stream trend = %v, want three points ending at the newest", got)
	}
	if _, ok := st.Trends["segment_count"]; ok {
		t.Errorf("an untracked metric brought a trend along: %v", st.Trends)
	}
}

// A page where the broken target is third alphabetically is a page you have to
// read rather than glance at.
func TestTroubleSortsFirst(t *testing.T) {
	reg := metrics.New()
	reg.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": "aaa-healthy"})
	reg.SetGauge("streampulse_probe_up", "h", 0, map[string]string{"target": "zzz-down"})

	tr := alert.NewTracker(alert.TrackerConfig{}, alert.NewRecorder(10), nil)
	tr.Notify(alert.Finding{Target: "mmm-firing", Check: "edge_stale", Severity: alert.Warning, Message: "x"})

	src := Sources{Registry: reg, Tracker: tr, Targets: fixed(
		config.Target{Name: "aaa-healthy", URL: "http://a/x.m3u8"},
		config.Target{Name: "mmm-firing", URL: "http://m/x.m3u8"},
		config.Target{Name: "zzz-down", URL: "http://z/x.m3u8"},
	)}

	var order []string
	for _, tg := range Build(src).Targets {
		order = append(order, tg.Name)
	}
	want := []string{"mmm-firing", "zzz-down", "aaa-healthy"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want firing, then down, then healthy: %v", order, want)
	}
}

// Unprobed is not the same as down. A target that has said nothing yet must
// not push a working stream off the top of the page.
func TestUnprobedDoesNotSortAsDown(t *testing.T) {
	reg := metrics.New()
	reg.SetGauge("streampulse_probe_up", "h", 1, map[string]string{"target": "aaa-healthy"})

	src := Sources{Registry: reg, Targets: fixed(
		config.Target{Name: "aaa-healthy", URL: "http://a/x.m3u8"},
		config.Target{Name: "bbb-unprobed", URL: "http://b/x.m3u8"},
	)}
	if got := Build(src).Targets[0].Name; got != "aaa-healthy" {
		t.Errorf("first target = %q, want the alphabetical order kept", got)
	}
}
