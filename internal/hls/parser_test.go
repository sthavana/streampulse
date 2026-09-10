package hls

import "testing"

func TestDetectType(t *testing.T) {
	cases := map[string]PlaylistType{
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n": Master,
		"#EXTM3U\n#EXTINF:4.0,\nseg0.ts\n":                 Media,
		"#EXTM3U\n":                                        Unknown,
	}
	for raw, want := range cases {
		if got := DetectType(raw); got != want {
			t.Errorf("DetectType(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseMaster(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1800000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",FRAME-RATE=25.000,AUDIO="aud"
720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
360p.m3u8
`
	m := ParseMaster(raw)
	if len(m.Variants) != 2 {
		t.Fatalf("got %d variants, want 2", len(m.Variants))
	}
	v := m.Variants[0]
	if v.Bandwidth != 2000000 || v.AvgBandwidth != 1800000 {
		t.Errorf("bandwidth parse wrong: %+v", v)
	}
	if v.Resolution != "1280x720" || v.URI != "720p.m3u8" {
		t.Errorf("resolution/uri parse wrong: %+v", v)
	}
	if v.Codecs != "avc1.4d401f,mp4a.40.2" {
		t.Errorf("codecs comma not preserved: %q", v.Codecs)
	}
	if v.FrameRate != 25.0 {
		t.Errorf("framerate parse wrong: %v", v.FrameRate)
	}
}

func TestParseMedia(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00.000Z
#EXTINF:4.000,
seg100.ts
#EXT-X-DISCONTINUITY
#EXTINF:3.968,segment title here
seg101.ts
#EXT-X-ENDLIST
`
	pl := ParseMedia(raw)
	if pl.Version != 3 || pl.TargetDuration != 4 || pl.MediaSequence != 100 {
		t.Errorf("header parse wrong: %+v", pl)
	}
	if !pl.EndList {
		t.Error("expected EndList true")
	}
	if len(pl.Segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(pl.Segments))
	}
	if pl.Segments[0].ProgramDateTime == nil {
		t.Error("expected PDT on first segment")
	}
	if !pl.Segments[1].Discontinuity {
		t.Error("expected discontinuity flag on second segment")
	}
	if pl.Segments[1].Duration != 3.968 {
		t.Errorf("EXTINF with title parsed wrong: %v", pl.Segments[1].Duration)
	}
	if got := pl.Duration(); got != 7.968 {
		t.Errorf("Duration() = %v, want 7.968", got)
	}
}

func TestParseMasterRenditions(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2",URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="Français",LANGUAGE="fr",DEFAULT=NO,AUTOSELECT=YES,URI="audio/fr.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",FORCED=NO,URI="subs/en.m3u8"
#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID="cc",NAME="CC1",LANGUAGE="en",INSTREAM-ID="CC1"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO="aac",SUBTITLES="subs",CLOSED-CAPTIONS="cc"
720p.m3u8
`
	m := ParseMaster(raw)
	if len(m.Renditions) != 4 {
		t.Fatalf("got %d renditions, want 4", len(m.Renditions))
	}

	en := m.Renditions[0]
	if en.Type != "AUDIO" || en.GroupID != "aac" || en.Name != "English" || en.Language != "en" {
		t.Errorf("audio rendition parsed wrong: %+v", en)
	}
	if en.URI != "audio/en.m3u8" {
		t.Errorf("URI = %q, want audio/en.m3u8", en.URI)
	}
	if !en.Default || !en.AutoSelect {
		t.Errorf("DEFAULT/AUTOSELECT not parsed: %+v", en)
	}
	if en.Channels != "2" {
		t.Errorf("CHANNELS = %q, want 2", en.Channels)
	}
	if m.Renditions[1].Default {
		t.Error("DEFAULT=NO should parse as false")
	}

	// CLOSED-CAPTIONS legitimately carries no URI: it rides in the video.
	cc := m.Renditions[3]
	if cc.URI != "" {
		t.Errorf("closed-captions URI = %q, want empty", cc.URI)
	}
	if cc.InstreamID != "CC1" {
		t.Errorf("INSTREAM-ID = %q, want CC1", cc.InstreamID)
	}

	v := m.Variants[0]
	if v.AudioGroup != "aac" || v.SubtitleGroup != "subs" || v.ClosedCaptions != "cc" {
		t.Errorf("variant group references parsed wrong: %+v", v)
	}
	if v.URI != "720p.m3u8" {
		t.Errorf("EXT-X-MEDIA tags disturbed variant URI pairing: %q", v.URI)
	}
}

// An EXT-X-MEDIA tag between an EXT-X-STREAM-INF and its URI must not be
// mistaken for the variant URI.
func TestRenditionTagDoesNotBreakVariantPairing(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="aac"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",URI="audio/en.m3u8"
360p.m3u8
`
	m := ParseMaster(raw)
	if len(m.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(m.Variants))
	}
	if m.Variants[0].URI != "360p.m3u8" {
		t.Errorf("variant URI = %q, want 360p.m3u8", m.Variants[0].URI)
	}
	if len(m.Renditions) != 1 || m.Renditions[0].URI != "audio/en.m3u8" {
		t.Errorf("rendition parsed wrong: %+v", m.Renditions)
	}
}

func TestRenditionGroups(t *testing.T) {
	m := &MasterPlaylist{Renditions: []Rendition{
		{Type: "AUDIO", GroupID: "aac", Name: "English"},
		{Type: "AUDIO", GroupID: "aac", Name: "French"},
		{Type: "SUBTITLES", GroupID: "subs", Name: "English"},
	}}
	g := m.RenditionGroups()
	if len(g["AUDIO/aac"]) != 2 {
		t.Errorf("AUDIO/aac has %d members, want 2", len(g["AUDIO/aac"]))
	}
	if len(g["SUBTITLES/subs"]) != 1 {
		t.Errorf("SUBTITLES/subs has %d members, want 1", len(g["SUBTITLES/subs"]))
	}
	if len(g["AUDIO/missing"]) != 0 {
		t.Error("an undeclared group should be empty")
	}
}

func TestRenditionLabel(t *testing.T) {
	cases := []struct {
		r    Rendition
		want string
	}{
		{Rendition{Type: "AUDIO", GroupID: "aud1", Name: "English"}, "audio/aud1/English"},
		{Rendition{Type: "SUBTITLES", GroupID: "sub1", Name: "Français"}, "subtitles/sub1/Français"},
		{Rendition{Type: "AUDIO", GroupID: "g", Language: "de"}, "audio/g/de"}, // no NAME
		{Rendition{Type: "AUDIO", GroupID: "aac"}, "audio/aac"},                // no NAME or LANGUAGE
	}
	for _, c := range cases {
		if got := c.r.Label(); got != c.want {
			t.Errorf("Label() = %q, want %q", got, c.want)
		}
	}
}

// Apple's own reference stream names three different audio renditions
// "English", distinguished only by group (stereo aud1, and two 5.1 mixes).
// Their labels must not collide, or three renditions share one metric series
// and one incident key, and a break in one is masked by the others.
func TestRenditionLabelsAreUniqueAcrossGroups(t *testing.T) {
	rs := []Rendition{
		{Type: "AUDIO", GroupID: "aud1", Name: "English", Channels: "2"},
		{Type: "AUDIO", GroupID: "aud2", Name: "English", Channels: "6"},
		{Type: "AUDIO", GroupID: "aud3", Name: "English", Channels: "6"},
	}
	seen := map[string]bool{}
	for _, r := range rs {
		l := r.Label()
		if seen[l] {
			t.Fatalf("label collision on %q", l)
		}
		seen[l] = true
	}
}

// EXT-X-MAP is what an fMP4 player loads before anything else, and nothing
// else in the playlist references it.
func TestParseMediaWithMap(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg0.m4s
#EXTINF:4.000,
seg1.m4s
`
	pl := ParseMedia(raw)
	if len(pl.Maps) != 1 || pl.Maps[0].URI != "init.mp4" {
		t.Fatalf("map parse wrong: %+v", pl.Maps)
	}
	// It applies to every segment that follows it.
	for i, s := range pl.Segments {
		if s.Map == nil || s.Map.URI != "init.mp4" {
			t.Errorf("segment %d has map %+v, want init.mp4", i, s.Map)
		}
	}
}

// A playlist whose initialisation section changes mid-stream, which is what a
// discontinuity between differently-packaged sources looks like.
func TestParseMediaWithChangingMap(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init_a.mp4"
#EXTINF:4.000,
a0.m4s
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="init_b.mp4",BYTERANGE="800@1024"
#EXTINF:4.000,
b0.m4s
#EXTINF:4.000,
b1.m4s
`
	pl := ParseMedia(raw)
	if len(pl.Maps) != 2 {
		t.Fatalf("got %d maps, want 2", len(pl.Maps))
	}
	if pl.Segments[0].Map.URI != "init_a.mp4" {
		t.Errorf("first segment map = %q", pl.Segments[0].Map.URI)
	}
	if pl.Segments[2].Map.URI != "init_b.mp4" {
		t.Errorf("last segment map = %q", pl.Segments[2].Map.URI)
	}
	off, ok := pl.Maps[1].Offset()
	if !ok || off != 1024 {
		t.Errorf("BYTERANGE offset = %d/%v, want 1024/true", off, ok)
	}
}

// A segment playlist with no EXT-X-MAP is the normal MPEG-TS case, not a fault.
func TestParseMediaWithoutMap(t *testing.T) {
	pl := ParseMedia("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n")
	if len(pl.Maps) != 0 || pl.Segments[0].Map != nil {
		t.Errorf("a TS playlist should carry no map, got %+v", pl.Maps)
	}
}

func TestMapOffset(t *testing.T) {
	cases := map[string]struct {
		want int64
		ok   bool
	}{
		"800@1024": {1024, true},
		"800":      {0, false}, // length only: the offset continues the previous section
		"":         {0, false},
		"800@":     {0, false},
		"800@junk": {0, false},
		"800@-1":   {0, false},
	}
	for in, want := range cases {
		got, ok := Map{ByteRange: in}.Offset()
		if got != want.want || ok != want.ok {
			t.Errorf("Offset(%q) = %d/%v, want %d/%v", in, got, ok, want.want, want.ok)
		}
	}
}

// DistinctMaps exists so a 300-segment window does not mean 300 fetches of
// the same initialisation section.
func TestDistinctMaps(t *testing.T) {
	pl := &MediaPlaylist{Maps: []Map{
		{URI: "init.mp4"}, {URI: "init.mp4"}, {URI: "other.mp4"}, {URI: "init.mp4", ByteRange: "10@0"},
	}}
	if got := len(pl.DistinctMaps()); got != 3 {
		t.Errorf("got %d distinct maps, want 3: %+v", got, pl.DistinctMaps())
	}
}
