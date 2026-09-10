package probe

import (
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

var at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func findingsFor(m *hls.MasterPlaylist) []alert.Finding {
	return masterChecks(at, config.Target{Name: "t"}, m)
}

func TestCleanMasterProducesNoFindings(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "720p.m3u8", AudioGroup: "aac", SubtitleGroup: "subs", ClosedCaptions: "NONE"}},
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en.m3u8", Default: true},
			{Type: "AUDIO", GroupID: "aac", Name: "French", URI: "a/fr.m3u8"},
			{Type: "SUBTITLES", GroupID: "subs", Name: "English", URI: "s/en.m3u8"},
		},
	}
	if fs := findingsFor(m); len(fs) != 0 {
		t.Errorf("clean master produced %v", checks(fs))
	}
}

// The headline packaging fault: a variant asks for audio that was never declared.
func TestDanglingAudioGroupIsCritical(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants:   []hls.Variant{{URI: "720p.m3u8", AudioGroup: "aac"}},
		Renditions: []hls.Rendition{{Type: "AUDIO", GroupID: "different", Name: "English", URI: "a/en.m3u8"}},
	}
	fs := findingsFor(m)
	if !hasCheck(fs, "rendition_group_missing") {
		t.Fatalf("expected rendition_group_missing, got %v", checks(fs))
	}
	for _, f := range fs {
		if f.Check == "rendition_group_missing" && f.Severity != alert.Critical {
			t.Errorf("severity = %v, want critical", f.Severity)
		}
	}
}

// Reported once per missing group, not once per variant: a 10-rung ladder
// referencing one absent group is one fault, not ten.
func TestDanglingGroupReportedOncePerGroup(t *testing.T) {
	var variants []hls.Variant
	for i := 0; i < 10; i++ {
		variants = append(variants, hls.Variant{URI: "v.m3u8", AudioGroup: "aac"})
	}
	fs := findingsFor(&hls.MasterPlaylist{Variants: variants})
	if len(fs) != 1 {
		t.Errorf("got %d findings for 10 variants sharing one missing group, want 1: %v", len(fs), checks(fs))
	}
}

func TestDanglingSubtitleAndVideoGroups(t *testing.T) {
	m := &hls.MasterPlaylist{Variants: []hls.Variant{
		{URI: "v.m3u8", SubtitleGroup: "subs", VideoGroup: "angles"},
	}}
	fs := findingsFor(m)
	if len(fs) != 2 {
		t.Fatalf("got %d findings, want 2 (subtitles + video): %v", len(fs), checks(fs))
	}
}

// CLOSED-CAPTIONS=NONE is an enumerated value meaning "no captions here", not
// a group id. Treating it as a dangling reference would fire on healthy streams.
func TestClosedCaptionsNoneIsNotADanglingReference(t *testing.T) {
	for _, v := range []string{"NONE", "none", "None"} {
		m := &hls.MasterPlaylist{Variants: []hls.Variant{{URI: "v.m3u8", ClosedCaptions: v}}}
		if fs := findingsFor(m); len(fs) != 0 {
			t.Errorf("CLOSED-CAPTIONS=%s produced %v", v, checks(fs))
		}
	}
}

func TestMultipleDefaultsInAGroup(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", AudioGroup: "aac"}},
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en.m3u8", Default: true},
			{Type: "AUDIO", GroupID: "aac", Name: "French", URI: "a/fr.m3u8", Default: true},
		},
	}
	if !hasCheck(findingsFor(m), "rendition_multiple_default") {
		t.Errorf("expected rendition_multiple_default, got %v", checks(findingsFor(m)))
	}
}

func TestSingleDefaultIsFine(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", AudioGroup: "aac"}},
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en.m3u8", Default: true},
			{Type: "AUDIO", GroupID: "aac", Name: "French", URI: "a/fr.m3u8"},
		},
	}
	if hasCheck(findingsFor(m), "rendition_multiple_default") {
		t.Error("one DEFAULT=YES per group is correct and must not warn")
	}
}

func TestDuplicateNameWithinGroup(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", AudioGroup: "aac"}},
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en1.m3u8"},
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en2.m3u8"},
		},
	}
	if !hasCheck(findingsFor(m), "rendition_duplicate_name") {
		t.Errorf("expected rendition_duplicate_name, got %v", checks(findingsFor(m)))
	}
}

// The same NAME in two different groups is legal.
func TestSameNameAcrossGroupsIsFine(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", AudioGroup: "aac", SubtitleGroup: "subs"}},
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en.m3u8"},
			{Type: "SUBTITLES", GroupID: "subs", Name: "English", URI: "s/en.m3u8"},
		},
	}
	if hasCheck(findingsFor(m), "rendition_duplicate_name") {
		t.Error("the same NAME in different groups is legal")
	}
}

func TestClosedCaptionsWithURIIsFlagged(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", ClosedCaptions: "cc"}},
		Renditions: []hls.Rendition{
			{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "CC1", InstreamID: "CC1", URI: "cc/en.m3u8"},
		},
	}
	if !hasCheck(findingsFor(m), "closed_captions_with_uri") {
		t.Errorf("expected closed_captions_with_uri, got %v", checks(findingsFor(m)))
	}
}

func TestClosedCaptionsWithoutURIIsCorrect(t *testing.T) {
	m := &hls.MasterPlaylist{
		Variants: []hls.Variant{{URI: "v.m3u8", ClosedCaptions: "cc"}},
		Renditions: []hls.Rendition{
			{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "CC1", InstreamID: "CC1"},
		},
	}
	if fs := findingsFor(m); len(fs) != 0 {
		t.Errorf("captions without a URI are correct, got %v", checks(fs))
	}
}

// --- selectRenditions ---

func renditionSet() *hls.MasterPlaylist {
	return &hls.MasterPlaylist{Renditions: []hls.Rendition{
		{Type: "AUDIO", GroupID: "aac", Name: "English", URI: "a/en.m3u8"},
		{Type: "AUDIO", GroupID: "aac", Name: "French", URI: "a/fr.m3u8"},
		{Type: "SUBTITLES", GroupID: "subs", Name: "English", URI: "s/en.m3u8"},
		{Type: "AUDIO", GroupID: "muxed", Name: "Muxed"},      // no URI
		{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "CC1"}, // no URI
	}}
}

// A rendition with no URI is muxed into the variants: there is nothing to fetch,
// and probing it would mean fetching the master again as if it were a playlist.
func TestSelectSkipsRenditionsWithoutURI(t *testing.T) {
	got := selectRenditions(config.Target{}, renditionSet())
	if len(got) != 3 {
		t.Fatalf("got %d probeable renditions, want 3: %+v", len(got), got)
	}
	for _, r := range got {
		if r.URI == "" {
			t.Errorf("selected a rendition with no URI: %+v", r)
		}
	}
}

func TestSelectFiltersByType(t *testing.T) {
	got := selectRenditions(config.Target{RenditionTypes: []string{"audio"}}, renditionSet())
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 audio renditions: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Type != "AUDIO" {
			t.Errorf("type filter leaked %q", r.Type)
		}
	}
}

func TestSelectRespectsMaxRenditions(t *testing.T) {
	got := selectRenditions(config.Target{MaxRenditions: 2}, renditionSet())
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
	if all := selectRenditions(config.Target{MaxRenditions: 0}, renditionSet()); len(all) != 3 {
		t.Errorf("MaxRenditions=0 should mean all, got %d", len(all))
	}
}

func TestSelectHandlesNoRenditions(t *testing.T) {
	if got := selectRenditions(config.Target{}, &hls.MasterPlaylist{}); len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}
