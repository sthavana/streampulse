package probe

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
	"streampulse/internal/hls"
)

var dashAST = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// timelineMPD is a live, timeline-addressed manifest of n 4s segments whose
// last segment *ends* at edgeTick seconds after availabilityStartTime.
// Advancing edgeTick in step with the clock is what a healthy packager does.
func timelineMPD(edgeTick, n int) string {
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="%d"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, edgeTick-4*n, n-1)
}

// liveEdgeTick is the tick a healthy packager's edge sits at when the prober's
// clock reads t: the fixtures run one hour after availabilityStartTime.
func liveEdgeTick(elapsed time.Duration) int { return int((time.Hour + elapsed).Seconds()) }

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
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 10))
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
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	for i := 1; i <= 10; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(8 * time.Second)
		// The window slides in step with the clock, as a healthy packager's does.
		origin.set(timelineMPD(liveEdgeTick(time.Duration(i)*8*time.Second), 10))
	}
	if len(cap.findings) != 0 {
		t.Fatalf("a healthy live stream produced findings: %+v", cap.findings)
	}
}

func TestDASHTimelineRollback(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	pr.ProbeTarget(context.Background(), target)
	clk.advance(4 * time.Second)
	origin.set(timelineMPD(liveEdgeTick(0)-20, 10)) // failover to a stale packager
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
	  <Period id="ad" start="PT0S" duration="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="ad/$Time$.m4s" timescale="1">
	      <SegmentTimeline><S t="0" d="4" r="2"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="adv0" bandwidth="1"/>
	  </AdaptationSet></Period>
	  <Period id="main" start="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
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
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
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
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 3)) // 12s of window
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
	frozen := newMutableOrigin(timelineMPD(liveEdgeTick(0), 5))
	defer frozen.close()
	moving := newMutableOrigin(timelineMPD(liveEdgeTick(0), 5))
	defer moving.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	a := config.Target{Name: "frozen", URL: frozen.url()}
	b := config.Target{Name: "moving", URL: moving.url()}

	for i := 1; i <= 4; i++ {
		pr.ProbeTarget(context.Background(), a)
		pr.ProbeTarget(context.Background(), b)
		clk.advance(20 * time.Second)
		moving.set(timelineMPD(liveEdgeTick(time.Duration(i)*20*time.Second), 5))
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
		  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
		      <SegmentTimeline><S t="8" d="4" r="2"/></SegmentTimeline>
		    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		// No mediaPresentationDuration, so the period has no declared end and
		// the closed-period rule cannot be what refuses this one: it is
		// refused for being static.
		staticTimeline = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
		  <Period><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
		      <SegmentTimeline><S t="0" d="4" r="2"/></SegmentTimeline>
		    </SegmentTemplate><Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		liveNumber = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
		   availabilityStartTime="2026-01-01T00:00:00Z">
		  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
		    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4"/>
		    <Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`

		closedPeriod = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
		   availabilityStartTime="2026-01-01T00:00:00Z">
		  <Period start="PT0S" duration="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
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
	  <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1">
	      <SegmentTimeline><S t="3560" d="4" r="9"/></SegmentTimeline>
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

// An encoder that is falling behind but still publishing is a different fault
// from a frozen one: the timeline advances every poll, so the freeze check is
// correctly silent, and only the comparison against wall clock catches it.
func TestDASHEdgeStaleWhileStillAdvancing(t *testing.T) {
	origin := newMutableOrigin(timelineMPD(liveEdgeTick(0), 10))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "lagging", URL: origin.url()}
	polls := 1

	// Publishing 4s of media for every 8s of wall clock: always moving, never
	// catching up. The lag grows by 4s per poll.
	poll := func(n int) {
		for i := 0; i < n; i++ {
			pr.ProbeTarget(context.Background(), target)
			clk.advance(8 * time.Second)
			origin.set(timelineMPD(liveEdgeTick(time.Duration(polls)*4*time.Second), 10))
			polls++
		}
	}

	// A few seconds behind is what every healthy packager looks like.
	poll(4)
	if cap.has("edge_stale") {
		t.Fatalf("fired at ~16s of lag, which is normal publishing delay: %+v", cap.findings)
	}

	poll(12)
	if !cap.has("edge_stale") {
		t.Fatalf("a packager falling behind wall clock went undetected: %+v", cap.findings)
	}
	if cap.has("playlist_stalled") {
		t.Error("the timeline was advancing every poll; this is not a freeze")
	}
}

// The DRM half. An MPD carries no key URI, so what is checkable is what it
// asserts: that the content is protected, and that its init data is well formed.
func TestDASHDRMChecks(t *testing.T) {
	mpdWith := func(protection string) string {
		return `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"
		   type="static" mediaPresentationDuration="PT8S">
		  <Period><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">` + protection + `
		    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4"/>
		    <Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`
	}

	cases := []struct {
		name       string
		protection string
		expect     bool
		want       string
	}{
		{"clear stream declared encrypted", ``, true, "unexpected_clear_segments"},
		{"protected stream", `<ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED"/>`, true, ""},
		{"bad default_KID", `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="$KID$"/>`, false, "kid_invalid"},
		{"undashed default_KID is accepted", `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" cenc:default_KID="21ec20203aea4069a2dd08002b30309d"/>`, false, ""},
		{"pssh not base64", `<ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED"><cenc:pssh>not!base64!</cenc:pssh></ContentProtection>`, false, "pssh_malformed"},
		{"pssh truncated", `<ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED"><cenc:pssh>AAAAKXBzc2gAAAAA</cenc:pssh></ContentProtection>`, false, "pssh_malformed"},
		// A bare PlayReady Object is not a PSSH box and must not be flagged.
		{"playready object", `<ContentProtection schemeIdUri="urn:uuid:9A04F079-9840-4286-AB92-E65BE0885F95"><cenc:pssh>PFdSTUhFQURFUj48REFUQS8+PC9XUk1IRUFERVI+</cenc:pssh></ContentProtection>`, false, ""},
	}

	for _, c := range cases {
		origin := newMutableOrigin(mpdWith(c.protection))
		pr, _ := newTestProber(dashAST)
		cap := &capture{}
		pr.notifier = cap
		pr.ProbeTarget(context.Background(), config.Target{
			Name: "vod", URL: origin.url(), ExpectEncrypted: c.expect,
		})
		origin.close()

		switch {
		case c.want == "" && len(cap.findings) > 0:
			t.Errorf("%s: expected silence, got %+v", c.name, cap.findings)
		case c.want != "" && !cap.has(c.want):
			t.Errorf("%s: expected %s, got %+v", c.name, c.want, cap.findings)
		}
	}
}

// A valid PSSH box must be silent, and its DRM system named the same way the
// HLS path names it.
func TestValidPSSHIsSilentAndNamed(t *testing.T) {
	box := psshBox(hls.SystemWidevine, []byte("init-data"))
	raw := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"
	   type="static" mediaPresentationDuration="PT4S">
	  <Period><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED">
	      <cenc:pssh>` + base64.StdEncoding.EncodeToString(box) + `</cenc:pssh>
	    </ContentProtection>
	    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4"/>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`

	origin := newMutableOrigin(raw)
	defer origin.close()
	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.url(), ExpectEncrypted: true})

	if len(cap.findings) != 0 {
		t.Fatalf("a valid Widevine PSSH produced %+v", cap.findings)
	}
	empty := psshBox(hls.SystemWidevine, nil)
	origin.set(strings.Replace(raw, base64.StdEncoding.EncodeToString(box), base64.StdEncoding.EncodeToString(empty), 1))
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.url(), ExpectEncrypted: true})
	if !cap.has("pssh_empty") {
		t.Errorf("a PSSH carrying nothing produced %+v", cap.findings)
	}
}

// Multiple periods are DASH's splice points, reported the same way HLS
// discontinuities are.
func TestPeriodBoundariesAreReported(t *testing.T) {
	raw := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT16S">
	  <Period id="a" duration="PT8S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="a/$Number$.m4s" timescale="1" duration="4"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period>
	  <Period id="b" duration="PT8S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="b/$Number$.m4s" timescale="1" duration="4"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period></MPD>`

	origin := newMutableOrigin(raw)
	defer origin.close()
	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.url()})

	if !cap.has("discontinuity_present") {
		t.Errorf("a two-period presentation produced %+v", cap.findings)
	}
}

// A missing init segment stops playback dead while every other check passes:
// the manifest parses and every media segment is served.
func TestInitSegmentUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/manifest.mpd":
			_, _ = w.Write([]byte(staticMPD))
		case strings.HasSuffix(r.URL.Path, "/init.mp4"):
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte("\x00\x00"))
		}
	}))
	defer srv.Close()

	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "vod", URL: srv.URL + "/manifest.mpd", SegmentSample: 1,
	})

	if !cap.has("init_segment_availability") {
		t.Fatalf("a dead init segment went undetected: %+v", cap.findings)
	}
	if cap.has("segment_availability") {
		t.Error("media segments were fine; only the init segment was not")
	}
}

// The floor matters more than the scaling term, and only bites on short
// segments -- which is where the real false positive came from: Unified
// Streaming's 1.92s segments run about 6s behind wall clock in perfect health,
// and a threshold derived from segment duration alone reported it every poll.
func TestStalenessThreshold(t *testing.T) {
	dur := func(d time.Duration) dash.Duration { return dash.Duration{Value: d, Set: true} }

	cases := []struct {
		name    string
		segment time.Duration
		spd     dash.Duration
		want    time.Duration
	}{
		{"short segments use the floor", 1920 * time.Millisecond, dash.Duration{}, 30 * time.Second},
		{"long segments scale past it", 10 * time.Second, dash.Duration{}, 40 * time.Second},
		{"a declared presentation delay is believed", 2 * time.Second, dur(60 * time.Second), 68 * time.Second},
		{"a small declared delay does not lower the floor", 2 * time.Second, dur(time.Second), 30 * time.Second},
	}
	for _, c := range cases {
		m := &dash.MPD{SuggestedPresentationDelay: c.spd}
		if got := stalenessThreshold(c.segment, m); got != c.want {
			t.Errorf("%s: threshold = %v, want %v", c.name, got, c.want)
		}
	}
}

// find returns the first finding for a check, so a test can assert on what it
// says rather than only that it fired.
func (c *capture) find(check string) (alert.Finding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.findings {
		if f.Check == check {
			return f, true
		}
	}
	return alert.Finding{}, false
}

// gappyMPD is a live timeline whose two runs do not meet: `hole` seconds of
// presentation are missing between them. Each run is n 4s segments and the
// last one ends at edgeTick.
func gappyMPD(edgeTick, n, hole int) string {
	first := edgeTick - 4*n - hole - 4*n
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S"
	   timeShiftBufferDepth="PT1H">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="%d"/><S t="%d" d="4" r="%d"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, first, n-1, first+4*n+hole, n-1)
}

// A hole in the timeline is a hole in playback, and no other check can see it:
// every segment listed fetches, the manifest parses, the edge advances.
func TestTimelineGapIsReported(t *testing.T) {
	origin := newMutableOrigin(gappyMPD(liveEdgeTick(0), 5, 12))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	f, ok := cap.find("timeline_gap")
	if !ok {
		t.Fatalf("a 12s hole in the timeline went unreported: %+v", cap.findings)
	}
	if f.Severity != alert.Critical {
		t.Errorf("severity = %s, want critical for a hole of three whole segments", f.Severity)
	}
	if !strings.Contains(f.Message, "12.0s") {
		t.Errorf("message does not say how big the hole is: %q", f.Message)
	}
}

// The mirror image, and the one that matters more: a continuous timeline must
// never be reported, or the check is an alarm nobody trusts.
func TestContinuousTimelineIsSilent(t *testing.T) {
	origin := newMutableOrigin(gappyMPD(liveEdgeTick(0), 5, 0))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("timeline_gap") || cap.has("timeline_overlap") {
		t.Fatalf("two runs that meet exactly were reported as broken: %+v", cap.findings)
	}
}

// A break smaller than a segment is a defect worth knowing about; one at least
// a segment long is a stall a viewer sees.
func TestTimelineGapSeverityIsGradedAgainstTheSegment(t *testing.T) {
	origin := newMutableOrigin(gappyMPD(liveEdgeTick(0), 5, 2))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	f, ok := cap.find("timeline_gap")
	if !ok {
		t.Fatalf("a 2s hole went unreported: %+v", cap.findings)
	}
	if f.Severity != alert.Warning {
		t.Errorf("severity = %s, want warning for half a 4s segment", f.Severity)
	}
}

// A break that has scrolled out of the DVR is history. Reporting it every poll
// until the packager trims it would say a stream is broken long after the hole
// stopped being reachable.
func TestBreaksOutsideTheWindowAreNotReported(t *testing.T) {
	// The hole sits an hour back, far outside a 60s timeShiftBufferDepth.
	body := fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S"
	   timeShiftBufferDepth="PT60S">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="0" d="4" r="2"/><S t="1000" d="4" r="%d"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, (liveEdgeTick(0)-1000)/4-1)
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("timeline_gap") {
		t.Fatalf("a hole an hour outside the DVR window was reported: %+v", cap.findings)
	}
}

// dvrMPD declares a DVR depth and offers a window of n 4s segments, which is
// how a packager that has just restarted looks: healthy at the edge, with
// nothing behind it.
func dvrMPD(edgeTick, n int, depth string) string {
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S"
	   timeShiftBufferDepth="%s">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="%d"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, depth, edgeTick-4*n, n-1)
}

// The promise is in the document, so this needs no configuration: seek back
// this far, the manifest says, and the segments will be there.
func TestWindowBelowDeclaredDepth(t *testing.T) {
	origin := newMutableOrigin(dvrMPD(liveEdgeTick(0), 5, "PT10M")) // 20s of a promised 600s
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	f, ok := cap.find("window_below_declared")
	if !ok {
		t.Fatalf("20s of a promised 600s DVR went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "600.0s") || !strings.Contains(f.Message, "20.0s") {
		t.Errorf("message does not compare the promise with the reality: %q", f.Message)
	}
}

// A live window is legitimately a little short of the promise: the oldest
// segment expires while the newest is still being produced.
func TestWindowSlightlyShortOfTheDepthIsSilent(t *testing.T) {
	origin := newMutableOrigin(dvrMPD(liveEdgeTick(0), 29, "PT2M")) // 116s of a promised 120s
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("window_below_declared") {
		t.Fatalf("116s of a promised 120s was reported: %+v", cap.findings)
	}
}

// A static presentation has no DVR window to fall short of.
func TestDeclaredDepthIsNotCheckedOnStatic(t *testing.T) {
	body := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
	   timeShiftBufferDepth="PT10M" mediaPresentationDuration="PT20S">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="0" d="4" r="4"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("window_below_declared") {
		t.Fatalf("a static presentation was held to a DVR promise: %+v", cap.findings)
	}
}

// Players size their buffers from @maxSegmentDuration, so a segment longer
// than it is a rebuffer on a stream whose every request succeeded.
func TestSegmentLongerThanDeclaredMaximum(t *testing.T) {
	body := fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S"
	   maxSegmentDuration="PT4S" timeShiftBufferDepth="PT1H">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="2"/><S d="11"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, liveEdgeTick(0)-23)
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	f, ok := cap.find("segment_duration_violation")
	if !ok {
		t.Fatalf("an 11s segment under a 4s declared maximum went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "11.0s") || !strings.Contains(f.Message, "4.0s") {
		t.Errorf("message does not give both durations: %q", f.Message)
	}
}

// Segments at exactly the declared maximum are what a correct packager emits,
// and every poll of every healthy stream would report one if this were wrong.
func TestSegmentAtTheDeclaredMaximumIsSilent(t *testing.T) {
	origin := newMutableOrigin(strings.Replace(
		dvrMPD(liveEdgeTick(0), 5, "PT1H"), `minimumUpdatePeriod="PT4S"`,
		`minimumUpdatePeriod="PT4S" maxSegmentDuration="PT4S"`, 1))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("segment_duration_violation") {
		t.Fatalf("4s segments under a 4s declared maximum were reported: %+v", cap.findings)
	}
}

// periodsMPD is two periods, the first declaring both its start and its
// length, which is the form a server-side ad stitcher writes.
func periodsMPD(firstDuration, secondStart string) string {
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
	   mediaPresentationDuration="PT1H">
	  <Period id="content-1" start="PT0S" duration="%s">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="a/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period>
	  <Period id="ad-break-1" start="%s">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="b/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period></MPD>`,
		firstDuration, secondStart)
}

func probePeriods(t *testing.T, body string) *capture {
	t.Helper()
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.url()})
	return cap
}

// Where server-side ad insertion goes wrong: the stitcher's arithmetic leaves
// a hole, and players stall at the boundary -- which viewers experience as the
// stream dying exactly when the ad starts.
func TestPeriodGapIsReported(t *testing.T) {
	cap := probePeriods(t, periodsMPD("PT30S", "PT32S"))

	f, ok := cap.find("period_gap")
	if !ok {
		t.Fatalf("a 2s hole at a period boundary went unreported: %+v", cap.findings)
	}
	if f.Severity != alert.Critical {
		t.Errorf("severity = %s, want critical for a 2s hole", f.Severity)
	}
	if !strings.Contains(f.Message, "content-1") || !strings.Contains(f.Message, "ad-break-1") {
		t.Errorf("message does not name the boundary: %q", f.Message)
	}
}

func TestPeriodOverlapIsReported(t *testing.T) {
	cap := probePeriods(t, periodsMPD("PT30S", "PT28S"))

	f, ok := cap.find("period_overlap")
	if !ok {
		t.Fatalf("a 2s overlap at a period boundary went unreported: %+v", cap.findings)
	}
	if cap.has("period_gap") {
		t.Errorf("an overlap was also reported as a gap: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "2.0s") {
		t.Errorf("message does not say how big the overlap is: %q", f.Message)
	}
}

// Periods that meet exactly are the overwhelming majority, and reporting them
// would make the check worthless.
func TestAdjacentPeriodsAreSilent(t *testing.T) {
	if cap := probePeriods(t, periodsMPD("PT30S", "PT30S")); cap.has("period_gap") || cap.has("period_overlap") {
		t.Fatalf("periods that meet exactly were reported: %+v", cap.findings)
	}
}

// A frame at 24fps is 41ms. Below that there is no frame for a player to miss,
// and the number is a stitcher rounding its arithmetic.
func TestSubFramePeriodDisagreementIsSilent(t *testing.T) {
	if cap := probePeriods(t, periodsMPD("PT30S", "PT30.02S")); cap.has("period_gap") {
		t.Fatalf("a 20ms boundary disagreement was reported: %+v", cap.findings)
	}
}

// Where @start is absent the parser derives it from the previous period's
// duration. Comparing that against the duration it came from is comparing our
// own arithmetic to itself: it can only ever agree, which would look like
// coverage while checking nothing.
func TestDerivedPeriodTimesAreNotCompared(t *testing.T) {
	body := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
	   mediaPresentationDuration="PT1H">
	  <Period id="content-1" start="PT0S" duration="PT30S">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="a/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period>
	  <Period id="ad-break-1">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="b/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period></MPD>`
	if cap := probePeriods(t, body); cap.has("period_gap") || cap.has("period_overlap") {
		t.Fatalf("derived period times were compared: %+v", cap.findings)
	}
}

// A period without an @id is named by its position, because a finding that
// cannot say where the fault is does not help anyone.
func TestUnnamedPeriodsAreNamedByPosition(t *testing.T) {
	body := strings.ReplaceAll(periodsMPD("PT30S", "PT40S"), ` id="content-1"`, "")
	body = strings.ReplaceAll(body, ` id="ad-break-1"`, "")

	f, ok := probePeriods(t, body).find("period_gap")
	if !ok {
		t.Fatal("a 10s hole between unnamed periods went unreported")
	}
	if !strings.Contains(f.Message, "#1") || !strings.Contains(f.Message, "#2") {
		t.Errorf("message does not locate the boundary: %q", f.Message)
	}
}

// On a short DVR the proportional allowance is smaller than the churn at the
// two ends of the window: a quarter of 20s is 5s, which is barely one 4s
// segment. A stream promising 20s and offering 12s is two segments short, and
// two segments is what the oldest expiring and the newest still being produced
// costs.
func TestShortDVRAllowanceIsAtLeastTwoSegments(t *testing.T) {
	origin := newMutableOrigin(dvrMPD(liveEdgeTick(0), 3, "PT20S")) // 12s of a promised 20s
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("window_below_declared") {
		t.Fatalf("12s of a promised 20s -- two segments of churn -- was reported: %+v", cap.findings)
	}
}

// The same allowance must not swallow a genuinely collapsed window.
func TestShortDVRStillCatchesACollapse(t *testing.T) {
	origin := newMutableOrigin(dvrMPD(liveEdgeTick(0), 1, "PT20S")) // 4s of a promised 20s
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if !cap.has("window_below_declared") {
		t.Fatalf("4s of a promised 20s went unreported: %+v", cap.findings)
	}
}

// The mirror of the floor: on a long DVR the allowance is proportional, since
// two segments of a ten-minute window is no allowance at all. Eight minutes of
// a promised ten is churn, not a collapse.
func TestLongDVRAllowanceIsProportional(t *testing.T) {
	origin := newMutableOrigin(dvrMPD(liveEdgeTick(0), 120, "PT10M")) // 480s of a promised 600s
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if cap.has("window_below_declared") {
		t.Fatalf("480s of a promised 600s was reported: %+v", cap.findings)
	}
}

// multiPeriodLiveMPD is a live presentation whose DVR spans a period boundary:
// a completed 30s period and a live one, together the 60s the manifest
// promises. This is every stream that has just crossed an ad break.
func multiPeriodLiveMPD() string {
	seg := `<SegmentTemplate media="$RepresentationID$/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="0" d="6" r="4"/></SegmentTimeline></SegmentTemplate>`
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT6S"
	   timeShiftBufferDepth="PT60S">
	  <Period id="content-1" start="PT3540S" duration="PT30S">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">%s
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period>
	  <Period id="ad-break-1" start="PT3570S">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">%s
	    <Representation id="v0" bandwidth="1"/></AdaptationSet></Period></MPD>`, seg, seg)
}

// The promise is made by the presentation, not by whichever period holds the
// live edge. Measuring one period against it reported every multi-period live
// stream as broken, which is how this was found -- against livesim2, not in a
// test.
func TestDVRSpanningPeriodsIsNotShort(t *testing.T) {
	origin := newMutableOrigin(multiPeriodLiveMPD())
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if f, ok := cap.find("window_below_declared"); ok {
		t.Fatalf("two 30s periods covering the promised 60s were reported: %q", f.Message)
	}
}

// One finding for a presentation-wide promise, not one per representation.
func TestDVRShortfallIsReportedOncePerTarget(t *testing.T) {
	origin := newMutableOrigin(strings.ReplaceAll(
		multiPeriodLiveMPD(), `timeShiftBufferDepth="PT60S"`, `timeShiftBufferDepth="PT10M"`))
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	var n int
	for _, f := range cap.findings {
		if f.Check == "window_below_declared" {
			n++
			if f.Variant != "" {
				t.Errorf("finding is labelled with variant %q, but the promise is the presentation's", f.Variant)
			}
		}
	}
	if n != 1 {
		t.Errorf("window_below_declared fired %d times, want once: %+v", n, cap.findings)
	}
}

// When nothing is available at all, no_segments is the finding that matters.
// Adding "the DVR is 0s short of its promise" on top of it says nothing new
// and buries the one that does.
func TestNoSegmentsIsNotAlsoAShortWindow(t *testing.T) {
	// A timeline that starts an hour after the live edge: nothing is fetchable.
	body := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	   availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S"
	   timeShiftBufferDepth="PT60S">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="7200" d="4" r="4"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "live", URL: origin.url()})

	if !cap.has("no_segments") {
		t.Fatalf("nothing was available and no_segments did not fire: %+v", cap.findings)
	}
	if f, ok := cap.find("window_below_declared"); ok {
		t.Errorf("an empty window was also reported as a short DVR: %q", f.Message)
	}
}
