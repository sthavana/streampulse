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
	now := time.Now().UTC()

	// --- structural sanity ---
	if pl.TargetDuration == 0 {
		out = append(out, finding(t, variant, alert.Warning, "targetduration_missing",
			"playlist has no EXT-X-TARGETDURATION"))
	}
	if len(pl.Segments) == 0 {
		out = append(out, finding(t, variant, alert.Critical, "no_segments",
			"playlist contains no media segments"))
		return out
	}

	// --- TARGETDURATION compliance (RFC 8216 4.3.3.1: no segment may exceed it) ---
	if pl.TargetDuration > 0 {
		for _, s := range pl.Segments {
			if s.Duration > float64(pl.TargetDuration)+0.5 {
				out = append(out, finding(t, variant, alert.Warning, "targetduration_violation",
					"segment duration "+ftoa(s.Duration)+"s exceeds TARGETDURATION "+itoa(pl.TargetDuration)+"s: "+s.URI))
				break
			}
		}
	}

	live := !pl.EndList
	if t.ExpectLive && pl.EndList {
		out = append(out, finding(t, variant, alert.Critical, "unexpected_endlist",
			"expected a live stream but the playlist carries EXT-X-ENDLIST"))
	}

	// --- live window size ---
	if t.MinWindowSec > 0 && pl.Duration() < t.MinWindowSec {
		out = append(out, finding(t, variant, alert.Warning, "short_window",
			"live window "+ftoa(pl.Duration())+"s is below the expected "+ftoa(t.MinWindowSec)+"s"))
	}

	// --- stateful checks: freeze detection + PDT progression ---
	if live {
		latestPDT := lastPDT(pl)

		p.mu.Lock()
		st := p.state[plURL]
		if st == nil {
			st = &plState{lastSequence: pl.MediaSequence, lastSeqChange: now, lastPDT: latestPDT}
			p.state[plURL] = st
		} else {
			if pl.MediaSequence > st.lastSequence {
				st.lastSequence = pl.MediaSequence
				st.lastSeqChange = now
			} else {
				stalled := now.Sub(st.lastSeqChange)
				threshold := time.Duration(3*maxInt(pl.TargetDuration, 2)) * time.Second
				if stalled > threshold {
					out = append(out, finding(t, variant, alert.Critical, "playlist_stalled",
						"media sequence has not advanced for "+ftoa(stalled.Seconds())+"s (live edge frozen)"))
				}
			}
			if latestPDT != nil {
				if st.lastPDT != nil && !latestPDT.After(*st.lastPDT) {
					out = append(out, finding(t, variant, alert.Warning, "pdt_not_advancing",
						"EXT-X-PROGRAM-DATE-TIME did not advance since the previous poll"))
				}
				st.lastPDT = latestPDT
			}
		}
		p.mu.Unlock()

		// Heuristic staleness vs wall-clock. Requires an accurate local clock
		// (NTP) and is intentionally conservative to avoid false positives on
		// high-latency / large-DVR configurations.
		if latestPDT != nil && pl.TargetDuration > 0 {
			lastEnd := latestPDT.Add(time.Duration(pl.Segments[len(pl.Segments)-1].Duration * float64(time.Second)))
			behind := now.Sub(lastEnd).Seconds()
			if behind > float64(pl.TargetDuration)*3 {
				out = append(out, finding(t, variant, alert.Warning, "pdt_stale",
					"live-edge PROGRAM-DATE-TIME is "+ftoa(behind)+"s behind wall-clock"))
			}
		}
	}

	// --- discontinuity awareness (informational; useful around ad breaks) ---
	discCount := 0
	for _, s := range pl.Segments {
		if s.Discontinuity {
			discCount++
		}
	}
	if discCount > 0 {
		out = append(out, finding(t, variant, alert.Info, "discontinuity_present",
			itoa(discCount)+" discontinuity marker(s) in the current window"))
	}

	return out
}

// --- shared helpers (used across the probe package) ---

func finding(t config.Target, variant string, sev alert.Severity, check, msg string) alert.Finding {
	return alert.Finding{
		Time: time.Now().UTC(), Target: t.Name, Variant: variant,
		Severity: sev, Check: check, Message: msg,
	}
}

func lastPDT(pl *hls.MediaPlaylist) *time.Time {
	for i := len(pl.Segments) - 1; i >= 0; i-- {
		if pl.Segments[i].ProgramDateTime != nil {
			return pl.Segments[i].ProgramDateTime
		}
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
