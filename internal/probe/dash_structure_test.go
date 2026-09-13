package probe

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
)

// structureMPD wraps period bodies in a static presentation. Structure is
// checked without fetching anything, so a VOD manifest is the simplest place
// to state one.
func structureMPD(periods string) string {
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
	   mediaPresentationDuration="PT30S">%s</MPD>`, periods)
}

// videoSet is a well-formed adaptation set: one rung, fully declared.
func videoSet(id, repID string, extra string) string {
	return fmt.Sprintf(`<AdaptationSet id="%s" contentType="video" mimeType="video/mp4"
	    codecs="avc1.4d401f"%s>
	    <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="%s" bandwidth="1000000" width="1280" height="720"/></AdaptationSet>`,
		id, extra, repID)
}

func probeStructure(t *testing.T, body string) *capture {
	t.Helper()
	origin := newMutableOrigin(body)
	defer origin.close()

	pr, _ := newTestProber(dashAST)
	cap := &capture{}
	pr.notifier = cap
	pr.ProbeTarget(context.Background(), config.Target{Name: "vod", URL: origin.url()})
	return cap
}

// A well-formed manifest must produce nothing. This is the test that keeps the
// other seven honest.
func TestWellFormedStructureIsSilent(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` +
		videoSet("v", "v720", `>`[1:]) +
		`<AdaptationSet id="a-en" contentType="audio" mimeType="audio/mp4" lang="en" codecs="mp4a.40.2">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a128" bandwidth="128000"/></AdaptationSet>
		 <AdaptationSet id="a-de" contentType="audio" mimeType="audio/mp4" lang="de" codecs="mp4a.40.2">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a128de" bandwidth="128000"/></AdaptationSet>
		 <AdaptationSet id="t-en" contentType="text" mimeType="application/mp4" lang="en">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="sub-en" bandwidth="1000"/></AdaptationSet></Period>`)

	if cap := probeStructure(t, body); len(cap.findings) != 0 {
		t.Fatalf("a well-formed manifest produced findings: %+v", cap.findings)
	}
}

// Marking the main audio of every language Role=main is ordinary and correct.
// The test above covers it; this states why it is not an ambiguity.
func TestMainRolePerLanguageIsNotAmbiguous(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="a-en" contentType="audio" mimeType="audio/mp4" lang="en" codecs="mp4a.40.2">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a1" bandwidth="128000"/></AdaptationSet>
		 <AdaptationSet id="a-fr" contentType="audio" mimeType="audio/mp4" lang="fr" codecs="mp4a.40.2">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a2" bandwidth="128000"/></AdaptationSet></Period>`)

	if cap := probeStructure(t, body); cap.has("adaptation_set_multiple_main") {
		t.Fatalf("one main track per language was called ambiguous: %+v", cap.findings)
	}
}

// Two sets of the same type and language both claiming to be the main one is
// the fault: nothing in the document says which a player should start with.
func TestTwoMainTracksInOneLanguageAreAmbiguous(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="a-en-aac" contentType="audio" mimeType="audio/mp4" lang="en" codecs="mp4a.40.2">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a1" bandwidth="128000"/></AdaptationSet>
		 <AdaptationSet id="a-en-ec3" contentType="audio" mimeType="audio/mp4" lang="en" codecs="ec-3">
		 <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="a2" bandwidth="448000"/></AdaptationSet></Period>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("adaptation_set_multiple_main")
	if !ok {
		t.Fatalf("two English main tracks went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "audio in en") {
		t.Errorf("message does not say which track is ambiguous: %q", f.Message)
	}
	if !strings.Contains(f.Message, `"a-en-aac"`) || !strings.Contains(f.Message, `"a-en-ec3"`) {
		t.Errorf("message does not name both sets: %q", f.Message)
	}
	// Once for the pair, not once per set.
	var n int
	for _, g := range cap.findings {
		if g.Check == "adaptation_set_multiple_main" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("reported %d times for one ambiguous pair", n)
	}
}

// $RepresentationID$ expands to @id, so two representations sharing one
// resolve to the same segment URLs: one serves the other's media, at the wrong
// bitrate or in the wrong language, with every request succeeding.
func TestDuplicateRepresentationID(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` +
		videoSet("v1", "same", "") + videoSet("v2", "same", "") + `</Period>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("representation_duplicate_id")
	if !ok {
		t.Fatalf("two representations sharing an id went unreported: %+v", cap.findings)
	}
	if f.Severity != alert.Critical {
		t.Errorf("severity = %s, want critical: the two serve each other's media", f.Severity)
	}
	if !strings.Contains(f.Message, `"same"`) {
		t.Errorf("message does not name the id: %q", f.Message)
	}
}

// @id is unique within a period, not across the presentation. Reusing ids in
// the next period is ordinary and must not be reported.
func TestIDsMayRepeatAcrossPeriods(t *testing.T) {
	body := structureMPD(
		`<Period id="p0" start="PT0S" duration="PT15S">` + videoSet("v", "v720", "") + `</Period>` +
			`<Period id="p1" start="PT15S">` + videoSet("v", "v720", "") + `</Period>`)

	if cap := probeStructure(t, body); cap.has("representation_duplicate_id") {
		t.Fatalf("an id reused in the next period was reported: %+v", cap.findings)
	}
}

// A pointer to nothing: the player is offered an enhancement layer whose base
// layer is not in the document.
func TestDanglingDependencyID(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="v-hi" contentType="video" mimeType="video/mp4" codecs="avc1.4d4028">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="v1080" bandwidth="4000000" dependencyId="v-base-missing"/>
		 </AdaptationSet></Period>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("dependency_missing")
	if !ok {
		t.Fatalf("a dependency on a representation that does not exist went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, `"v-base-missing"`) {
		t.Errorf("message does not name what is missing: %q", f.Message)
	}
}

// The mirror: a dependency that resolves, including one pointing into another
// adaptation set, must be silent.
func TestSatisfiedDependencyIsSilent(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="v-hi" contentType="video" mimeType="video/mp4" codecs="avc1.4d4028">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="v1080" bandwidth="4000000" dependencyId="v720"/>
		 </AdaptationSet></Period>`)

	if cap := probeStructure(t, body); cap.has("dependency_missing") {
		t.Fatalf("a dependency that resolves was reported: %+v", cap.findings)
	}
}

// @dependencyId is a whitespace-separated list, and each entry is a separate
// pointer that can dangle on its own.
func TestEachDependencyIsResolvedSeparately(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="v-hi" contentType="video" mimeType="video/mp4" codecs="avc1.4d4028">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="v1080" bandwidth="4000000" dependencyId="v720 v-mid-missing"/>
		 </AdaptationSet></Period>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("dependency_missing")
	if !ok {
		t.Fatalf("the second of two dependencies dangled and went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "v-mid-missing") {
		t.Errorf("message names the wrong dependency: %q", f.Message)
	}
}

// A track with no encodings under it. Nothing else notices: the set is never
// selected, so no segment is ever missing.
func TestEmptyAdaptationSet(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="a-en" contentType="audio" mimeType="audio/mp4" lang="en" codecs="mp4a.40.2"/>
		 </Period>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("adaptation_set_empty")
	if !ok {
		t.Fatalf("an adaptation set with no representations went unreported: %+v", cap.findings)
	}
	if f.Severity != alert.Critical {
		t.Errorf("severity = %s, want critical", f.Severity)
	}
	if !strings.Contains(f.Message, `"a-en"`) {
		t.Errorf("message does not name the set: %q", f.Message)
	}
}

// An SSAI stitcher that opened a period for a break and filled in nothing.
func TestEmptyPeriod(t *testing.T) {
	body := structureMPD(
		`<Period id="content-1" start="PT0S" duration="PT15S">` + videoSet("v", "v720", "") + `</Period>` +
			`<Period id="ad-break-1" start="PT15S"/>`)

	cap := probeStructure(t, body)
	f, ok := cap.find("period_empty")
	if !ok {
		t.Fatalf("a period with no adaptation sets went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "ad-break-1") {
		t.Errorf("message does not name the period: %q", f.Message)
	}
	// An empty period has no sets to walk, so nothing else should be said
	// about it.
	if cap.has("adaptation_set_empty") {
		t.Errorf("an empty period also reported an empty adaptation set: %+v", cap.findings)
	}
}

// A player cannot tell what it is being offered.
func TestRepresentationWithoutMimeType(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">
	    <AdaptationSet id="v" contentType="video" codecs="avc1.4d401f">
	    <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v720" bandwidth="1000000"/></AdaptationSet></Period>`)

	if !probeStructure(t, body).has("representation_missing_mime") {
		t.Fatal("a representation with no mimeType above or on it went unreported")
	}
}

// The set above it declaring one is enough: Parse folds it down, and the
// overwhelming majority of manifests state it there and nowhere else.
func TestMimeTypeInheritedFromTheSetIsEnough(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") + `</Period>`)
	if probeStructure(t, body).has("representation_missing_mime") {
		t.Fatal("a mimeType inherited from the adaptation set was not counted")
	}
}

func TestVideoWithoutCodecs(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">
	    <AdaptationSet id="v" contentType="video" mimeType="video/mp4">
	    <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
	    <Representation id="v720" bandwidth="1000000"/></AdaptationSet></Period>`)

	if !probeStructure(t, body).has("representation_missing_codecs") {
		t.Fatal("a video representation with no codecs went unreported")
	}
}

// A text track carrying TTML declares no codec and is not wrong to: there is
// nothing to be incapable of decoding.
func TestTextWithoutCodecsIsSilent(t *testing.T) {
	body := structureMPD(`<Period id="p0" start="PT0S">` + videoSet("v", "v720", "") +
		`<AdaptationSet id="t-en" contentType="text" mimeType="application/mp4" lang="en">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="sub-en" bandwidth="1000"/></AdaptationSet></Period>`)

	if probeStructure(t, body).has("representation_missing_codecs") {
		t.Fatal("a text track was required to declare a codec")
	}
}

// Structure is a property of the document, so it is checked in every period,
// including the ones the segment walk skips.
func TestStructureIsCheckedInEveryPeriod(t *testing.T) {
	body := structureMPD(
		`<Period id="content-1" start="PT0S" duration="PT15S">` + videoSet("v", "v720", "") + `</Period>` +
			`<Period id="ad-break-1" start="PT15S">` +
			`<AdaptationSet id="ad-v" contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
		 <SegmentTemplate media="$RepresentationID$/$Number$.m4s" timescale="1" duration="4" startNumber="1"/>
		 <Representation id="ad1" bandwidth="1000000" dependencyId="not-here"/>
		 </AdaptationSet></Period>`)

	f, ok := probeStructure(t, body).find("dependency_missing")
	if !ok {
		t.Fatal("a fault in the second period went unreported")
	}
	if !strings.Contains(f.Message, "ad-break-1") {
		t.Errorf("message does not locate the fault in its period: %q", f.Message)
	}
}

// rotatingMPD is a live representation whose declared encryption is whatever
// kid and pssh are passed in, so a test can rotate them between polls.
func rotatingMPD(edgeTick int, kid, pssh string) string {
	body := ""
	if pssh != "" {
		body = "<cenc:pssh>" + pssh + "</cenc:pssh>"
	}
	return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"
	   type="dynamic" availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S">
	  <Period id="p0" start="PT0S">
	    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
	    <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="%s"/>
	    <ContentProtection schemeIdUri="urn:uuid:EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED">%s</ContentProtection>
	    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
	      <SegmentTimeline><S t="%d" d="4" r="4"/></SegmentTimeline>
	    </SegmentTemplate>
	    <Representation id="v0" bandwidth="1"/>
	  </AdaptationSet></Period></MPD>`, kid, body, edgeTick-20)
}

const (
	kidA = "21ec2020-3aea-4069-a2dd-08002b30309d"
	kidB = "31ec2020-3aea-4069-a2dd-08002b30309d"
)

// The point of rotation is to bound what a leaked key is worth, and it fails
// silently: the stream plays, licences are granted, and the window a
// compromised key opens simply stops closing.
func TestDASHKeyRotationStalled(t *testing.T) {
	origin := newMutableOrigin(rotatingMPD(liveEdgeTick(0), kidA, ""))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url(), KeyRotationMaxSec: 60}

	pr.ProbeTarget(context.Background(), target)
	clk.advance(30 * time.Second)
	origin.set(rotatingMPD(liveEdgeTick(30*time.Second), kidA, ""))
	pr.ProbeTarget(context.Background(), target)
	if cap.has("key_rotation_stalled") {
		t.Fatalf("fired after 30s of a 60s budget: %+v", cap.findings)
	}

	clk.advance(45 * time.Second)
	origin.set(rotatingMPD(liveEdgeTick(75*time.Second), kidA, ""))
	pr.ProbeTarget(context.Background(), target)
	f, ok := cap.find("key_rotation_stalled")
	if !ok {
		t.Fatalf("a key unchanged for 75s of a 60s budget went unreported: %+v", cap.findings)
	}
	if !strings.Contains(f.Message, "75.0s") {
		t.Errorf("message does not say how long: %q", f.Message)
	}
}

// The mirror: a packager that keeps rotating must never be reported.
func TestDASHKeyRotatingIsSilent(t *testing.T) {
	origin := newMutableOrigin(rotatingMPD(liveEdgeTick(0), kidA, ""))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url(), KeyRotationMaxSec: 60}

	for i := 0; i < 6; i++ {
		pr.ProbeTarget(context.Background(), target)
		clk.advance(40 * time.Second)
		kid := kidA
		if i%2 == 0 {
			kid = kidB
		}
		origin.set(rotatingMPD(liveEdgeTick(time.Duration(i+1)*40*time.Second), kid, ""))
	}
	if cap.has("key_rotation_stalled") {
		t.Fatalf("a rotating stream was reported: %+v", cap.findings)
	}
}

// Some packagers rotate the initialisation data while leaving default_KID
// alone. Either changing is evidence that rotation is alive.
func TestRotatingPSSHAloneCountsAsRotation(t *testing.T) {
	origin := newMutableOrigin(rotatingMPD(liveEdgeTick(0), kidA, "AAAAKXBzc2gAAAAA"))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url(), KeyRotationMaxSec: 60}

	pr.ProbeTarget(context.Background(), target)
	clk.advance(90 * time.Second)
	origin.set(rotatingMPD(liveEdgeTick(90*time.Second), kidA, "BBBBKXBzc2gAAAAA"))
	pr.ProbeTarget(context.Background(), target)

	if cap.has("key_rotation_stalled") {
		t.Fatalf("a pssh that changed was not counted as rotation: %+v", cap.findings)
	}
}

// Opt-in. DASH keys often rotate in-band, in the pssh of each segment's moof,
// with the MPD never changing -- so without the operator asserting that their
// rotation is visible in the manifest, silence here means nothing either way.
func TestDASHRotationIsNotCheckedWithoutTheKnob(t *testing.T) {
	origin := newMutableOrigin(rotatingMPD(liveEdgeTick(0), kidA, ""))
	defer origin.close()

	pr, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	pr.notifier = cap
	target := config.Target{Name: "live", URL: origin.url()}

	pr.ProbeTarget(context.Background(), target)
	clk.advance(2 * time.Hour)
	origin.set(rotatingMPD(liveEdgeTick(2*time.Hour), kidA, ""))
	pr.ProbeTarget(context.Background(), target)

	if cap.has("key_rotation_stalled") {
		t.Fatalf("rotation was checked without key_rotation_max_seconds: %+v", cap.findings)
	}
}

// Reordering is the packager's business, not a rotation. Comparing in document
// order would report one every time the output was reshuffled.
func TestReorderedProtectionIsNotRotation(t *testing.T) {
	ordered := func(first, second string) string {
		return fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"
		   type="dynamic" availabilityStartTime="2026-01-01T00:00:00Z" minimumUpdatePeriod="PT4S">
		  <Period id="p0" start="PT0S">
		    <AdaptationSet contentType="video" mimeType="video/mp4" codecs="avc1.4d401f">
		    <ContentProtection schemeIdUri="urn:uuid:%s"/>
		    <ContentProtection schemeIdUri="urn:uuid:%s"/>
		    <SegmentTemplate media="v/$Time$.m4s" timescale="1" startNumber="1">
		      <SegmentTimeline><S t="%d" d="4" r="4"/></SegmentTimeline>
		    </SegmentTemplate>
		    <Representation id="v0" bandwidth="1"/>
		  </AdaptationSet></Period></MPD>`, first, second, liveEdgeTick(0)-20)
	}
	const wv = "EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED"
	const pr9 = "9A04F079-9840-4286-AB92-E65BE0885F95"

	origin := newMutableOrigin(ordered(wv, pr9))
	defer origin.close()

	prober, clk := newTestProber(dashAST.Add(time.Hour))
	cap := &capture{}
	prober.notifier = cap
	target := config.Target{Name: "live", URL: origin.url(), KeyRotationMaxSec: 60}

	prober.ProbeTarget(context.Background(), target)
	clk.advance(90 * time.Second)
	origin.set(ordered(pr9, wv)) // same two systems, swapped
	prober.ProbeTarget(context.Background(), target)

	if !cap.has("key_rotation_stalled") {
		t.Fatalf("a reordering was accepted as a rotation: %+v", cap.findings)
	}
}
