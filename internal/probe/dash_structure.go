package probe

import (
	"strings"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
)

// dashStructureChecks validates the shape of the presentation: whether the
// document describes something a player can actually select from.
//
// These are the DASH counterparts of the EXT-X-MEDIA rendition checks on the
// HLS side, and they answer the same question -- does this manifest promise a
// track it does not deliver. None of them needs a fetch. A manifest can be
// perfectly available, perfectly fresh, every segment 200, and still be
// unplayable because it points at a representation that is not there or offers
// a track with no encodings under it.
//
// They run against every period, including ones the segment checks skip,
// because a broken ad period is exactly the case worth catching.
func dashStructureChecks(now time.Time, t config.Target, m *dash.MPD) []alert.Finding {
	var out []alert.Finding
	for i, p := range m.Periods {
		name := periodName(p, i)

		if len(p.AdaptationSets) == 0 {
			// Most often an SSAI stitcher that opened a period for a break and
			// filled in nothing. Players reaching it have no track to select.
			out = append(out, finding(now, t, "", alert.Critical, "period_empty",
				"period "+name+" declares no adaptation sets: there is nothing to play for its whole duration"))
			continue
		}
		out = append(out, adaptationSetChecks(now, t, p, name)...)
		out = append(out, representationIDChecks(now, t, p, name)...)
		out = append(out, defaultRoleChecks(now, t, p, name)...)
	}
	return out
}

// adaptationSetChecks looks at each track the period declares.
func adaptationSetChecks(now time.Time, t config.Target, p *dash.Period, period string) []alert.Finding {
	var out []alert.Finding
	for i, as := range p.AdaptationSets {
		set := setName(as, i)
		if len(as.Representations) == 0 {
			out = append(out, finding(now, t, "", alert.Critical, "adaptation_set_empty",
				"adaptation set "+set+" in period "+period+
					" declares no representations: a track with no encodings under it"))
			continue
		}
		for _, r := range as.Representations {
			// @mimeType is mandatory on the representation or the set above
			// it, and Parse has already folded the set's down. Without one a
			// player cannot tell what it is being offered.
			if r.MimeType == "" {
				out = append(out, finding(now, t, r.Label(), alert.Warning, "representation_missing_mime",
					"representation "+r.Label()+" in period "+period+
						" declares no @mimeType, and neither does the adaptation set above it"))
			}
			// @codecs is how a player decides whether it can play something
			// before fetching any of it. Missing, it must fetch and find out.
			//
			// Asked of audio and video only. A text track carrying TTML or
			// WebVTT routinely declares no codec and is not wrong to: there is
			// nothing to be incapable of decoding.
			if r.Codecs == "" && codecBearing(as.ContentType) {
				out = append(out, finding(now, t, r.Label(), alert.Warning, "representation_missing_codecs",
					"representation "+r.Label()+" in period "+period+
						" declares no @codecs, so a player cannot tell whether it can decode it without fetching it"))
			}
		}
	}
	return out
}

// representationIDChecks enforces what @id is for.
//
// Two things depend on it and both break silently. $RepresentationID$ in a
// SegmentTemplate expands to it, so two representations sharing an id resolve
// to the same segment URLs -- one of them serves the other's media, at the
// wrong bitrate or the wrong language, with every request succeeding.
// @dependencyId points at it, and a pointer to nothing leaves a player unable
// to assemble a stream it was offered.
func representationIDChecks(now time.Time, t config.Target, p *dash.Period, period string) []alert.Finding {
	// The spec scopes @id uniqueness to the period, and so does this: reusing
	// ids across periods is ordinary and reporting it would be wrong.
	seen := map[string]bool{}
	var dupes []string
	for _, as := range p.AdaptationSets {
		for _, r := range as.Representations {
			if r.ID == "" {
				continue
			}
			if seen[r.ID] {
				dupes = appendOnce(dupes, r.ID)
				continue
			}
			seen[r.ID] = true
		}
	}

	var out []alert.Finding
	for _, id := range dupes {
		out = append(out, finding(now, t, "", alert.Critical, "representation_duplicate_id",
			"representation id "+quote(id)+" is used more than once in period "+period+
				": $RepresentationID$ resolves both to the same segments"))
	}

	for _, as := range p.AdaptationSets {
		for _, r := range as.Representations {
			for _, dep := range strings.Fields(r.DependencyID) {
				if seen[dep] {
					continue
				}
				out = append(out, finding(now, t, r.Label(), alert.Critical, "dependency_missing",
					"representation "+r.Label()+" depends on "+quote(dep)+
						", which period "+period+" does not contain: it cannot be played on its own"))
			}
		}
	}
	return out
}

// defaultRoleChecks looks for an ambiguous default track, the DASH shape of
// two renditions in one HLS group both claiming DEFAULT=YES.
//
// Scoped to one content type and one language, deliberately. Marking the main
// audio of every language with Role=main is ordinary and correct; two English
// audio sets both claiming to be the main one is the fault, because nothing in
// the document says which a player should start with.
func defaultRoleChecks(now time.Time, t config.Target, p *dash.Period, period string) []alert.Finding {
	mains := map[string][]string{}
	for i, as := range p.AdaptationSets {
		if !hasRole(as, "main") {
			continue
		}
		key := as.ContentType + "/" + as.Lang
		mains[key] = append(mains[key], setName(as, i))
	}

	var out []alert.Finding
	// Walked in declaration order rather than over the map, so the findings
	// come out the same way on every poll and dedupe into one incident.
	for i, as := range p.AdaptationSets {
		key := as.ContentType + "/" + as.Lang
		sets := mains[key]
		if len(sets) < 2 || !hasRole(as, "main") || sets[0] != setName(as, i) {
			continue
		}
		out = append(out, finding(now, t, "", alert.Warning, "adaptation_set_multiple_main",
			itoa(len(sets))+" adaptation sets in period "+period+" claim Role=main for "+
				describeTrack(as)+" ("+strings.Join(sets, ", ")+
				"): nothing says which one a player should start with"))
	}
	return out
}

// setName prefers the set's own @id, falling back to its position, so a
// finding can be traced back to a line in the manifest.
func setName(as *dash.AdaptationSet, i int) string {
	if as.ID != "" {
		return quote(as.ID)
	}
	return "#" + itoa(i+1)
}

// describeTrack names what a set carries, for a message a reader can act on.
func describeTrack(as *dash.AdaptationSet) string {
	kind := as.ContentType
	if kind == "" {
		kind = "content"
	}
	if as.Lang == "" {
		return kind
	}
	return kind + " in " + as.Lang
}

func hasRole(as *dash.AdaptationSet, value string) bool {
	for _, r := range as.Roles {
		// The scheme is checked loosely: the 2011 role scheme is the only one
		// in practice, and a packager writing a private scheme still means
		// "main" by "main".
		if strings.EqualFold(r.Value, value) {
			return true
		}
	}
	return false
}

func appendOnce(list []string, s string) []string {
	for _, existing := range list {
		if existing == s {
			return list
		}
	}
	return append(list, s)
}

// codecBearing reports whether a track of this content type is expected to
// name a codec. An empty content type is included: Parse fills it in from the
// mimeType, so blank means the manifest said nothing either way, and the
// question is then worth asking.
func codecBearing(contentType string) bool {
	switch strings.ToLower(contentType) {
	case "audio", "video", "":
		return true
	}
	return false
}
