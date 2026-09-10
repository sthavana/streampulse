package probe

import (
	"strconv"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

// runChecks evaluates the detection rules against one media playlist and returns
// any findings. Stateful checks (freeze, PDT progression) compare this poll to
// the previous one for the same playlist URL.
func (p *Prober) runChecks(t config.Target, plURL, variant string, pl *hls.MediaPlaylist) []alert.Finding {
	var out []alert.Finding
	now := p.now().UTC()

	// --- structural sanity ---
	if pl.TargetDuration == 0 {
		out = append(out, finding(now, t, variant, alert.Warning, "targetduration_missing",
			"playlist has no EXT-X-TARGETDURATION"))
	}
	if len(pl.Segments) == 0 {
		out = append(out, finding(now, t, variant, alert.Critical, "no_segments",
			"playlist contains no media segments"))
		return out
	}

	// --- TARGETDURATION compliance (RFC 8216 4.3.3.1: no segment may exceed it) ---
	if pl.TargetDuration > 0 {
		for _, s := range pl.Segments {
			if s.Duration > float64(pl.TargetDuration)+0.5 {
				out = append(out, finding(now, t, variant, alert.Warning, "targetduration_violation",
					"segment duration "+ftoa(s.Duration)+"s exceeds TARGETDURATION "+itoa(pl.TargetDuration)+"s: "+s.URI))
				break
			}
		}
	}

	live := !pl.EndList
	if t.ExpectLive && pl.EndList {
		out = append(out, finding(now, t, variant, alert.Critical, "unexpected_endlist",
			"expected a live stream but the playlist carries EXT-X-ENDLIST"))
	}

	// --- live window size ---
	if t.MinWindowSec > 0 && pl.Duration() < t.MinWindowSec {
		out = append(out, finding(now, t, variant, alert.Warning, "short_window",
			"live window "+ftoa(pl.Duration())+"s is below the expected "+ftoa(t.MinWindowSec)+"s"))
	}

	if live {
		out = append(out, p.crossPollChecks(now, t, plURL, variant, pl)...)
		out = append(out, edgeStalenessCheck(now, t, variant, pl)...)
	}

	// --- discontinuity awareness (informational; useful around ad breaks) ---
	discCount := 0
	for _, s := range pl.Segments {
		if s.Discontinuity {
			discCount++
		}
	}
	if discCount > 0 {
		out = append(out, finding(now, t, variant, alert.Info, "discontinuity_present",
			itoa(discCount)+" discontinuity marker(s) in the current window"))
	}

	return out
}

// crossPollChecks compares this poll against the stored state for the same
// playlist URL: freeze detection, window rollback, and PDT progression.
func (p *Prober) crossPollChecks(now time.Time, t config.Target, plURL, variant string, pl *hls.MediaPlaylist) []alert.Finding {
	var out []alert.Finding
	// Compare the projected live edge, not the raw anchor tag: packagers emit
	// PDT sparsely (Unified Streaming tags only discontinuity boundaries, ~11
	// tags across a 313-segment window), so the last *tag* sits frozen for
	// dozens of segments while the stream is perfectly healthy. The projected
	// edge advances with every segment added.
	edge := liveEdgePDT(pl)

	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.state[plURL]
	if st == nil {
		// First sighting: record a baseline. There is nothing to compare against
		// yet, so no cross-poll rule can fire.
		p.state[plURL] = &plState{lastSequence: pl.MediaSequence, lastSeqChange: now, lastEdge: edge}
		return out
	}

	seqAdvanced := pl.MediaSequence > st.lastSequence
	switch {
	case seqAdvanced:
		st.lastSequence = pl.MediaSequence
		st.lastSeqChange = now

	case pl.MediaSequence < st.lastSequence:
		// The window moved backwards: an origin failover, or a load balancer
		// serving a stale cache. Distinct from a frozen edge, and always a fault.
		out = append(out, finding(now, t, variant, alert.Critical, "playlist_rollback",
			"media sequence went backwards, "+itoa(st.lastSequence)+" -> "+itoa(pl.MediaSequence)+
				" (stale origin or failover)"))
		st.lastSequence = pl.MediaSequence
		st.lastSeqChange = now

	default:
		// Unchanged. Only a fault once it has outlasted a few segments: polling
		// faster than the segment duration legitimately sees the same playlist
		// on consecutive polls.
		stalled := now.Sub(st.lastSeqChange)
		threshold := time.Duration(3*maxInt(pl.TargetDuration, 2)) * time.Second
		if stalled > threshold {
			out = append(out, finding(now, t, variant, alert.Critical, "playlist_stalled",
				"media sequence has not advanced for "+ftoa(stalled.Seconds())+"s (live edge frozen)"))
		}
	}

	// The edge is only expected to move when the window itself moved. Comparing
	// on every poll fires spuriously whenever the poll interval is shorter than
	// a segment duration, which is the common configuration.
	if edge != nil {
		if seqAdvanced && st.lastEdge != nil && !edge.After(*st.lastEdge) {
			out = append(out, finding(now, t, variant, alert.Warning, "pdt_not_advancing",
				"media sequence advanced but the PROGRAM-DATE-TIME timeline did not"))
		}
		st.lastEdge = edge
	}

	return out
}

// edgeStalenessCheck compares the projected live edge against the local clock.
// Heuristic: it requires an accurate local clock (NTP) and is intentionally
// conservative to avoid false positives on high-latency / large-DVR configs.
func edgeStalenessCheck(now time.Time, t config.Target, variant string, pl *hls.MediaPlaylist) []alert.Finding {
	if pl.TargetDuration == 0 {
		return nil
	}
	edge := liveEdgePDT(pl)
	if edge == nil {
		return nil
	}
	behind := now.Sub(*edge).Seconds()
	if behind > float64(pl.TargetDuration)*3 {
		return []alert.Finding{finding(now, t, variant, alert.Warning, "pdt_stale",
			"live-edge PROGRAM-DATE-TIME is "+ftoa(behind)+"s behind wall-clock")}
	}
	return nil
}

// --- shared helpers (used across the probe package) ---

func finding(now time.Time, t config.Target, variant string, sev alert.Severity, check, msg string) alert.Finding {
	return alert.Finding{
		Time: now, Target: t.Name, Variant: variant,
		Severity: sev, Check: check, Message: msg,
	}
}

// liveEdgePDT projects the wall-clock time at which the last segment ends.
// Packagers commonly emit PDT only on the first segment of a playlist, or only
// after a discontinuity, so the anchor tag is usually not on the final segment:
// the durations of every segment from the anchor onward have to be added to it.
func liveEdgePDT(pl *hls.MediaPlaylist) *time.Time {
	for i := len(pl.Segments) - 1; i >= 0; i-- {
		if pl.Segments[i].ProgramDateTime == nil {
			continue
		}
		edge := *pl.Segments[i].ProgramDateTime
		for _, s := range pl.Segments[i:] {
			edge = edge.Add(time.Duration(s.Duration * float64(time.Second)))
		}
		return &edge
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func itoa(i int) string { return strconv.Itoa(i) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
