package dash

import (
	"strings"
	"testing"
	"time"
)

var ast = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)

func mustParse(t *testing.T, raw, base string) *MPD {
	t.Helper()
	m, err := Parse([]byte(raw), base)
	if err != nil {
		t.Fatalf("Parse errored: %v", err)
	}
	return m
}

func uris(segs []Segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.URI
	}
	return out
}

// --- $Number$ with @duration ---

func TestNumberSegmentsStatic(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT30S">
	 <BaseURL>https://cdn.example.com/vod/</BaseURL>
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$RepresentationID$/seg-$Number%05d$.m4s" timescale="1000" duration="4000"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())

	// 30s of 4s segments is 8, the last one short. now is irrelevant here.
	if len(segs) != 8 {
		t.Fatalf("got %d segments, want 8", len(segs))
	}
	if got := segs[0].URI; got != "https://cdn.example.com/vod/v0/seg-00001.m4s" {
		t.Errorf("first URI = %q", got)
	}
	if got := segs[7].URI; got != "https://cdn.example.com/vod/v0/seg-00008.m4s" {
		t.Errorf("last URI = %q", got)
	}
	if segs[1].Start != 4*time.Second || segs[1].Duration != 4*time.Second {
		t.Errorf("segment timing wrong: %+v", segs[1])
	}
	if !segs[0].Available.IsZero() {
		t.Error("a static segment has no availability time")
	}
}

// The whole point of the availability window: on a live stream only the last
// timeShiftBufferDepth of segments exist on the CDN, and asking for anything
// past the live edge is a 404 that means nothing.
func TestNumberSegmentsLiveWindow(t *testing.T) {
	raw := `<MPD type="dynamic" availabilityStartTime="2026-09-09T00:00:00Z" timeShiftBufferDepth="PT1M">
	 <BaseURL>https://cdn.example.com/live/</BaseURL>
	 <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	segs := r.SegmentsAt(ast.Add(time.Hour))

	if len(segs) == 0 {
		t.Fatal("expected segments in the live window")
	}
	// One hour in, segment 900 (number = 1 + index 899) ends exactly at the
	// live edge and is the newest fetchable one.
	last := segs[len(segs)-1]
	if last.Number != 900 || last.End() != time.Hour {
		t.Errorf("newest segment = number %d ending %v, want 900 / 1h", last.Number, last.End())
	}
	if got := last.URI; got != "https://cdn.example.com/live/900.m4s" {
		t.Errorf("newest URI = %q", got)
	}
	// Nothing older than the DVR window, nothing beyond the edge.
	if got := segs[0].End(); got < time.Hour-70*time.Second {
		t.Errorf("oldest segment ends at %v, further back than the 1m DVR window", got)
	}
	for _, s := range segs {
		if s.End() > time.Hour {
			t.Fatalf("segment %d ends at %v, past the live edge", s.Number, s.End())
		}
	}
	if got, want := last.Available, ast.Add(time.Hour); !got.Equal(want) {
		t.Errorf("Available = %v, want %v", got, want)
	}
}

// A stream that has just started has no complete segments yet, and must not
// report a negative or wrapped-around range.
func TestNumberSegmentsBeforeFirstSegmentIsEmpty(t *testing.T) {
	raw := `<MPD type="dynamic" availabilityStartTime="2026-09-09T00:00:00Z">
	 <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" duration="4"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	if segs := r.SegmentsAt(ast.Add(2 * time.Second)); len(segs) != 0 {
		t.Errorf("got %d segments 2s into a 4s-segment stream, want none", len(segs))
	}
}

// A long-running live stream with a huge DVR window must not be enumerated
// back to the beginning of time.
func TestNumberSegmentsAreCapped(t *testing.T) {
	raw := `<MPD type="dynamic" availabilityStartTime="2026-09-09T00:00:00Z" timeShiftBufferDepth="P30D">
	 <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" duration="2" startNumber="1"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	segs := r.SegmentsAt(ast.Add(30 * 24 * time.Hour))
	if len(segs) != maxSegments {
		t.Fatalf("got %d segments, want the cap of %d", len(segs), maxSegments)
	}
	// The newest end is what a prober samples, so that is the end to keep.
	if got := segs[len(segs)-1].End(); got != 30*24*time.Hour {
		t.Errorf("capping should keep the newest segments, newest ends at %v", got)
	}
}

// A period that has already closed does not grow with the clock, even in a
// live manifest.
func TestClosedPeriodInLiveMPDStopsAtItsEnd(t *testing.T) {
	raw := `<MPD type="dynamic" availabilityStartTime="2026-09-09T00:00:00Z">
	 <Period id="ad" start="PT0S" duration="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" duration="4"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period>
	 <Period id="main" start="PT12S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="m/$Number$.m4s" duration="4"/>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	m := mustParse(t, raw, "")
	segs := m.Periods[0].AdaptationSets[0].Representations[0].SegmentsAt(ast.Add(time.Hour))
	if len(segs) != 3 {
		t.Fatalf("got %d segments in a 12s period, want 3", len(segs))
	}
}

// --- SegmentTimeline ---

func TestTimelineSegments(t *testing.T) {
	raw := `<MPD type="static" xmlns="urn:mpeg:dash:schema:mpd:2011">
	 <BaseURL>https://cdn.example.com/v/</BaseURL>
	 <Period duration="PT16S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$RepresentationID$/$Time$.m4s" timescale="90000" startNumber="10">
	    <SegmentTimeline>
	      <S t="0" d="180000" r="2"/>
	      <S d="90000"/>
	    </SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())

	// r="2" means two *additional* repeats: three 2s segments, then one 1s.
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4: %v", len(segs), uris(segs))
	}
	want := []string{
		"https://cdn.example.com/v/v0/0.m4s",
		"https://cdn.example.com/v/v0/180000.m4s",
		"https://cdn.example.com/v/v0/360000.m4s",
		"https://cdn.example.com/v/v0/540000.m4s",
	}
	for i, w := range want {
		if segs[i].URI != w {
			t.Errorf("segment %d URI = %q, want %q", i, segs[i].URI, w)
		}
	}
	if segs[0].Number != 10 || segs[3].Number != 13 {
		t.Errorf("numbering = %d..%d, want 10..13", segs[0].Number, segs[3].Number)
	}
	if segs[2].Start != 4*time.Second || segs[2].Duration != 2*time.Second {
		t.Errorf("timing of the third segment wrong: %+v", segs[2])
	}
	if segs[3].Duration != time.Second {
		t.Errorf("last segment duration = %v, want 1s", segs[3].Duration)
	}
}

// @r="-1" means "and so on to the live edge", which is only computable from
// the clock.
func TestTimelineOpenEndedRunStopsAtLiveEdge(t *testing.T) {
	raw := `<MPD type="dynamic" availabilityStartTime="2026-09-09T00:00:00Z">
	 <Period start="PT0S"><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" timescale="1000" startNumber="1">
	    <SegmentTimeline><S t="0" d="4000" r="-1"/></SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	segs := r.SegmentsAt(ast.Add(30 * time.Second))

	// Seven complete 4s segments have been published by 30s; the eighth ends
	// at 32s and does not exist yet.
	if len(segs) != 7 {
		t.Fatalf("got %d segments 30s in, want 7: %v", len(segs), uris(segs))
	}
	if last := segs[6]; last.Number != 7 || last.End() != 28*time.Second {
		t.Errorf("newest = number %d ending %v, want 7 / 28s", last.Number, last.End())
	}
}

// An open-ended run in a period with a declared length stops at the end of the
// period rather than at the clock.
func TestTimelineOpenEndedRunStopsAtPeriodEnd(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT10S">
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" timescale="1">
	    <SegmentTimeline><S t="0" d="2" r="-1"/></SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())
	if len(segs) != 5 {
		t.Fatalf("got %d segments in a 10s period of 2s segments, want 5", len(segs))
	}
}

// A gap in the timeline restarts @t, and @n restarts the numbering with it.
func TestTimelineGapResetsTimeAndNumber(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT20S">
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$-$Time$.m4s" timescale="1" startNumber="1">
	    <SegmentTimeline>
	      <S t="0" d="2"/>
	      <S t="10" d="2" n="100"/>
	      <S d="2"/>
	    </SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())
	if got, want := uris(segs), []string{"1-0.m4s", "100-10.m4s", "101-12.m4s"}; !equal(got, want) {
		t.Errorf("URIs = %v, want %v", got, want)
	}
	if segs[1].Start != 10*time.Second {
		t.Errorf("segment after the gap starts at %v, want 10s", segs[1].Start)
	}
}

// @presentationTimeOffset is the media time of the period start: $Time$ keeps
// the raw tick, but the segment's position in the period is relative to it.
func TestPresentationTimeOffset(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT4S">
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Time$.m4s" timescale="1000" presentationTimeOffset="900000">
	    <SegmentTimeline><S t="900000" d="2000" r="1"/></SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].URI != "900000.m4s" {
		t.Errorf("$Time$ should be the raw tick, got %q", segs[0].URI)
	}
	if segs[0].Start != 0 || segs[1].Start != 2*time.Second {
		t.Errorf("starts should be relative to the period: %v, %v", segs[0].Start, segs[1].Start)
	}
}

// A malformed run with @d="0" would step forever. It must be skipped, and the
// rest of the timeline must still be read.
func TestTimelineZeroDurationRunIsSkipped(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT4S">
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" timescale="1">
	    <SegmentTimeline><S t="0" d="0" r="-1"/><S t="0" d="2" r="1"/></SegmentTimeline>
	  </SegmentTemplate>
	  <Representation id="v0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	segs := mustParse(t, raw, "").Representations()[0].SegmentsAt(time.Now())
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want the 2 from the valid run", len(segs))
	}
}

// --- SegmentList and SegmentBase ---

func TestSegmentList(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT8S">
	 <BaseURL>https://cdn.example.com/v/</BaseURL>
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <Representation id="v0" bandwidth="1">
	    <SegmentList timescale="1000" duration="4000" startNumber="7">
	      <Initialization sourceURL="init.mp4"/>
	      <SegmentURL media="a.m4s"/>
	      <SegmentURL media="b.m4s"/>
	    </SegmentList>
	  </Representation>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	if got := r.Addressing(); got != AddressingList {
		t.Errorf("Addressing = %q", got)
	}
	if got := r.InitURI(); got != "https://cdn.example.com/v/init.mp4" {
		t.Errorf("InitURI = %q", got)
	}
	segs := r.SegmentsAt(time.Now())
	if got, want := uris(segs), []string{"https://cdn.example.com/v/a.m4s", "https://cdn.example.com/v/b.m4s"}; !equal(got, want) {
		t.Errorf("URIs = %v, want %v", got, want)
	}
	if segs[0].Number != 7 || segs[1].Number != 8 {
		t.Errorf("numbering = %d, %d, want 7, 8", segs[0].Number, segs[1].Number)
	}
	if segs[1].Start != 4*time.Second {
		t.Errorf("second segment starts at %v, want 4s", segs[1].Start)
	}
}

// The on-demand profile: one file per representation, indexed internally. We
// cannot enumerate what is inside it without reading the sidx, but the file
// itself is what a reachability check should ask for.
func TestSegmentBaseIsTheFileItself(t *testing.T) {
	raw := `<MPD type="static" mediaPresentationDuration="PT2M">
	 <BaseURL>https://cdn.example.com/vod/</BaseURL>
	 <Period><AdaptationSet contentType="video" mimeType="video/mp4">
	  <Representation id="v0" bandwidth="1">
	    <BaseURL>v0.mp4</BaseURL>
	    <SegmentBase indexRange="0-1234"><Initialization range="0-999"/></SegmentBase>
	  </Representation>
	 </AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	if got := r.Addressing(); got != AddressingBase {
		t.Errorf("Addressing = %q", got)
	}
	if got := r.InitURI(); got != "https://cdn.example.com/vod/v0.mp4" {
		t.Errorf("InitURI = %q, want the representation's own file", got)
	}
	segs := r.SegmentsAt(time.Now())
	if len(segs) != 1 || segs[0].URI != "https://cdn.example.com/vod/v0.mp4" {
		t.Fatalf("SegmentsAt = %v, want the single file", uris(segs))
	}
	if segs[0].Duration != 2*time.Minute {
		t.Errorf("duration = %v, want the period's 2m", segs[0].Duration)
	}
}

// --- template expansion ---

func TestExpandIdentifiers(t *testing.T) {
	r := &Representation{ID: "v0", Bandwidth: 128000}
	cases := map[string]string{
		"$RepresentationID$/$Number$.m4s":  "v0/42.m4s",
		"$Number%05d$.m4s":                 "00042.m4s",
		"$Number%1d$.m4s":                  "42.m4s",
		"$Bandwidth$/$Time$.m4s":           "128000/900.m4s",
		"seg$$5.m4s":                       "seg$5.m4s",
		"no-identifiers.m4s":               "no-identifiers.m4s",
		"$Unknown$/$Number$.m4s":           "$Unknown$/42.m4s",
		"$RepresentationID$/trailing$.m4s": "v0/trailing$.m4s",
	}
	for tmpl, want := range cases {
		if got := r.expand(tmpl, 42, 900); got != want {
			t.Errorf("expand(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

// A format tag we would not want handed to fmt: a bogus verb would put "%!d"
// in the URL, and a huge width would put kilobytes of zeroes there.
func TestExpandIgnoresHostileFormatTags(t *testing.T) {
	r := &Representation{ID: "v0"}
	for _, tmpl := range []string{"$Number%09999999d$.m4s", "$Number%s$.m4s", "$Number%d%d$.m4s", "$Number%$.m4s"} {
		got := r.expand(tmpl, 42, 0)
		if got != "42.m4s" {
			t.Errorf("expand(%q) = %q, want the plain number", tmpl, got)
		}
	}
}

// --- helpers ---

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A representation with nothing to address segments with should say so rather
// than pretend.
func TestNoAddressing(t *testing.T) {
	raw := `<MPD type="static"><Period><AdaptationSet contentType="video" mimeType="video/mp4">
	 <Representation id="v0" bandwidth="1"/></AdaptationSet></Period></MPD>`
	r := mustParse(t, raw, "").Representations()[0]
	if got := r.Addressing(); got != AddressingNone {
		t.Errorf("Addressing = %q, want %q", got, AddressingNone)
	}
	if segs := r.SegmentsAt(time.Now()); segs != nil {
		t.Errorf("SegmentsAt = %v, want nil", uris(segs))
	}
	if got := r.InitURI(); got != "" {
		t.Errorf("InitURI = %q, want empty", got)
	}
}

// Sanity check that the doc example in the README stays true: a 4s-segment
// live stream one hour in reports a window, not the whole hour.
func TestLiveWindowIsNotTheWholePresentation(t *testing.T) {
	m := mustParse(t, liveMPD, "")
	for _, r := range m.Representations() {
		segs := r.SegmentsAt(ast.Add(time.Hour))
		if len(segs) == 0 || len(segs) > 30 {
			t.Errorf("%s: got %d segments in a 1m DVR window", r.Label(), len(segs))
		}
		for _, s := range segs {
			if !strings.HasSuffix(s.URI, ".m4s") {
				t.Errorf("%s: unexpanded URI %q", r.Label(), s.URI)
			}
			if strings.Contains(s.URI, "$") {
				t.Errorf("%s: template left in URI %q", r.Label(), s.URI)
			}
		}
	}
}
