package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"streampulse/internal/config"
	"streampulse/internal/dash"
)

var dashAST = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// timelineMPD is a live, timeline-addressed manifest of n 4s segments whose
// first segment starts at firstTick. Advancing firstTick is what a healthy
// packager does between polls.
func timelineMPD(firstTick, n int) string {
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="%d"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, firstTick, n-1)
}

// mutableOrigin serves whatever MPD body it currently holds.
type mutableOrigin struct {
	mu   sync.Mutex
	body string
	srv  *httptest.Server
}

func newMutableOrigin(body string) *mutableOrigin {
	o := &mutableOrigin{body: body}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.mpd" {
			o.mu.Lock()
			b := o.body
			o.mu.Unlock()
			_, _ = w.Write([]byte(b))
			return
		}
		_, _ = w.Write([]byte("\x00\x00"))
	}))
	return o
}

func (o *mutableOrigin) set(body string) {
	o.mu.Lock()
	o.body = body
	o.mu.Unlock()
}

func (o *mutableOrigin) url() string { return o.srv.URL + "/manifest.mpd" }
func (o *mutableOrigin) close()      { o.srv.Close() }

// A packager that stops publishing produces a byte-identical timeline on every
// poll. That is the single most expensive live failure and the reason this
// check exists.
func TestDASHStalledTimeline(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(0, 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	// Threshold is three 4s segments; two polls 5s apart stay under it.
	pr.ProbeTarget(context.Background(), target)
	clk.advance(5 * time.Second)
	pr.ProbeTarget(context.Background(), target)
	if cap.has("playlist_stalled") {
		t.Fatalf("fired after 5s of a 12s threshold: %+v", cap.findings)
	}

	clk.advance(10 * time.Second)
	pr.ProbeTarget(context.Background(), target)
	if !cap.has("playlist_stalled") {
		t.Fatalf("a frozen timeline went undetected after 15s: %+v", cap.findings)
	}
}

// The mirror image: a packager that keeps publishing must never be reported.
func TestDASHAdvancingTimelineIsSilent(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(0, 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	for i := 0; i < 10; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(8 * time.Second)
		origin.set(timelineMPD(4*(i+1), 10)) // the window slides
	}
	if len(cap.findings) != 0 {
		t.Fatalf("a healthy live stream produced findings: %+v", cap.findings)
	}
}

func TestDASHTimelineRollback(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(40, 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	pr.ProbeTarget(context.Background(), target)
	clk.advance(4 * time.Second)
	origin.set(timelineMPD(20, 10)) // origin failover to a stale packager
	pr.ProbeTarget(context.Background(), target)

	if !cap.has("playlist_rollback") {
		t.Fatalf("a backwards timeline went undetected: %+v", cap.findings)
	}
	if cap.has("playlist_stalled") {
		t.Error("a rollback should not also be reported as a freeze")
	}
}

// A static presentation's timeline never advances, and never should be
// reported for it.
func TestStaticMPDNeverStalls(t *testing.T) {
	origin := newMutableOrigin(staticMPD)
	defer origin.close()

	pr, clk := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "vod", URL: origin.url()}

	for i := 0; i < 5; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(time.Minute)
	}
	if cap.has("playlist_stalled") {
		t.Errorf("a VOD manifest was reported as frozen: %+v", cap.findings)
	}
}

// A period that declares its own end is complete: the finished period before
// an ad break. Its timeline is not expected to grow, and reporting it would
// page someone at every ad break on the channel.
func TestClosedPeriodNeverStalls(t *testing.T) {
	multiPeriod := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S">
	  <Period id="ad" start="PT0S" duration="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="ad/$Time$.m4s" timescale="1">
	      <SegmentTimeline><S t="0" d="4" r="2"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="adv0" bandwidth="1"/>
	  </AdaptationSet></Period>
	  <Period id="main" start="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="m/$Time$.m4s" timescale="1">
	      <SegmentTimeline><S t="12" d="4" r="2"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	origin := newMutableOrigin(multiPeriod)
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	// Poll well past the threshold. The closed ad period must stay silent; the
	// open one is genuinely frozen and must not.
	for i := 0; i < 4; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(20 * time.Second)
	}
	var stalled []string
	for _, f := range cap.findings {
		if f.Check == "playlist_stalled" {
			stalled = append(stalled, f.Variant)
		}
	}
	if len(stalled) == 0 {
		t.Fatal("the open period's frozen timeline went undetected")
	}
	for _, v := range stalled {
		if v != "video/v0" {
			t.Errorf("the closed ad period was reported as frozen (variant %q)", v)
		}
	}
}

// A number-addressed live stream has no manifest-declared edge to compare, so
// the freeze check must stay quiet rather than pretend to cover it. The
// coverage comes from the segment sample instead.
func TestNumberAddressedLiveDoesNotFakeAFreezeCheck(t *testing.T) {
	numberMPD := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" timeShiftBufferDepth="PT30S">
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	// The origin serves the manifest but no segment has ever been published.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.mpd" {
			_, _ = w.Write([]byte(numberMPD))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: srv.URL + "/manifest.mpd", SegmentSample: 1}

	for i := 0; i < 4; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(20 * time.Second)
	}
	if cap.has("playlist_stalled") {
		t.Error("the freeze check fired on a clock-derived edge, which it cannot judge")
	}
	if !cap.has("segment_availability") {
		t.Errorf("a dead number-addressed stream produced %+v, want segment_availability", cap.findings)
	}
}

func TestDASHShortWindow(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(0, 3)) // 12s of window
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "live", URL: origin.url(), MinWindowSec: 30,
	})
	if !cap.has("short_window") {
		t.Errorf("a 12s window against a 30s expectation produced %+v", cap.findings)
	}
}

func TestUnexpectedStatic(t *testing.T) {
	origin := newMutableOrigin(staticMPD)
	defer origin.close()

	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "live", URL: origin.url(), ExpectLive: true,
	})
	if !cap.has("unexpected_static") {
		t.Errorf(`type="static" on a target declared live produced %+v`, cap.findings)
	}
}

// Two targets pointing at different manifests must not share edge state, or a
// freeze on one would be masked by the other advancing.
func TestEdgeStateIsPerTargetAndRepresentation(t *testing.T) {
	frozen := newMutableOrigin(timelineMPD(0, 5))
	defer frozen.close()
	moving := newMutableOrigin(timelineMPD(0, 5))
	defer moving.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	a := config.Target{Name: "frozen", URL: frozen.url()}
	b := config.Target{Name: "moving", URL: moving.url()}

	for i := 0; i < 4; i++ {
		pr.ProbeTarget(context.Background(), a)
		pr.ProbeTarget(context.Background(), b)
		clk.advance(20 * time.Second)
		moving.set(timelineMPD(4*(i+1), 5))
	}
	for _, f := range cap.findings {
		if f.Target == "moving" {
			t.Errorf("the healthy target produced %s: %s", f.Check, f.Message)
		}
	}
	if !cap.has("playlist_stalled") {
		t.Error("the frozen target went undetected")
	}
}

// manifestEdge is the decision about *what may be compared across polls*, and
// each of its four refusals matters: comparing a clock-derived edge, or a
// timeline that is complete by design, produces a check that can never fire
// while looking on a dashboard exactly like one that can.
func TestManifestEdge(t *testing.T) {
	const (
		liveTimeline = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
		   availabilityStartTime="2026-01-01T00:00:00Z">
		  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
		      <SegmentTimeline><S t="8" d="4" r="2"/></SegmentTimeline>
		    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		// No mediaPresentationDuration, so the period has no declared end and
		// the closed-period rule cannot be what refuses this one: it is
		// refused for being static.
		staticTimeline = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
		  <Period><AdaptationSet contentType="video" mimeType="video/mp4">
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
		      <SegmentTimeline><S t="0" d="4" r="2"/></SegmentTimeline>
		    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		liveNumber = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
		   availabilityStartTime="2026-01-01T00:00:00Z">
		  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
		    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4"/>
		    <Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		closedPeriod = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
		   availabilityStartTime="2026-01-01T00:00:00Z">
		  <Period start="PT0S" duration="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4">
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
		      <SegmentTimeline><S t="0" d="4" r="2"/></SegmentTimeline>
		    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`
	)

	cases := []struct {
		name    string
		mpd     string
		wantOK  bool
		wantVal int64
		why     string
	}{
		{"live timeline", liveTimeline, true, 16, "the timeline states where the edge is"},
		{"static timeline", staticTimeline, false, 0, "a static presentation's timeline never grows"},
		{"live number-addressed", liveNumber, false, 0, "the edge is derived from our own clock"},
		{"closed period", closedPeriod, false, 0, "a period with a declared end is complete"},
	}
	for _, c := range cases {
		m, err := dashParse(t, c.mpd)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r := m.Representations()[0]
		segs := r.SegmentsAt(dashAST.Add(time.Hour))
		got, ok := manifestEdge(r, segs)
		if ok != c.wantOK {
			t.Errorf("%s: comparable = %v, want %v (%s)", c.name, ok, c.wantOK, c.why)
			continue
		}
		if ok && got != c.wantVal {
			t.Errorf("%s: edge = %d, want %d", c.name, got, c.wantVal)
		}
	}
}

func dashParse(t *testing.T, raw string) (*dash.MPD, error) {
	t.Helper()
	return dash.Parse([]byte(raw), "https://example.com/manifest.mpd")
}

// A manifest that says it only republishes every 30s is supposed to serve an
// identical timeline for 30s at a time. Judging it by its 4s segments would
// report a healthy stream as frozen on almost every poll.
func TestLongMinimumUpdatePeriodRaisesTheStallThreshold(t *testing.T) {
	slow := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT30S">
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
	      <SegmentTimeline><S t="0" d="4" r="9"/></SegmentTimeline>
	    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	origin := newMutableOrigin(slow)
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "slow", URL: origin.url()}

	// 20s of an unchanged manifest: past 3 segments, inside one update period.
	pr.ProbeTarget(context.Background(), target)
	clk.advance(20 * time.Second)
	pr.ProbeTarget(context.Background(), target)
	if cap.has("playlist_stalled") {
		t.Fatalf("fired inside a single 30s update period: %+v", cap.findings)
	}

	// Past three update periods with no change is a real freeze.
	clk.advance(80 * time.Second)
	pr.ProbeTarget(context.Background(), target)
	if !cap.has("playlist_stalled") {
		t.Errorf("a genuinely frozen slow-update stream went undetected: %+v", cap.findings)
	}
}
