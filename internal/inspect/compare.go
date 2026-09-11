package inspect

import (
	"strconv"
	"strings"
)

// Diff is one disagreement between what a manifest declares and what the
// media actually contains.
type Diff struct {
	Kind   string // the check name the caller should report it under
	Detail string
}

// Declared is what the manifest says a stream should be. Zero fields mean the
// manifest did not say, and nothing is compared against silence: an
// EXT-X-MEDIA rendition carries no CODECS or RESOLUTION at all, and inventing
// an expectation for it would manufacture faults.
type Declared struct {
	Codecs     string // RFC 6381 list, e.g. "avc1.4d401f,mp4a.40.2"
	Width      int
	Height     int
	Resolution string // HLS writes it as "1280x720"; parsed into Width/Height
	// SeparateKinds are track kinds this playlist declares but does not
	// carry, because they arrive through a different one. A demuxed HLS
	// variant lists the audio codec of its rendition group in CODECS while
	// shipping video only -- which is not just legal but the norm, and
	// expecting audio in its segments reports every such stream as broken.
	SeparateKinds []string
}

func (d Declared) elsewhere(kind string) bool {
	for _, k := range d.SeparateKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Compare checks the media against what was declared.
func Compare(d Declared, m *Media) []Diff {
	var out []Diff
	if m == nil || len(m.Streams) == 0 {
		return []Diff{{Kind: "no_streams", Detail: "ffprobe found no elementary streams"}}
	}

	w, h := d.Width, d.Height
	if w == 0 && h == 0 && d.Resolution != "" {
		w, h = parseResolution(d.Resolution)
	}

	out = append(out, compareCodecs(d, m)...)
	if w > 0 && h > 0 {
		out = append(out, compareResolution(w, h, m)...)
	}
	return out
}

// compareCodecs matches declared codecs against the streams present.
//
// Only the codec *family* is compared -- avc1 against h264 -- not the profile
// and level in the rest of the RFC 6381 string. Packagers get those subtly
// wrong all the time in ways no player minds, and a check that fires on a
// level mismatch nobody can perceive would be switched off within a week,
// taking the codec check that matters with it.
//
// Each declared codec produces at most one finding, and the right one: a
// missing track is reported as a missing track, not as a codec that failed to
// match something that was never there.
func compareCodecs(d Declared, m *Media) []Diff {
	var out []Diff
	done := map[string]bool{}

	for _, c := range strings.Split(d.Codecs, ",") {
		fam := family(c)
		if fam == "" {
			continue
		}
		kind := kindOf(fam)
		if kind == "" || done[kind] || d.elsewhere(kind) {
			continue
		}
		done[kind] = true

		s := m.first(kind)
		if s == nil {
			out = append(out, Diff{
				Kind:   kind + "_track_missing",
				Detail: "manifest declares " + fam + " but the media has no " + kind + " track (it has " + strings.Join(m.Kinds(), ", ") + ")",
			})
			continue
		}
		if s.CodecName != fam {
			out = append(out, Diff{
				Kind:   "codec_mismatch",
				Detail: "manifest declares " + fam + " for the " + kind + " track but the media carries " + s.CodecName,
			})
		}
	}
	return out
}

func compareResolution(w, h int, m *Media) []Diff {
	v := m.Video()
	if v == nil || v.Width == 0 {
		return nil
	}
	gotW, gotH := v.DisplaySize()
	if gotW == w && gotH == h {
		return nil
	}
	return []Diff{{
		Kind: "resolution_mismatch",
		Detail: "manifest declares " + itoa(w) + "x" + itoa(h) + " but the media is " +
			itoa(gotW) + "x" + itoa(gotH),
	}}
}

// codecFamilies maps the RFC 6381 four-character code a manifest uses to the
// name ffprobe reports.
var codecFamilies = map[string]string{
	"avc1": "h264", "avc3": "h264",
	"hvc1": "hevc", "hev1": "hevc", "dvh1": "hevc", "dvhe": "hevc",
	"vp09": "vp9", "vp08": "vp8", "av01": "av1",
	"mp4a": "aac", // refined below: mp4a.a5 and mp4a.a6 are not AAC
	"ac-3": "ac3", "ec-3": "eac3", "opus": "opus", "fLaC": "flac",
}

// family reduces one RFC 6381 codec string to the ffprobe codec name.
func family(codec string) string {
	c := strings.TrimSpace(codec)
	if c == "" {
		return ""
	}
	// mp4a carries the actual format in its object type: 40.x is AAC, a5 is
	// AC-3 and a6 is Enhanced AC-3, all under the same four-character code.
	if strings.HasPrefix(strings.ToLower(c), "mp4a.") {
		switch strings.ToLower(strings.SplitN(c, ".", 3)[1]) {
		case "a5":
			return "ac3"
		case "a6":
			return "eac3"
		default:
			return "aac"
		}
	}
	four := c
	if i := strings.IndexByte(c, '.'); i > 0 {
		four = c[:i]
	}
	if fam, ok := codecFamilies[four]; ok {
		return fam
	}
	if fam, ok := codecFamilies[strings.ToLower(four)]; ok {
		return fam
	}
	// Unknown four-character codes are ignored rather than reported. New
	// codecs appear faster than this table is updated, and a monitoring tool
	// that cries wolf about a codec it simply has not heard of is worse than
	// one that stays quiet.
	return ""
}

// familyKind says what sort of track a codec family implies. A family absent
// from this table is one whose kind we do not know, and nothing is concluded
// from it.
var familyKind = map[string]string{
	"h264": "video", "hevc": "video", "vp9": "video", "vp8": "video", "av1": "video",
	"aac": "audio", "ac3": "audio", "eac3": "audio", "opus": "audio", "flac": "audio",
}

func kindOf(family string) string { return familyKind[family] }

func parseResolution(r string) (int, int) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(r)), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

func itoa(i int) string { return strconv.Itoa(i) }
