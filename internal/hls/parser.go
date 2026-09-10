// Package hls provides a small, dependency-free parser for HLS master and
// media playlists (RFC 8216). It is intentionally tolerant: real-world encoders
// and packagers emit slightly off-spec playlists, and a monitoring tool should
// keep going and flag problems rather than fail to parse.
package hls

import (
	"strconv"
	"strings"
	"time"
)

// PlaylistType distinguishes a master (multivariant) playlist from a media playlist.
type PlaylistType int

const (
	Unknown PlaylistType = iota
	Master
	Media
)

// Variant is one entry in a master playlist (one rung of the ABR ladder).
// The *Group fields name the EXT-X-MEDIA groups this variant pulls from.
type Variant struct {
	URI            string
	Bandwidth      int
	AvgBandwidth   int
	Resolution     string
	Codecs         string
	FrameRate      float64
	AudioGroup     string
	VideoGroup     string
	SubtitleGroup  string
	ClosedCaptions string // may be the enumerated value NONE rather than a group id
}

// Rendition is one EXT-X-MEDIA entry: an alternative audio track, subtitle
// track, video angle, or closed-caption stream.
//
// URI is optional and its absence is meaningful, not a defect: audio with no
// URI is muxed into the variant streams, and for CLOSED-CAPTIONS the URI MUST
// NOT be present at all (RFC 8216 4.3.4.1). Only renditions carrying a URI are
// separately fetchable, so only those can be probed.
type Rendition struct {
	Type       string // AUDIO, VIDEO, SUBTITLES, CLOSED-CAPTIONS
	GroupID    string
	Name       string
	Language   string
	URI        string
	Default    bool
	AutoSelect bool
	Forced     bool
	Channels   string
	InstreamID string
}

// Label renders a rendition for use as a metric label and in findings.
//
// The GROUP-ID is included because NAME alone is not unique across a playlist:
// Apple's own reference stream declares three audio renditions all named
// "English" in groups aud1/aud2/aud3 (stereo and two 5.1 mixes). Collapsing
// those to one label would overwrite each other's metrics and merge three
// independent renditions into a single incident. RFC 8216 4.3.4.1.1 requires
// NAME to be unique within a group, so type+group+name is unique.
func (r Rendition) Label() string {
	name := r.Name
	if name == "" {
		name = r.Language
	}
	if name == "" {
		return strings.ToLower(r.Type) + "/" + r.GroupID
	}
	return strings.ToLower(r.Type) + "/" + r.GroupID + "/" + name
}

// MasterPlaylist is the parsed multivariant playlist.
type MasterPlaylist struct {
	Variants   []Variant
	Renditions []Rendition
	// SessionKeys are EXT-X-SESSION-KEY tags, which let a player fetch a
	// licence before it has chosen a variant.
	SessionKeys []Key
}

// RenditionGroups indexes renditions by (type, group id), the pair a variant
// uses to reference them.
func (m *MasterPlaylist) RenditionGroups() map[string][]Rendition {
	g := make(map[string][]Rendition)
	for _, r := range m.Renditions {
		g[r.Type+"/"+r.GroupID] = append(g[r.Type+"/"+r.GroupID], r)
	}
	return g
}

// Segment is one media segment reference in a media playlist.
// Key is the EXT-X-KEY in force for this segment, or nil if it is in the clear.
type Segment struct {
	URI             string
	Duration        float64
	ProgramDateTime *time.Time
	Discontinuity   bool
	Key             *Key
}

// MediaPlaylist is the parsed media (variant) playlist.
type MediaPlaylist struct {
	Version        int
	TargetDuration int
	MediaSequence  int
	EndList        bool
	Segments       []Segment
	// Keys holds every EXT-X-KEY tag in the order it appeared, including
	// METHOD=NONE tags, which are meaningful: they mark a return to clear.
	Keys []Key
}

// DistinctKeys returns the unique encrypting keys referenced by the playlist,
// identified by method, URI and format. Used to spot key rotation.
func (m *MediaPlaylist) DistinctKeys() []Key {
	var out []Key
	seen := map[string]bool{}
	for _, k := range m.Keys {
		if !k.Encrypted() {
			continue
		}
		id := k.Method + "|" + k.URI + "|" + k.KeyFormat
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, k)
	}
	return out
}

// Encrypted reports whether any segment in the window is encrypted.
func (m *MediaPlaylist) Encrypted() bool {
	for _, s := range m.Segments {
		if s.Key != nil {
			return true
		}
	}
	return false
}

// ClearSegments counts segments carrying no key.
func (m *MediaPlaylist) ClearSegments() int {
	n := 0
	for _, s := range m.Segments {
		if s.Key == nil {
			n++
		}
	}
	return n
}

// Duration returns the sum of segment durations (the length of the live/DVR window).
func (m *MediaPlaylist) Duration() float64 {
	var total float64
	for _, s := range m.Segments {
		total += s.Duration
	}
	return total
}

// DetectType inspects the raw playlist body and classifies it.
func DetectType(raw string) PlaylistType {
	if strings.Contains(raw, "#EXT-X-STREAM-INF") {
		return Master
	}
	if strings.Contains(raw, "#EXTINF") {
		return Media
	}
	return Unknown
}

// ParseMaster parses a multivariant playlist. Unknown tags are ignored.
func ParseMaster(raw string) *MasterPlaylist {
	mp := &MasterPlaylist{}
	var pending *Variant
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			v := Variant{
				Codecs:         attrs["CODECS"],
				Resolution:     attrs["RESOLUTION"],
				AudioGroup:     attrs["AUDIO"],
				VideoGroup:     attrs["VIDEO"],
				SubtitleGroup:  attrs["SUBTITLES"],
				ClosedCaptions: attrs["CLOSED-CAPTIONS"],
			}
			v.Bandwidth = atoiSafe(attrs["BANDWIDTH"])
			v.AvgBandwidth = atoiSafe(attrs["AVERAGE-BANDWIDTH"])
			if fr, err := strconv.ParseFloat(attrs["FRAME-RATE"], 64); err == nil {
				v.FrameRate = fr
			}
			pending = &v
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-SESSION-KEY:") {
			mp.SessionKeys = append(mp.SessionKeys,
				parseKey(parseAttributes(strings.TrimPrefix(line, "#EXT-X-SESSION-KEY:"))))
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			mp.Renditions = append(mp.Renditions, Rendition{
				Type:       attrs["TYPE"],
				GroupID:    attrs["GROUP-ID"],
				Name:       attrs["NAME"],
				Language:   attrs["LANGUAGE"],
				URI:        attrs["URI"],
				Default:    attrs["DEFAULT"] == "YES",
				AutoSelect: attrs["AUTOSELECT"] == "YES",
				Forced:     attrs["FORCED"] == "YES",
				Channels:   attrs["CHANNELS"],
				InstreamID: attrs["INSTREAM-ID"],
			})
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// A bare line following an EXT-X-STREAM-INF is the variant URI.
		if pending != nil {
			pending.URI = line
			mp.Variants = append(mp.Variants, *pending)
			pending = nil
		}
	}
	return mp
}

// ParseMedia parses a media playlist. Unknown tags are ignored.
func ParseMedia(raw string) *MediaPlaylist {
	pl := &MediaPlaylist{}
	var (
		pendingDur  float64
		pendingPDT  *time.Time
		pendingDisc bool
		haveInf     bool
		// An EXT-X-KEY applies to every segment that follows it, until the
		// next EXT-X-KEY. METHOD=NONE clears it.
		currentKey *Key
	)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			pl.TargetDuration = atoiSafe(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			pl.MediaSequence = atoiSafe(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			pl.Version = atoiSafe(strings.TrimPrefix(line, "#EXT-X-VERSION:"))
		case line == "#EXT-X-ENDLIST":
			pl.EndList = true
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			k := parseKey(parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:")))
			pl.Keys = append(pl.Keys, k)
			if k.Encrypted() {
				kc := k
				currentKey = &kc
			} else {
				currentKey = nil
			}
		case line == "#EXT-X-DISCONTINUITY":
			pendingDisc = true
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			if t, err := parsePDT(strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")); err == nil {
				pendingPDT = &t
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if idx := strings.Index(v, ","); idx >= 0 {
				v = v[:idx]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				pendingDur = d
				haveInf = true
			}
		case strings.HasPrefix(line, "#"):
			// ignore other tags for now (KEY, MAP, BYTERANGE, etc.)
		default:
			if haveInf {
				pl.Segments = append(pl.Segments, Segment{
					URI:             line,
					Duration:        pendingDur,
					ProgramDateTime: pendingPDT,
					Discontinuity:   pendingDisc,
					Key:             currentKey,
				})
				pendingDur, pendingPDT, pendingDisc, haveInf = 0, nil, false, false
			}
		}
	}
	return pl
}

// --- helpers ---

// splitAttributes splits an HLS attribute list on commas, respecting quotes
// so that e.g. CODECS="avc1.4d401f,mp4a.40.2" stays intact.
func splitAttributes(s string) []string {
	var parts []string
	var b strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			b.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func parseAttributes(s string) map[string]string {
	m := make(map[string]string)
	for _, part := range splitAttributes(s) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		m[key] = val
	}
	return m
}

func atoiSafe(s string) int {
	// Some encoders emit fractional TARGETDURATION; accept and floor it.
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return int(f)
	}
	return 0
}

func parsePDT(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z0700"}
	var (
		t   time.Time
		err error
	)
	for _, l := range layouts {
		if t, err = time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return t, err
}
