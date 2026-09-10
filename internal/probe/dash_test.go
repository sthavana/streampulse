package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streampulse/internal/config"
	"streampulse/internal/metrics"
)

// A two-track static MPD: one video representation and one audio, four
// segments in total.
const staticMPD = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT8S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate media="$RepresentationID$/seg-$Number$.m4s" initialization="$RepresentationID$/init.mp4"
                       timescale="1" duration="4" startNumber="1"/>
      <Representation id="v720" bandwidth="2000000" width="1280" height="720"/>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en">
      <SegmentTemplate media="$RepresentationID$/seg-$Number$.m4s" initialization="$RepresentationID$/init.mp4"
                       timescale="1" duration="4" startNumber="1"/>
      <Representation id="a128" bandwidth="128000"/>
    </AdaptationSet>
  </Period>
</MPD>`

// hits records which paths an origin was asked for.
type hits struct {
	mu   sync.Mutex
	seen []string
}

func (h *hits) add(p string) {
	h.mu.Lock()
	h.seen = append(h.seen, p)
	h.mu.Unlock()
}

func (h *hits) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.seen...)
	sort.Strings(out)
	return out
}

func (h *hits) got(p string) bool {
	for _, s := range h.paths() {
		if s == p {
			return true
		}
	}
	return false
}

// dashOrigin serves mpd at /manifest.mpd and a two-byte body for anything
// else, unless the path is in broken.
func dashOrigin(mpd string, broken map[string]bool) (*httptest.Server, *hits) {
	h := &hits{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.add(r.URL.Path)
		switch {
		case r.URL.Path == "/manifest.mpd":
			w.Header().Set("Content-Type", "application/dash+xml")
			_, _ = w.Write([]byte(mpd))
		case broken[r.URL.Path]:
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte("\x00\x00"))
		}
	}))
	return srv, h
}

func TestDASHPipeline(t *testing.T) {
	origin, h := dashOrigin(staticMPD, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: origin.URL + "/manifest.mpd", SegmentSample: 2,
	})

	if len(cap.findings) != 0 {
		t.Fatalf("a healthy MPD produced findings: %+v", cap.findings)
	}
	for _, want := range []string{"/v720/seg-1.m4s", "/v720/seg-2.m4s", "/a128/seg-1.m4s", "/a128/seg-2.m4s"} {
		if !h.got(want) {
			t.Errorf("segment %s was never fetched; got %v", want, h.paths())
		}
	}
}

// The DASH form of the gap that EXT-X-MEDIA closed for HLS: video is fine and
// the audio track is dead. The finding has to name the audio representation,
// not the manifest.
func TestBrokenDASHAudioIsAttributedToItsRepresentation(t *testing.T) {
	origin, _ := dashOrigin(staticMPD, map[string]bool{"/a128/seg-2.m4s": true})
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: origin.URL + "/manifest.mpd", SegmentSample: 2,
	})

	if !cap.has("segment_availability") {
		t.Fatalf("a broken audio segment went undetected: %+v", cap.findings)
	}
	var got string
	for _, f := range cap.findings {
		if f.Check == "segment_availability" {
			got = f.Variant
		}
	}
	if got != "audio/en/a128" {
		t.Errorf("finding attributed to variant %q, want audio/en/a128", got)
	}
}

// A live MPD: only the segments inside the availability window may be probed.
// Asking for one the packager has not published yet is a 404 that means
// nothing, and asking for the whole hour would be 900 requests per poll.
func TestDASHLiveProbesOnlyTheAvailabilityWindow(t *testing.T) {
	const liveMPD = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	     availabilityStartTime="2026-01-01T00:00:00Z" timeShiftBufferDepth="PT20S" minimumUpdatePeriod="PT4S">
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	origin, h := dashOrigin(liveMPD, nil)
	defer origin.Close()

	pr, _ := newTestProber(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)) // one hour in
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "live", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if len(cap.findings) != 0 {
		t.Fatalf("a healthy live MPD produced findings: %+v", cap.findings)
	}
	// One hour of 4s segments: number 900 ends exactly at the live edge.
	if !h.got("/v/900.m4s") {
		t.Errorf("expected the live-edge segment to be sampled, got %v", h.paths())
	}
	if h.got("/v/901.m4s") {
		t.Error("sampled a segment past the live edge, which does not exist yet")
	}
	if n := len(h.paths()); n > 3 {
		t.Errorf("%d requests for one poll with segment_sample=1: %v", n, h.paths())
	}
}

// A dynamic manifest whose window has gone empty -- a packager that has
// stopped publishing -- has nothing to sample, and silence would be the wrong
// answer.
func TestDASHEmptyWindowIsReported(t *testing.T) {
	const notStartedMPD = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	     availabilityStartTime="2026-01-01T00:00:00Z">
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	origin, _ := dashOrigin(notStartedMPD, nil)
	defer origin.Close()

	// Two seconds in, no 4s segment has been published yet.
	pr, _ := newTestProber(time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "live", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if !cap.has("no_segments") {
		t.Errorf("an empty availability window produced %+v, want no_segments", cap.findings)
	}
}

// --- format detection ---

func TestDASHIsAutoDetected(t *testing.T) {
	origin, _ := dashOrigin(staticMPD, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	// No "type" on the target: the body decides.
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.URL + "/manifest.mpd"})
	if cap.has("unknown_playlist") {
		t.Errorf("an MPD was not detected: %+v", cap.findings)
	}
}

func TestExplicitTypeOverridesDetection(t *testing.T) {
	origin, _ := dashOrigin(staticMPD, nil)
	defer origin.Close()

	// Declared HLS, served an MPD: the HLS parser recognises nothing.
	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: origin.URL + "/manifest.mpd", Type: "hls",
	})
	if !cap.has("unknown_playlist") {
		t.Errorf("type=hls on an MPD produced %+v, want unknown_playlist", cap.findings)
	}
}

// Declaring the type is what buys the better error: a CDN serving an HTML
// error page with a 200 comes back as a parse failure naming the problem
// rather than a shrug.
func TestDeclaredDASHReportsAParseError(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>404 Not Found</body></html>"))
	}))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "gone", URL: origin.URL + "/manifest.mpd", Type: "dash",
	})
	if !cap.has("manifest_parse") {
		t.Fatalf("got %+v, want manifest_parse", cap.findings)
	}
	for _, f := range cap.findings {
		if f.Check == "manifest_parse" && !strings.Contains(f.Message, "html") {
			t.Errorf("parse finding should name what was served, got %q", f.Message)
		}
	}
}

// --- representation selection ---

const ladderMPD = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT4S">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate media="$RepresentationID$.m4s" timescale="1" duration="4"/>
      <Representation id="v240" bandwidth="400000"/>
      <Representation id="v720" bandwidth="2000000"/>
      <Representation id="v1080" bandwidth="6000000"/>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en">
      <SegmentTemplate media="$RepresentationID$.m4s" timescale="1" duration="4"/>
      <Representation id="a128" bandwidth="128000"/>
    </AdaptationSet>
    <AdaptationSet contentType="text" mimeType="application/mp4" lang="en">
      <SegmentTemplate media="$RepresentationID$.m4s" timescale="1" duration="4"/>
      <Representation id="sub_en" bandwidth="1000"/>
    </AdaptationSet>
  </Period>
</MPD>`

// The cap is per adaptation set precisely so that it cannot silently stop
// probing a whole track: three video rungs capped to one must still leave the
// audio and subtitles covered.
func TestMaxRepresentationsCapsPerAdaptationSet(t *testing.T) {
	origin, h := dashOrigin(ladderMPD, nil)
	defer origin.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: origin.URL + "/manifest.mpd", SegmentSample: 1, MaxRepresentations: 1,
	})

	want := []string{"/a128.m4s", "/manifest.mpd", "/sub_en.m4s", "/v1080.m4s"}
	got := h.paths()
	if len(got) != len(want) {
		t.Fatalf("fetched %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fetched %v, want %v", got, want)
		}
	}
}

func TestRepresentationTypesFilter(t *testing.T) {
	origin, h := dashOrigin(ladderMPD, nil)
	defer origin.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
		RepresentationTypes: []string{"audio"}, MaxRepresentations: 1,
	})

	if !h.got("/a128.m4s") {
		t.Errorf("audio should have been probed: %v", h.paths())
	}
	for _, p := range h.paths() {
		if strings.HasPrefix(p, "/v") || strings.HasPrefix(p, "/sub") {
			t.Errorf("representation_types=[audio] still fetched %s", p)
		}
	}
}

// A DASH target must reach the tracker the same way an HLS one does, so a
// fault re-observed every poll is one incident rather than one per poll.
func TestDASHFindingsFlowThroughTheSamePipeline(t *testing.T) {
	var broken atomic.Bool
	h := &hits{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.add(r.URL.Path)
		switch {
		case r.URL.Path == "/manifest.mpd":
			_, _ = w.Write([]byte(staticMPD))
		case broken.Load() && strings.HasPrefix(r.URL.Path, "/v720/"):
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte("\x00\x00"))
		}
	}))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	target := config.Target{Name: "vod", URL: origin.URL + "/manifest.mpd", SegmentSample: 1}

	broken.Store(true)
	for i := 0; i < 5; i++ {
		pr.ProbeTarget(context.Background(), target)
	}
	// The prober itself does not deduplicate -- the tracker does -- so five
	// polls are five findings. What matters here is that they are identical in
	// target/variant/check, which is the incident key.
	n := 0
	for _, f := range cap.findings {
		if f.Check == "segment_availability" && f.Variant == "video/v720" && f.Target == "vod" {
			n++
		}
	}
	if n != 5 {
		t.Errorf("got %d matching findings across 5 polls, want 5: %+v", n, cap.findings)
	}
}
