package inspect

import "testing"

func kinds(ds []Diff) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Kind
	}
	return out
}

func TestFamilyMapsRFC6381ToFFprobeNames(t *testing.T) {
	cases := map[string]string{
		"avc1.4d401f":      "h264",
		"avc3.640028":      "h264",
		"hvc1.2.4.L153.B0": "hevc",
		"hev1.1.6.L93.B0":  "hevc",
		"dvh1.05.06":       "hevc",
		"vp09.00.10.08":    "vp9",
		"av01.0.04M.08":    "av1",
		"mp4a.40.2":        "aac",
		"mp4a.40.5":        "aac",
		// The same four-character code carries three different formats.
		"mp4a.a5": "ac3",
		"mp4a.a6": "eac3",
		"ac-3":    "ac3",
		"ec-3":    "eac3",
		// Unknown codes are ignored rather than guessed at: new codecs appear
		// faster than this table is updated.
		"xyzw.1": "",
		"":       "",
		"stpp":   "",
	}
	for in, want := range cases {
		if got := family(in); got != want {
			t.Errorf("family(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareCleanStream(t *testing.T) {
	m := &Media{Streams: []Stream{
		{CodecType: "video", CodecName: "h264", Width: 1280, Height: 720, SampleAR: "1:1"},
		{CodecType: "audio", CodecName: "aac", Channels: 2},
	}}
	d := Declared{Codecs: "avc1.4d401f,mp4a.40.2", Resolution: "1280x720"}
	if got := Compare(d, m); len(got) != 0 {
		t.Errorf("a matching stream produced %v", got)
	}
}

func TestCodecMismatch(t *testing.T) {
	m := &Media{Streams: []Stream{
		{CodecType: "video", CodecName: "hevc", Width: 1280, Height: 720},
		{CodecType: "audio", CodecName: "aac"},
	}}
	got := Compare(Declared{Codecs: "avc1.4d401f,mp4a.40.2"}, m)
	if len(got) != 1 || got[0].Kind != "codec_mismatch" {
		t.Fatalf("got %v, want one codec_mismatch", kinds(got))
	}
	if got[0].Detail == "" {
		t.Error("the finding should say what was declared and what was found")
	}
}

// Profile and level differences are not reported: packagers get them subtly
// wrong constantly in ways no player minds, and firing on those would get the
// whole check switched off.
func TestProfileAndLevelAreNotCompared(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264", Profile: "Main"}}}
	if got := Compare(Declared{Codecs: "avc1.640028"}, m); len(got) != 0 {
		t.Errorf("a profile difference should not be reported, got %v", got)
	}
}

// The audio-is-dead fault, one layer below the manifest.
func TestDeclaredAudioMissingFromTheMedia(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264"}}}
	got := Compare(Declared{Codecs: "avc1.4d401f,mp4a.40.2"}, m)
	if len(got) != 1 || got[0].Kind != "audio_track_missing" {
		t.Fatalf("got %v, want one audio_track_missing", kinds(got))
	}
}

// One fault produces one finding: a missing track must not also be reported as
// a codec that failed to match something that was never there.
func TestMissingTrackIsNotAlsoACodecMismatch(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264"}}}
	got := Compare(Declared{Codecs: "avc1.4d401f,mp4a.40.2"}, m)
	for _, k := range kinds(got) {
		if k == "codec_mismatch" {
			t.Errorf("got %v: a missing track was also reported as a mismatch", kinds(got))
		}
	}
}

func TestResolutionMismatch(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264", Width: 1280, Height: 720, SampleAR: "1:1"}}}
	got := Compare(Declared{Codecs: "avc1.4d401f", Resolution: "1920x1080"}, m)
	if len(got) != 1 || got[0].Kind != "resolution_mismatch" {
		t.Fatalf("got %v, want one resolution_mismatch", kinds(got))
	}
}

// Anamorphic content codes 1920x1080 as 1440x1080 with a 4:3 pixel. A manifest
// that correctly declares the display size must not be reported as wrong.
func TestAnamorphicResolutionIsNotAMismatch(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264", Width: 1440, Height: 1080, SampleAR: "4:3"}}}
	if got := Compare(Declared{Resolution: "1920x1080"}, m); len(got) != 0 {
		t.Errorf("anamorphic video reported as a mismatch: %v", got)
	}
	w, h := m.Streams[0].DisplaySize()
	if w != 1920 || h != 1080 {
		t.Errorf("DisplaySize() = %dx%d, want 1920x1080", w, h)
	}
}

// A rendition declares neither codecs nor resolution. Nothing is compared
// against silence.
func TestNothingDeclaredMeansNothingReported(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "audio", CodecName: "aac"}}}
	if got := Compare(Declared{}, m); len(got) != 0 {
		t.Errorf("an undeclared stream produced %v", got)
	}
}

func TestEmptyMediaIsReported(t *testing.T) {
	got := Compare(Declared{Codecs: "avc1.4d401f"}, &Media{})
	if len(got) != 1 || got[0].Kind != "no_streams" {
		t.Fatalf("got %v, want no_streams", kinds(got))
	}
}

// Demuxed HLS is the norm, not an edge case: a variant lists its rendition
// group's audio codec in CODECS while shipping video only. Expecting audio in
// its segments reports every such stream as broken -- which is exactly what
// real ffprobe reported against Apple's own example the first time this ran.
func TestAudioDeliveredByARenditionIsNotMissing(t *testing.T) {
	m := &Media{Streams: []Stream{{CodecType: "video", CodecName: "h264"}}}
	d := Declared{Codecs: "avc1.640020,mp4a.40.2", SeparateKinds: []string{"audio"}}
	if got := Compare(d, m); len(got) != 0 {
		t.Errorf("a demuxed variant was reported as broken: %v", got)
	}

	// Without the rendition group, the same shape is a real fault.
	if got := Compare(Declared{Codecs: "avc1.640020,mp4a.40.2"}, m); len(got) != 1 {
		t.Errorf("a muxed variant with no audio should still be reported, got %v", kinds(got))
	}
}
