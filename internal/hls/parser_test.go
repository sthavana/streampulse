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
