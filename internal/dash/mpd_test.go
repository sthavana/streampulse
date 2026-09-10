package dash

import (
	"testing"
	"time"
)

// A live manifest in the shape most packagers emit: dynamic, one period, a
// SegmentTemplate on the adaptation set, and an audio set alongside the video
// ladder.
const liveMPD = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"
     profiles="urn:mpeg:dash:profile:isoff-live:2011"
     type="dynamic"
     availabilityStartTime="2026-09-09T00:00:00Z"
     publishTime="2026-09-09T01:00:00Z"
     minimumUpdatePeriod="PT4S"
     timeShiftBufferDepth="PT1M"
     suggestedPresentationDelay="PT8S"
     minBufferTime="PT4S">
  <BaseURL>https://cdn.example.com/live/</BaseURL>
  <UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.example.com/"/>
  <Period id="p0" start="PT0S">
    <AdaptationSet id="1" contentType="video" mimeType="video/mp4" segmentAlignment="true">
      <SegmentTemplate media="$RepresentationID$/seg-$Number%05d$.m4s"
                       initialization="$RepresentationID$/init.mp4"
                       timescale="1000" duration="4000" startNumber="1"/>
      <Representation id="v720" bandwidth="2000000" width="1280" height="720" codecs="avc1.4d401f"/>
      <Representation id="v360" bandwidth="800000" width="640" height="360" codecs="avc1.4d401e"/>
    </AdaptationSet>
    <AdaptationSet id="2" contentType="audio" mimeType="audio/mp4" lang="en">
      <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
      <SegmentTemplate media="a/$Number$.m4s" initialization="a/init.mp4" timescale="48000" duration="192000"/>
      <Representation id="a128" bandwidth="128000" codecs="mp4a.40.2"/>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseLive(t *testing.T) {
	m, err := Parse([]byte(liveMPD), "https://origin.example.com/ch1/manifest.mpd")
	if err != nil {
		t.Fatalf("Parse errored: %v", err)
	}
	if !m.Dynamic() {
		t.Error("expected a dynamic presentation")
	}
	if got := m.MinimumUpdatePeriod.Or(0); got != 4*time.Second {
		t.Errorf("minimumUpdatePeriod = %v, want 4s", got)
	}
	if got := m.TimeShiftBufferDepth.Or(0); got != time.Minute {
		t.Errorf("timeShiftBufferDepth = %v, want 1m", got)
	}
	want := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	if !m.AvailabilityStartTime.Equal(want) {
		t.Errorf("availabilityStartTime = %v, want %v", m.AvailabilityStartTime, want)
	}
	if len(m.UTCTimings) != 1 || m.UTCTimings[0].Value != "https://time.example.com/" {
		t.Errorf("UTCTiming parse wrong: %+v", m.UTCTimings)
	}
	if len(m.Periods) != 1 || len(m.Periods[0].AdaptationSets) != 2 {
		t.Fatalf("got %d periods / %d sets", len(m.Periods), len(m.Periods[0].AdaptationSets))
	}
	if got := len(m.Representations()); got != 3 {
		t.Errorf("got %d representations, want 3", got)
	}

	v := m.Representations()[0]
	if v.MimeType != "video/mp4" {
		t.Errorf("mimeType should be inherited from the adaptation set, got %q", v.MimeType)
	}
	if v.SegmentTemplate == nil || v.SegmentTemplate.Media == "" {
		t.Fatal("the adaptation set's SegmentTemplate should be inherited")
	}
	if got := v.SegmentTemplate.timescale(); got != 1000 {
		t.Errorf("timescale = %d, want 1000", got)
	}
	// The MPD-level BaseURL is absolute, so it replaces the manifest URL.
	if got := v.BaseURL(); got != "https://cdn.example.com/live/" {
		t.Errorf("BaseURL = %q", got)
	}
	if got := v.InitURI(); got != "https://cdn.example.com/live/v720/init.mp4" {
		t.Errorf("InitURI = %q", got)
	}
	if got := v.Addressing(); got != AddressingNumber {
		t.Errorf("Addressing = %q, want %q", got, AddressingNumber)
	}
}

// A relative BaseURL resolves against the URL the manifest was fetched from,
// which is how most packagers write it.
func TestRelativeBaseURLResolvesAgainstManifest(t *testing.T) {
	raw := `<MPD><BaseURL>v/</BaseURL><Period><AdaptationSet mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" duration="4"/>
	  <Representation id="r0" bandwidth="1"/></AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "https://origin.example.com/a/b/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Representations()[0].BaseURL(); got != "https://origin.example.com/a/b/v/" {
		t.Errorf("BaseURL = %q", got)
	}
}

// BaseURL is inherited and resolved level by level, and the deepest one wins.
func TestBaseURLChain(t *testing.T) {
	raw := `<MPD><BaseURL>https://cdn.example.com/x/</BaseURL>
	 <Period><BaseURL>p1/</BaseURL>
	  <AdaptationSet mimeType="video/mp4"><BaseURL>video/</BaseURL>
	   <SegmentTemplate media="$Number$.m4s" duration="4"/>
	   <Representation id="hi" bandwidth="1"><BaseURL>hi/</BaseURL></Representation>
	   <Representation id="lo" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	reps := m.Representations()
	if got := reps[0].BaseURL(); got != "https://cdn.example.com/x/p1/video/hi/" {
		t.Errorf("representation BaseURL = %q", got)
	}
	if got := reps[1].BaseURL(); got != "https://cdn.example.com/x/p1/video/" {
		t.Errorf("inherited BaseURL = %q", got)
	}
}

// SegmentTemplate inheritance is attribute-wise, not element-wise: a period
// level template supplying @media and @timescale combines with a
// representation level one supplying only @startNumber.
func TestSegmentTemplateMergesAttributeWise(t *testing.T) {
	raw := `<MPD><Period>
	  <SegmentTemplate media="$RepresentationID$-$Number$.m4s" initialization="$RepresentationID$-init.mp4" timescale="90000" duration="360000"/>
	  <AdaptationSet mimeType="video/mp4">
	    <SegmentTemplate duration="180000"/>
	    <Representation id="r0" bandwidth="1"><SegmentTemplate startNumber="42"/></Representation>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	tpl := m.Representations()[0].SegmentTemplate
	if tpl.Media != "$RepresentationID$-$Number$.m4s" {
		t.Errorf("@media should come from the period level, got %q", tpl.Media)
	}
	if got := tpl.timescale(); got != 90000 {
		t.Errorf("@timescale should come from the period level, got %d", got)
	}
	if tpl.Duration == nil || *tpl.Duration != 180000 {
		t.Errorf("the adaptation set's @duration should override the period's, got %v", tpl.Duration)
	}
	if got := tpl.startNumber(); got != 42 {
		t.Errorf("@startNumber should come from the representation, got %d", got)
	}
}

// Merging must not write through to a template that siblings share.
func TestMergeDoesNotMutateSharedTemplate(t *testing.T) {
	raw := `<MPD><Period><AdaptationSet mimeType="video/mp4">
	  <SegmentTemplate media="$Number$.m4s" duration="4" startNumber="1"/>
	  <Representation id="a" bandwidth="1"><SegmentTemplate startNumber="900"/></Representation>
	  <Representation id="b" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	reps := m.Representations()
	if got := reps[0].SegmentTemplate.startNumber(); got != 900 {
		t.Errorf("first representation startNumber = %d, want 900", got)
	}
	if got := reps[1].SegmentTemplate.startNumber(); got != 1 {
		t.Errorf("sibling startNumber = %d, want 1 -- the merge leaked", got)
	}
}

// Multi-period: only some of @start and @duration are written, and the rest
// has to be inferred from the neighbours.
func TestMultiPeriodTimesAreInferred(t *testing.T) {
	raw := `<MPD mediaPresentationDuration="PT30M">
	  <Period id="main1" duration="PT10M"/>
	  <Period id="ad" start="PT10M" duration="PT30S"/>
	  <Period id="main2" start="PT10M30S"/>
	</MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start, dur time.Duration
	}{
		{0, 10 * time.Minute},
		{10 * time.Minute, 30 * time.Second},
		{10*time.Minute + 30*time.Second, 19*time.Minute + 30*time.Second},
	}
	for i, w := range want {
		p := m.Periods[i]
		if p.Start != w.start || p.Duration != w.dur {
			t.Errorf("period %q: start=%v duration=%v, want start=%v duration=%v",
				p.ID, p.Start, p.Duration, w.start, w.dur)
		}
	}
}

// The last period of a live stream has no end, and must not be given a
// fabricated one.
func TestOpenEndedPeriodHasNoDuration(t *testing.T) {
	m, err := Parse([]byte(`<MPD type="dynamic"><Period id="p0" start="PT0S"/></MPD>`), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Periods[0].End(); ok {
		t.Error("an open-ended period should report no end")
	}
}

func TestContentProtection(t *testing.T) {
	raw := `<MPD xmlns:cenc="urn:mpeg:cenc:2013"><Period><AdaptationSet mimeType="video/mp4">
	  <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"
	                     cenc:default_KID="21EC2020-3AEA-4069-A2DD-08002B30309D"/>
	  <ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED">
	    <cenc:pssh>AAAAKXBzc2gAAAAA</cenc:pssh>
	  </ContentProtection>
	  <SegmentTemplate media="$Number$.m4s" duration="4"/>
	  <Representation id="r0" bandwidth="1"/>
	 </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Encrypted() {
		t.Fatal("expected the presentation to report as encrypted")
	}
	cps := m.Periods[0].AdaptationSets[0].ContentProtections
	if len(cps) != 2 {
		t.Fatalf("got %d ContentProtection elements, want 2", len(cps))
	}
	// mp4protection announces the scheme, not a DRM system.
	if got := cps[0].SystemID(); got != "" {
		t.Errorf("mp4protection SystemID = %q, want empty", got)
	}
	if got := cps[0].DefaultKID; got != "21EC2020-3AEA-4069-A2DD-08002B30309D" {
		t.Errorf("default_KID = %q", got)
	}
	if got := cps[1].SystemID(); got != "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed" {
		t.Errorf("SystemID = %q, want the Widevine uuid lowercased", got)
	}
	if b, ok := cps[1].PSSHBytes(); !ok || string(b[4:8]) != "pssh" {
		t.Errorf("PSSHBytes() = %q, %v", b, ok)
	}
	if _, ok := cps[0].PSSHBytes(); ok {
		t.Error("a ContentProtection with no cenc:pssh should report none")
	}
}

func TestLabel(t *testing.T) {
	m, err := Parse([]byte(liveMPD), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"video/v720", "video/v360", "audio/en/a128"}
	for i, r := range m.Representations() {
		if got := r.Label(); got != want[i] {
			t.Errorf("Label() = %q, want %q", got, want[i])
		}
	}
}

// @contentType is optional, and a label of "/r0" would be useless.
func TestContentTypeFallsBackToMimeType(t *testing.T) {
	raw := `<MPD><Period><AdaptationSet><Representation id="r0" mimeType="audio/mp4" bandwidth="1"/></AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Periods[0].AdaptationSets[0].ContentType; got != "audio" {
		t.Errorf("ContentType = %q, want audio", got)
	}
	if got := m.Representations()[0].Label(); got != "audio/r0" {
		t.Errorf("Label() = %q", got)
	}
}

func TestLooksLikeMPD(t *testing.T) {
	if !LooksLikeMPD([]byte(liveMPD)) {
		t.Error("a real MPD should be recognised")
	}
	if !LooksLikeMPD([]byte(`<?xml version="1.0"?><dash:MPD xmlns:dash="urn:mpeg:dash:schema:mpd:2011"/>`)) {
		t.Error("a prefixed root element should be recognised")
	}
	if LooksLikeMPD([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n")) {
		t.Error("an HLS playlist is not an MPD")
	}
	if LooksLikeMPD([]byte("<html><body>404 Not Found</body></html>")) {
		t.Error("an error page is not an MPD")
	}
}

// A CDN serving an HTML error page with a 200 is a real failure mode; it must
// come back as an error rather than an empty presentation.
func TestParseRejectsNonMPD(t *testing.T) {
	for _, raw := range []string{
		`<html><body>Not found</body></html>`,
		`not xml at all`,
		``,
	} {
		if m, err := Parse([]byte(raw), ""); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", raw, m)
		}
	}
}

// Namespace prefixes vary between packagers, and an unknown element should
// simply be skipped rather than derailing the parse.
func TestParseIsNamespaceAndTagTolerant(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
	<dash:MPD xmlns:dash="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT8S">
	  <dash:ProgramInformation><dash:Title>x</dash:Title></dash:ProgramInformation>
	  <dash:Period>
	    <dash:AdaptationSet contentType="video" mimeType="video/mp4">
	      <dash:SegmentTemplate media="$Number$.m4s" duration="4"/>
	      <dash:Representation id="r0" bandwidth="1"/>
	    </dash:AdaptationSet>
	  </dash:Period>
	</dash:MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatalf("Parse errored: %v", err)
	}
	if len(m.Representations()) != 1 {
		t.Fatalf("got %d representations, want 1", len(m.Representations()))
	}
	if m.Dynamic() {
		t.Error("type=static should not be dynamic")
	}
}

// ContentProtection is declared on the adaptation set by almost every
// packager. A consumer asking "is this representation encrypted" must not have
// to walk back up the tree to find out.
func TestContentProtectionIsInherited(t *testing.T) {
	raw := `<MPD xmlns:cenc="urn:mpeg:cenc:2013"><Period><AdaptationSet mimeType="video/mp4">
	  <ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED"/>
	  <SegmentTemplate media="$Number$.m4s" duration="4"/>
	  <Representation id="inherits" bandwidth="1"/>
	  <Representation id="declares" bandwidth="1">
	    <ContentProtection schemeIdUri="urn:uuid:9A04F079-9840-4286-AB92-E65BE0885F95"/>
	  </Representation>
	 </AdaptationSet></Period></MPD>`
	m, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	reps := m.Representations()
	if len(reps[0].ContentProtections) != 1 || reps[0].ContentProtections[0].SystemID() != "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed" {
		t.Errorf("inherited protection wrong: %+v", reps[0].ContentProtections)
	}
	// Its own declaration replaces the set's, so "unprotected" stays meaningful.
	if len(reps[1].ContentProtections) != 1 || reps[1].ContentProtections[0].SystemID() != "9a04f079-9840-4286-ab92-e65be0885f95" {
		t.Errorf("own protection should replace the set's: %+v", reps[1].ContentProtections)
	}
}
