package probe

import (
	"strings"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

// masterChecks validates the structure of a multivariant playlist and the
// EXT-X-MEDIA groups its variants reference. These are packaging faults that
// no amount of segment probing would surface: a player resolves them at parse
// time and fails, or silently drops a track.
func masterChecks(now time.Time, t config.Target, m *hls.MasterPlaylist) []alert.Finding {
	var out []alert.Finding
	groups := m.RenditionGroups()

	// --- variants referencing a group that was never declared ---
	// A dangling reference means a player asks for audio or subtitles that do
	// not exist. Reported once per missing group rather than once per variant.
	seen := map[string]bool{}
	for _, v := range m.Variants {
		for _, ref := range []struct{ kind, group string }{
			{"AUDIO", v.AudioGroup},
			{"VIDEO", v.VideoGroup},
			{"SUBTITLES", v.SubtitleGroup},
			{"CLOSED-CAPTIONS", v.ClosedCaptions},
		} {
			if ref.group == "" {
				continue
			}
			// CLOSED-CAPTIONS=NONE is an enumerated value meaning "this variant
			// carries no captions", not a group id. RFC 8216 4.3.4.2.
			if ref.kind == "CLOSED-CAPTIONS" && strings.EqualFold(ref.group, "NONE") {
				continue
			}
			key := ref.kind + "/" + ref.group
			if len(groups[key]) > 0 || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, finding(now, t, "", alert.Critical, "rendition_group_missing",
				"variant references "+ref.kind+" group "+quote(ref.group)+
					" but no EXT-X-MEDIA declares it"))
		}
	}

	// --- per-group rules (RFC 8216 4.3.4.1.1) ---
	for key, rs := range groups {
		names := map[string]bool{}
		defaults := 0
		for _, r := range rs {
			if r.Default {
				defaults++
			}
			if r.Name != "" {
				if names[r.Name] {
					out = append(out, finding(now, t, r.Label(), alert.Warning, "rendition_duplicate_name",
						"group "+quote(key)+" has more than one rendition named "+quote(r.Name)))
				}
				names[r.Name] = true
			}
			// "The URI attribute MUST NOT be present" for CLOSED-CAPTIONS:
			// captions ride inside the video segments.
			if r.Type == "CLOSED-CAPTIONS" && r.URI != "" {
				out = append(out, finding(now, t, r.Label(), alert.Warning, "closed_captions_with_uri",
					"CLOSED-CAPTIONS rendition "+quote(r.Name)+" carries a URI, which the spec forbids"))
			}
		}
		if defaults > 1 {
			out = append(out, finding(now, t, "", alert.Warning, "rendition_multiple_default",
				"group "+quote(key)+" has "+itoa(defaults)+" renditions marked DEFAULT=YES, expected at most one"))
		}
	}

	return out
}

// selectRenditions returns the renditions worth probing: those carrying a URI,
// filtered by the target's configured types and capped by MaxRenditions.
func selectRenditions(t config.Target, m *hls.MasterPlaylist) []hls.Rendition {
	var out []hls.Rendition
	for _, r := range m.Renditions {
		// No URI means the track is muxed into the variants (or is
		// CLOSED-CAPTIONS): there is nothing separate to fetch.
		if r.URI == "" {
			continue
		}
		if !typeWanted(t.RenditionTypes, r.Type) {
			continue
		}
		out = append(out, r)
	}
	if t.MaxRenditions > 0 && len(out) > t.MaxRenditions {
		out = out[:t.MaxRenditions]
	}
	return out
}

// typeWanted reports whether a rendition type is in the configured set. An
// empty set means every type.
func typeWanted(want []string, typ string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if strings.EqualFold(strings.TrimSpace(w), typ) {
			return true
		}
	}
	return false
}

func quote(s string) string { return "\"" + s + "\"" }
