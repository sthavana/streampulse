package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streampulse/internal/config"
	"streampulse/internal/dash"
	"streampulse/internal/metrics"
)

// llMPD is a chunked low-latency manifest: 4s segments published a whole
// segment early, with a 3s latency target. edgeTick is where the timeline's
// last segment ends, in seconds after availabilityStartTime -- for a healthy
// chunked stream that is *ahead* of the clock, because the manifest
// advertises the segment currently being produced.
func llMPD(edgeTick int, opts ...string) string {
	extra := strings.Join(opts, "\n")
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT2S">
	  <ServiceDescription id="0">
	    <Latency target="3000" min="2000" max="6000" referenceId="0"/>
	    <PlaybackRate min="0.96" max="1.04"/>
	  </ServiceDescription>
	  %s
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" initialization="v/init.mp4" timescale="1"
	                     availabilityTimeOffset="4" availabilityTimeComplete="false">
	      <SegmentTimeline><S t="%d" d="4" r="4"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, extra, edgeTick-20)
}

const utcTiming = `<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.example.com/"/>`

// --- parsing ---

func TestLowLatencyParsing(t *testing.T) {
	m, err := dash.Parse([]byte(llMPD(3600, utcTiming)), "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.LowLatency() {
		t.Error("a manifest with a Latency target and chunked template should read as low latency")
	}
	if got, ok := m.Latency(); !ok || got != 3*time.Second {
		t.Errorf("Latency() = %v/%v, want 3s", got, ok)
	}
	if got, ok := m.MaxLatency(); !ok || got != 6*time.Second {
		t.Errorf("MaxLatency() = %v/%v, want 6s", got, ok)
	}

	// A plain live manifest must not be mistaken for one.
	plain, err := dash.Parse([]byte(timelineMPD(3600, 5)), "")
	if err != nil {
		t.Fatal(err)
	}
	if plain.LowLatency() {
		t.Error("an ordinary live manifest should not read as low latency")
	}
}

// Available is earlier than CompleteAt by exactly the offset: that gap is what
// low latency buys, and conflating the two would flatter every measurement.
func TestSegmentCompletionVersusAvailability(t *testing.T) {
	m, err := dash.Parse([]byte(llMPD(liveEdgeTick(4*time.Second), utcTiming)), "")
	if err != nil {
		t.Fatal(err)
	}
	segs := m.Representations()[0].SegmentsAt(dashAST.Add(time.Hour))
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	last := segs[len(segs)-1]
	if gap := last.CompleteAt.Sub(last.Available); gap != 4*time.Second {
		t.Errorf("CompleteAt - Available = %v, want the 4s availabilityTimeOffset", gap)
	}
	// With a full-segment offset the newest segment is still being produced.
	if !last.CompleteAt.After(dashAST.Add(time.Hour)) {
		t.Errorf("newest segment completes at %v, expected it to still be in production", last.CompleteAt)
	}
}

// --- the threshold ---

// A 30s floor on a stream targeting three seconds is not conservative, it is
// blind: the stream would be ten targets behind before anything was said.
func TestLatencyDeclarationTightensTheThreshold(t *testing.T) {
	m, err := dash.Parse([]byte(llMPD(3600, utcTiming)), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := stalenessThreshold(4*time.Second, m); got != 6*time.Second {
		t.Errorf("threshold = %v, want the declared 6s max", got)
	}

	// Target but no max: twice the target, floored at a few segments.
	targetOnly := &dash.MPD{ServiceDescriptions: []dash.ServiceDescription{
		{Latency: &dash.Latency{Target: 10000}},
	}}
	if got := stalenessThreshold(time.Second, targetOnly); got != 20*time.Second {
		t.Errorf("threshold = %v, want twice the 10s target", got)
	}

	// No declaration: the generic floor still applies.
	if got := stalenessThreshold(2*time.Second, &dash.MPD{}); got != 30*time.Second {
		t.Errorf("threshold = %v, want the 30s floor", got)
	}
}

func TestLowLatencyEdgeStaleUsesTheDeclaredBound(t *testing.T) {
	// Timeline 20s behind the clock: fine for a normal stream, four times the
	// declared maximum for this one.
	origin := newMutableOrigin(llMPD(liveEdgeTick(-20*time.Second), utcTiming))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "ll", URL: origin.url()})

	if !cap.has("edge_stale") {
		t.Fatalf("20s behind on a 6s-max stream went undetected: %+v", cap.findings)
	}
	for _, f := range cap.findings {
		if f.Check == "edge_stale" && !strings.Contains(f.Message, "declared max 6.0s") {
			t.Errorf("finding should name the declared bound, got %q", f.Message)
		}
	}
}

func TestLowLatencyHealthyEdgeIsSilent(t *testing.T) {
	origin := newMutableOrigin(llMPD(liveEdgeTick(4*time.Second), utcTiming))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "ll", URL: origin.url()})

	if len(cap.findings) != 0 {
		t.Fatalf("a healthy low-latency stream produced findings: %+v", cap.findings)
	}
}

// --- UTCTiming ---

func TestLowLatencyWithoutUTCTiming(t *testing.T) {
	origin := newMutableOrigin(llMPD(liveEdgeTick(4 * time.Second)))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "ll", URL: origin.url()})

	if !cap.has("utc_timing_missing") {
		t.Errorf("a low-latency stream with no UTCTiming produced %+v", cap.findings)
	}
}

// An ordinary live stream without UTCTiming is not a fault: it has seconds of
// buffer to absorb clock drift.
func TestOrdinaryStreamWithoutUTCTimingIsSilent(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 5))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "plain", URL: origin.url()})

	if cap.has("utc_timing_missing") {
		t.Errorf("UTCTiming is not required of an ordinary stream: %+v", cap.findings)
	}
}

// --- chunked delivery ---

// llOrigin serves the manifest and segments. When buffered is true it answers
// segment requests with a Content-Length, as a proxy that waited for the whole
// segment would.
func llOrigin(mpd string, buffered bool) (*httptest.Server, *int) {
	segmentHits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mpd") {
			_, _ = w.Write([]byte(mpd))
			return
		}
		*segmentHits++
		body := []byte("\x00\x00\x00\x18ftyp")
		if buffered || r.Header.Get("Range") != "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
			return
		}
		// Chunked: no length, flushed as it is produced.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write(body)
			f.Flush()
		}
	}))
	return srv, segmentHits
}

// The quietest way for low latency to fail: everything serves, players play,
// and they play seconds late because something is buffering whole segments.
func TestBufferedDeliveryOnALowLatencyStreamIsReported(t *testing.T) {
	origin, _ := llOrigin(llMPD(liveEdgeTick(4*time.Second), utcTiming), true)
	defer origin.Close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ll", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if !cap.has("chunked_delivery_missing") {
		t.Fatalf("whole-segment buffering went undetected: %+v", cap.findings)
	}
}

func TestChunkedDeliveryIsSilent(t *testing.T) {
	origin, _ := llOrigin(llMPD(liveEdgeTick(4*time.Second), utcTiming), false)
	defer origin.Close()

	reg := metrics.New()
	pr, _ := newTestProber(dashAST.Add(time.Hour))
	pr.reg = reg
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ll", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if len(cap.findings) != 0 {
		t.Fatalf("a properly chunked stream produced findings: %+v", cap.findings)
	}
	if body := scrape(t, reg); !strings.Contains(body, `streampulse_chunked_delivery{target="ll",variant="video/v0"} 1`) {
		t.Error("chunked delivery should be recorded as 1")
	}
}

// Publishing a whole segment slightly early is not chunked delivery, and a
// stream that does it must not be judged by the chunking rule. This isolates
// the low-latency gate: the window here *does* contain a segment still being
// produced, so only the gate can be what keeps the check quiet.
func TestEarlyPublishingWithoutChunkingIsNotJudged(t *testing.T) {
	// availabilityTimeOffset but no availabilityTimeComplete="false", and no
	// ServiceDescription: segments arrive early, whole.
	earlyMPD := fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z">
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" availabilityTimeOffset="4">
	      <SegmentTimeline><S t="%d" d="4" r="4"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, liveEdgeTick(4*time.Second)-20)

	m, err := dash.Parse([]byte(earlyMPD), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.LowLatency() {
		t.Fatal("an offset alone is not chunked delivery")
	}
	segs := m.Representations()[0].SegmentsAt(dashAST.Add(time.Hour))
	if _, ok := inProduction(segs, dashAST.Add(time.Hour)); !ok {
		t.Fatal("fixture should contain a segment still in production, or it proves nothing")
	}

	origin, hits := llOrigin(earlyMPD, true)
	defer origin.Close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "early", URL: origin.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if cap.has("chunked_delivery_missing") {
		t.Errorf("a stream that never claimed low latency was judged by its rule: %+v", cap.findings)
	}
	_ = hits
}

// A finished segment has a length legitimately. Judging one would report every
// healthy low-latency stream as broken.
func TestOnlyAnInProductionSegmentIsJudged(t *testing.T) {
	now := dashAST.Add(time.Hour)
	segs := []dash.Segment{
		{URI: "a", CompleteAt: now.Add(-8 * time.Second)},
		{URI: "b", CompleteAt: now.Add(-4 * time.Second)},
		{URI: "c", CompleteAt: now.Add(2 * time.Second)},
	}
	got, ok := inProduction(segs, now)
	if !ok || got.URI != "c" {
		t.Errorf("inProduction = %+v/%v, want the segment still being produced", got, ok)
	}

	// Everything finished: nothing to judge, and the check must abstain.
	if _, ok := inProduction(segs[:2], now); ok {
		t.Error("a window of finished segments has nothing in production")
	}
	if _, ok := inProduction(nil, now); ok {
		t.Error("an empty window has nothing in production")
	}
}

// The reason the measurement is taken from CompleteAt and not Available,
// pinned at the only latency where the two disagree about the verdict.
//
// This stream is 3s behind its nominal edge, inside its declared 6s maximum,
// and healthy. Measuring from Available would add the 4s offset back on and
// call it 7s -- reporting a stream that is meeting its latency budget as
// failing it, on every single poll.
func TestStalenessDoesNotChargeTheOffsetTwice(t *testing.T) {
	origin := newMutableOrigin(llMPD(liveEdgeTick(-3*time.Second), utcTiming))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "ll", URL: origin.url()})

	if cap.has("edge_stale") {
		t.Errorf("3s behind on a 6s-max stream is within budget: %+v", cap.findings)
	}
}
