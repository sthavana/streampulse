package dash

import (
	"fmt"
	"testing"
	"time"
)

// timelineRep parses a one-representation MPD whose SegmentTimeline is the
// given S elements, at a timescale of 1000 so ticks read as milliseconds.
func timelineRep(t *testing.T, ss string) *Representation {
	t.Helper()
	raw := fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	  availabilityStartTime="2026-01-01T00:00:00Z">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1000" startNumber="1">
	      <SegmentTimeline>%s</SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, ss)
	m, err := Parse([]byte(raw), "http://example.com/m.mpd")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reps := m.Representations()
	if len(reps) != 1 {
		t.Fatalf("representations = %d, want 1", len(reps))
	}
	return reps[0]
}

func TestContinuousTimelineHasNoBreaks(t *testing.T) {
	// Three runs that meet exactly: 0..8000, 8000..12000, 12000..20000.
	r := timelineRep(t, `<S t="0" d="4000" r="1"/><S t="8000" d="4000"/><S t="12000" d="4000" r="1"/>`)
	if got := r.TimelineBreaks(); len(got) != 0 {
		t.Fatalf("breaks = %+v, want none from a continuous timeline", got)
	}
}

// A timeline that restates @t only once is the common form and cannot express
// a hole; it must not be read as one.
func TestSingleRunHasNoBreaks(t *testing.T) {
	r := timelineRep(t, `<S t="900000" d="1920" r="299"/>`)
	if got := r.TimelineBreaks(); len(got) != 0 {
		t.Fatalf("breaks = %+v, want none", got)
	}
}

func TestGapIsFoundWithItsSizeAndPlace(t *testing.T) {
	// 0..8000 published, then nothing until 12000: a 4s hole before segment 3.
	r := timelineRep(t, `<S t="0" d="4000" r="1"/><S t="12000" d="4000"/>`)
	got := r.TimelineBreaks()
	if len(got) != 1 {
		t.Fatalf("breaks = %+v, want exactly one", got)
	}
	b := got[0]
	if !b.Gap() {
		t.Errorf("Gap() = false, want a gap for a positive delta of %v", b.Delta)
	}
	if b.Delta != 4*time.Second {
		t.Errorf("Delta = %v, want 4s", b.Delta)
	}
	if b.At != 8*time.Second {
		t.Errorf("At = %v, want the hole at 8s, where the published run ended", b.At)
	}
	if b.Number != 3 {
		t.Errorf("Number = %d, want the hole in front of segment 3", b.Number)
	}
}

func TestOverlapIsFoundAndIsNotAGap(t *testing.T) {
	// The second run starts 2s before the first one ended.
	r := timelineRep(t, `<S t="0" d="4000" r="1"/><S t="6000" d="4000"/>`)
	got := r.TimelineBreaks()
	if len(got) != 1 {
		t.Fatalf("breaks = %+v, want exactly one", got)
	}
	if got[0].Gap() {
		t.Errorf("Gap() = true for delta %v, want an overlap", got[0].Delta)
	}
	if got[0].Delta != -2*time.Second {
		t.Errorf("Delta = %v, want -2s", got[0].Delta)
	}
}

// An @r of -1 runs until the next @t says otherwise. Every correct live
// manifest written that way would report a break if this were not handled,
// which is the difference between a check and an alarm nobody trusts.
func TestOpenEndedRunIsNeverABreak(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000" r="-1"/><S t="600000" d="4000"/>`)
	if got := r.TimelineBreaks(); len(got) != 0 {
		t.Fatalf("breaks = %+v, want none: the open run ends where the next @t says", got)
	}
}

// Runs that follow an open-ended one cannot be placed -- the open run absorbs
// whatever lies between -- but the timeline is knowable again from the next
// stated @t, and comparisons resume from there.
func TestComparisonResumesAfterAnOpenEndedRun(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000"/><S t="9000" d="4000" r="-1"/>`+
		`<S t="60000" d="4000"/><S t="70000" d="4000"/>`)
	got := r.TimelineBreaks()
	if len(got) != 2 {
		t.Fatalf("breaks = %+v, want the 5s gap before the open run and the 6s one after it", got)
	}
	if got[0].Delta != 5*time.Second {
		t.Errorf("first Delta = %v, want 5s", got[0].Delta)
	}
	// 60000 + 4000 = 64000, and the next run states 70000.
	if got[1].Delta != 6*time.Second {
		t.Errorf("second Delta = %v, want 6s: the timeline is placeable again", got[1].Delta)
	}
}

// Timeline arithmetic is exact, so sub-millisecond disagreement is a packager
// rounding a duration, not a hole any player can land in.
func TestSubMillisecondJitterIsIgnored(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000"/><S t="4002" d="4000"/>`)
	if got := r.TimelineBreaks(); len(got) != 0 {
		t.Fatalf("breaks = %+v, want a 2ms disagreement ignored", got)
	}
}

func TestJustOverTheThresholdIsReported(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000"/><S t="4011" d="4000"/>`)
	if got := r.TimelineBreaks(); len(got) != 1 {
		t.Fatalf("breaks = %+v, want an 11ms hole reported", got)
	}
}

// @n restates the segment number, and the break must be named by the number
// the packager actually uses.
func TestBreakCarriesTheRestatedSegmentNumber(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000" n="500" r="1"/><S t="12000" d="4000"/>`)
	got := r.TimelineBreaks()
	if len(got) != 1 {
		t.Fatalf("breaks = %+v, want one", got)
	}
	if got[0].Number != 502 {
		t.Errorf("Number = %d, want 502", got[0].Number)
	}
}

// A zero-duration run contributes nothing and cannot be stepped through. The
// expander skips it, and the two walks must agree or the segments fetched are
// not the ones being reasoned about.
func TestZeroDurationRunDoesNotShiftTheWalk(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000"/><S d="0" r="4"/><S t="4000" d="4000"/>`)
	if got := r.TimelineBreaks(); len(got) != 0 {
		t.Fatalf("breaks = %+v, want none: an empty run moves nothing", got)
	}
	// Numbering is the part that goes wrong quietly. Five empty repeats that
	// each claimed a segment number would misname every break after them.
	r = timelineRep(t, `<S t="0" d="4000"/><S d="0" r="4"/><S t="12000" d="4000"/>`)
	got := r.TimelineBreaks()
	if len(got) != 1 {
		t.Fatalf("breaks = %+v, want one", got)
	}
	if got[0].Number != 2 {
		t.Errorf("Number = %d, want 2: the empty run consumed segment numbers", got[0].Number)
	}
}

// Every other addressing mode states no timeline, so there is nothing to walk.
func TestNoTimelineNoBreaks(t *testing.T) {
	raw := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"
	  availabilityStartTime="2026-01-01T00:00:00Z">
	  <Period id="p0" start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="v/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "http://example.com/m.mpd")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.Representations()[0].TimelineBreaks(); got != nil {
		t.Fatalf("breaks = %+v, want nil for number addressing", got)
	}
}

// The walk must agree with the expansion about where each run starts. If it
// ever drifts, the tool reasons about segments it does not fetch.
func TestWalkAgreesWithTheExpander(t *testing.T) {
	r := timelineRep(t, `<S t="0" d="4000" r="1"/><S t="12000" d="4000" r="1"/>`)
	segs := r.SegmentsAt(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4", len(segs))
	}
	// The break sits between the runs: after segment 2 ends and before
	// segment 3 starts, which is exactly what the expander produced.
	b := r.TimelineBreaks()
	if len(b) != 1 {
		t.Fatalf("breaks = %+v, want one", b)
	}
	if b[0].At != segs[1].End() {
		t.Errorf("break At = %v, expander ended the run at %v", b[0].At, segs[1].End())
	}
	if b[0].At+b[0].Delta != segs[2].Start {
		t.Errorf("break ends at %v, expander resumed at %v", b[0].At+b[0].Delta, segs[2].Start)
	}
	if b[0].Number != segs[2].Number {
		t.Errorf("break Number = %d, expander numbered it %d", b[0].Number, segs[2].Number)
	}
}
