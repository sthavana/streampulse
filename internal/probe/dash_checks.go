package probe

import (
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
)

// dashManifestChecks validates the presentation as a whole, once per cycle.
func dashManifestChecks(now time.Time, t config.Target, m *dash.MPD) []alert.Finding {
	var out []alert.Finding
	if t.ExpectLive && !m.Dynamic() {
		out = append(out, finding(now, t, "", alert.Critical, "unexpected_static",
			`expected a live stream but the MPD declares type="static"`))
	}

	// Low-latency playback is anchored to wall clock: a player computes the
	// live edge from availabilityStartTime and its own clock, and at a
	// three-second target a few seconds of drift is the whole latency budget.
	// UTCTiming is how the manifest tells it whose clock to trust, and
	// without one every viewer is guessing from their own.
	if m.Dynamic() && m.LowLatency() && len(m.UTCTimings) == 0 {
		out = append(out, finding(now, t, "", alert.Warning, "utc_timing_missing",
			"low-latency stream declares no UTCTiming, so players must trust their own clocks"))
	}

	// Period boundaries are DASH's splice points -- the same ad-break
	// awareness EXT-X-DISCONTINUITY gives on the HLS side, so it reuses that
	// check name rather than inventing a parallel vocabulary for one concept.
	if n := len(m.Periods); n > 1 {
		out = append(out, finding(now, t, "", alert.Info, "discontinuity_present",
			itoa(n)+" periods in the presentation (each boundary is a splice point)"))
	}
	out = append(out, periodContinuityChecks(now, t, m)...)
	out = append(out, dashStructureChecks(now, t, m)...)
	return append(out, declaredWindowCheck(now, t, m)...)
}

// minPeriodBreak is the smallest hole or overlap between two periods worth
// reporting. A frame at 24fps is 41ms, so below this there is no frame for a
// player to miss and the number is a stitcher rounding its arithmetic.
const minPeriodBreak = 50 * time.Millisecond

// periodContinuityChecks looks for holes and overlaps at period boundaries.
//
// This is where server-side ad insertion goes wrong. A stitcher splicing a
// break into a live presentation writes the new period's @start and the
// previous period's @duration, and if its arithmetic is off by a frame -- or
// by the length of the whole break -- the presentation has a hole in it or two
// periods claiming the same instant. Players stall at the boundary, which
// viewers experience as the stream dying exactly when the ad starts.
//
// Only periods that state both numbers are compared. Where @start is absent
// the parser derives it from the previous period's duration, and comparing
// that against the duration it was derived from is comparing our own
// arithmetic to itself: it can only ever agree, which would look like coverage
// while checking nothing.
func periodContinuityChecks(now time.Time, t config.Target, m *dash.MPD) []alert.Finding {
	var out []alert.Finding
	for i := 1; i < len(m.Periods); i++ {
		prev, cur := m.Periods[i-1], m.Periods[i]
		// Belt and braces: today the parser's derivation makes these agree by
		// construction, so this guard changes no outcome and no test can
		// distinguish it. It stays because the invariant lives in another
		// file, and a change to how period times are derived would otherwise
		// turn every ordinary manifest into a boundary fault.
		if !cur.RawStart.Set || !prev.RawDuration.Set {
			continue
		}
		end, ok := prev.End()
		if !ok {
			continue
		}
		delta := cur.Start - end
		if delta > -minPeriodBreak && delta < minPeriodBreak {
			continue
		}
		where := "between period " + periodName(prev, i-1) + " and " + periodName(cur, i)
		if delta > 0 {
			out = append(out, finding(now, t, "", severityFor(delta, time.Second), "period_gap",
				"a "+ftoa(delta.Seconds())+"s hole "+where+
					": nothing is presented from "+ftoa(end.Seconds())+"s to "+ftoa(cur.Start.Seconds())+"s"))
			continue
		}
		out = append(out, finding(now, t, "", severityFor(-delta, time.Second), "period_overlap",
			"a "+ftoa(-delta.Seconds())+"s overlap "+where+
				": period "+periodName(cur, i)+" starts before period "+periodName(prev, i-1)+" ends"))
	}
	return out
}

// periodName prefers the period's own @id, since that is what the packager's
// logs will call it, and falls back to its position.
func periodName(p *dash.Period, i int) string {
	if p.ID != "" {
		return p.ID
	}
	return "#" + itoa(i+1)
}

// severityFor grades a break by size. Below the bar it is a defect worth
// knowing about; at or above it, it is long enough that a viewer sees it.
func severityFor(size, bar time.Duration) alert.Severity {
	if size >= bar {
		return alert.Critical
	}
	return alert.Warning
}

// dashRepChecks evaluates the per-representation rules, including the ones
// that compare this poll against the last.
func (p *Prober) dashRepChecks(now time.Time, t config.Target, key, variant string,
	r *dash.Representation, segs []dash.Segment, cache cacheInfo) []alert.Finding {

	var out []alert.Finding
	if len(segs) == 0 {
		return out
	}

	window := segs[len(segs)-1].End() - segs[0].Start
	if t.MinWindowSec > 0 && window.Seconds() < t.MinWindowSec {
		out = append(out, finding(now, t, variant, alert.Warning, "short_window",
			"window "+ftoa(window.Seconds())+"s is below the expected "+ftoa(t.MinWindowSec)+"s"))
	}
	out = append(out, timelineChecks(now, t, variant, r, segs)...)
	out = append(out, segmentDurationCheck(now, t, variant, r, segs)...)
	return append(out, p.dashEdgeChecks(now, t, key, variant, r, segs, cache)...)
}

// dashEdgeChecks is freeze and rollback detection for DASH.
//
// It compares the *manifest-declared* live edge across polls, which is a
// narrower thing than it sounds. On a SegmentTimeline the manifest states
// where the edge is, and a packager that stops publishing produces a
// byte-identical timeline on the next poll -- so a frozen edge is visible.
// A number-addressed template states no such thing: the MPD is a formula, and
// SegmentsAt derives the edge from the local clock, so it advances whether or
// not the packager is still alive. Comparing that would be comparing our own
// clock to itself and would never fire, which is worse than not checking --
// it would look like coverage.
//
// Number-addressed live streams are not left uncovered, they are covered
// somewhere else: the segment sample asks the CDN for the segment at the
// computed edge every poll, and a packager that has stopped publishing fails
// that fetch. segment_availability is the freeze check for those streams.
func (p *Prober) dashEdgeChecks(now time.Time, t config.Target, key, variant string,
	r *dash.Representation, segs []dash.Segment, cache cacheInfo) []alert.Finding {

	tick, ok := manifestEdge(r, segs)
	if !ok {
		return nil
	}

	var out []alert.Finding
	last := segs[len(segs)-1]
	threshold := stallThreshold(last.Duration, r.Period().MPD().MinimumUpdatePeriod)

	p.mu.Lock()
	defer p.mu.Unlock()

	// How far behind wall clock the packager's own edge is. Distinct from a
	// freeze: an encoder that is falling behind but still publishing produces
	// a timeline that advances, just never fast enough to catch up.
	// Measured from CompleteAt, not Available: on a low-latency stream
	// Available is shifted earlier by @availabilityTimeOffset, which would
	// flatter the measurement by exactly the amount the offset claims.
	if !last.CompleteAt.IsZero() {
		m := r.Period().MPD()
		if behind := now.Sub(last.CompleteAt); behind > stalenessThreshold(last.Duration, m) {
			out = append(out, finding(now, t, variant, alert.Warning, "edge_stale",
				"live edge is "+ftoa(behind.Seconds())+"s behind wall-clock"+declaredLatency(m)+
					cache.attributeLag(behind)))
		}
	}

	st := p.edges[key]
	if st == nil {
		// First sighting: record a baseline. Nothing to compare against yet.
		p.edges[key] = &edgeState{tick: tick, lastChange: now}
		return out
	}

	switch {
	case tick > st.tick:
		st.tick, st.lastChange = tick, now

	case tick < st.tick:
		// The timeline moved backwards: an origin failover, or a load balancer
		// serving a stale cache. Distinct from a freeze, and always a fault.
		out = append(out, finding(now, t, variant, alert.Critical, "playlist_rollback",
			"timeline went backwards, tick "+i64toa(st.tick)+" -> "+i64toa(tick)+
				" (stale origin or failover)"+cache.describe()))
		st.tick, st.lastChange = tick, now

	default:
		// Unchanged. Only a fault once it has outlasted a few segments:
		// polling faster than the segment duration legitimately sees the same
		// manifest on consecutive polls.
		stalled := now.Sub(st.lastChange)
		if stalled > threshold {
			out = append(out, finding(now, t, variant, alert.Critical, "playlist_stalled",
				"timeline has not advanced for "+ftoa(stalled.Seconds())+"s (live edge frozen)"+
					cache.explain(stalled)))
		}
	}
	return out
}

// manifestEdge returns a position for the live edge that the manifest itself
// asserts, and whether there is one to compare across polls.
//
// A period that declares its own end is complete by definition -- the finished
// period before an ad break, in a live presentation -- and its timeline is not
// expected to grow. Treating that as a freeze would page someone for every ad
// break on the channel.
func manifestEdge(r *dash.Representation, segs []dash.Segment) (int64, bool) {
	if len(segs) == 0 || !r.Period().MPD().Dynamic() {
		return 0, false
	}
	if r.Addressing() != dash.AddressingTimeline {
		return 0, false
	}
	if _, closed := r.Period().End(); closed {
		return 0, false
	}
	return int64(segs[len(segs)-1].Time), true
}

// stallThreshold is how long an unchanged edge is tolerated before it counts
// as frozen.
//
// Three segments is the HLS rule and the starting point, with a 6s floor so a
// stream of sub-second segments does not fire on one slow manifest update.
// @minimumUpdatePeriod then raises it, and has to: it is the packager stating
// how often the manifest changes at all, and a stream that republishes every
// 30s is *supposed* to serve an identical manifest for 30s at a time. Judging
// it by its segment duration alone would report a healthy stream as frozen
// every single poll.
func stallThreshold(segment time.Duration, mup dash.Duration) time.Duration {
	d := 6 * time.Second
	if s := 3 * segment; s > d {
		d = s
	}
	if m := 3 * mup.Or(0); m > d {
		d = m
	}
	return d
}

// stalenessThreshold is how far the live edge may sit behind wall clock before
// it is worth reporting, and it is deliberately coarse.
//
// Two things that are not faults live in this number. A packager cannot
// publish a segment before it has finished producing it, and then it has to
// reach the CDN, so a healthy live edge sits a few segments behind by
// construction -- Unified Streaming's demo channel runs about 6s behind with
// 1.92s segments. And the measurement is against *our* clock, so any NTP skew
// on this host lands here too. A tight threshold would report both as faults
// on every poll.
//
// What it is for is an encoder falling progressively behind, which is
// invisible to every other check: the timeline keeps advancing, so nothing is
// frozen, and the segments all exist. That fault grows to tens of seconds and
// then minutes, so a 30s floor loses nothing worth having.
func stalenessThreshold(segment time.Duration, m *dash.MPD) time.Duration {
	// A stream that declares its latency has told us what "behind" means for
	// it, and that beats any floor invented here. On a three-second target
	// the generic 30s floor is not conservative, it is blind.
	if max, ok := m.MaxLatency(); ok {
		return max
	}
	if target, ok := m.Latency(); ok {
		if d := 2 * target; d > 4*segment {
			return d
		}
		return 4 * segment
	}

	d := 30 * time.Second
	if s := 4 * segment; s > d {
		d = s
	}
	// A stream that tells players to sit well back from the edge is telling us
	// it runs with latency; take it at its word rather than paging about it.
	if spd := m.SuggestedPresentationDelay.Or(0); spd > 0 {
		if v := spd + 4*segment; v > d {
			d = v
		}
	}
	return d
}

// declaredLatency names the operator's own bound in a finding, so the number
// in the message can be read against what they asked for.
func declaredLatency(m *dash.MPD) string {
	if max, ok := m.MaxLatency(); ok {
		return " (declared max " + ftoa(max.Seconds()) + "s)"
	}
	if target, ok := m.Latency(); ok {
		return " (declared target " + ftoa(target.Seconds()) + "s)"
	}
	return ""
}

// edgeKey identifies one representation's edge state across polls. The target
// URL is part of it because a representation id is only unique within its own
// manifest.
// edgeKey identifies one representation of one target across polls.
//
// The target's *name* leads, not its URL. Two targets legitimately share a
// URL -- the same stream probed at two intervals, one with media inspection
// and one without, one origin reached two ways -- and keying on the URL made
// them share cross-poll state. A live soak found it: the faster target
// advanced the edge, so the slower one measured its own perfectly good
// manifest against a newer reading and reported a rollback of exactly one
// segment, seven times in twenty-eight hours. The costlier half never showed
// up in that run: a frozen edge on one target is reported as a rollback and
// never as a stall, because the other target keeps moving the timestamp on.
func edgeKey(t config.Target, variant string) string { return t.Name + "#" + t.URL + "#" + variant }

// breaksInWindow returns the timeline discontinuities that still matter: the
// ones inside the segments a player can currently fetch.
//
// A break that has scrolled out of the DVR is history. It stays in the
// document until the packager trims it, and reporting it every poll until then
// would say a stream is broken long after the hole stopped being reachable.
func breaksInWindow(r *dash.Representation, segs []dash.Segment) []dash.Discontinuity {
	if len(segs) == 0 {
		return nil
	}
	var out []dash.Discontinuity
	for _, b := range r.TimelineBreaks() {
		if b.At >= segs[0].Start {
			out = append(out, b)
		}
	}
	return out
}

// timelineChecks reports holes and overlaps within one representation's
// timeline. See dash.TimelineBreaks for what makes one.
func timelineChecks(now time.Time, t config.Target, variant string,
	r *dash.Representation, segs []dash.Segment) []alert.Finding {

	var out []alert.Finding
	// Graded against the segment length rather than a fixed number of
	// seconds: a 200ms hole is a rounding error on 6s segments and most of a
	// segment on a 320ms low-latency one.
	bar := segs[len(segs)-1].Duration
	for _, b := range breaksInWindow(r, segs) {
		at := "at " + ftoa(b.At.Seconds()) + "s into the period, before segment " + i64toa(b.Number)
		if b.Gap() {
			out = append(out, finding(now, t, variant, severityFor(b.Delta, bar), "timeline_gap",
				"the timeline skips "+ftoa(b.Delta.Seconds())+"s "+at+
					": those segments are not in the manifest and cannot be requested"))
			continue
		}
		out = append(out, finding(now, t, variant, severityFor(-b.Delta, bar), "timeline_overlap",
			"the timeline goes back "+ftoa(-b.Delta.Seconds())+"s "+at+
				": two runs of segments claim the same media time"))
	}
	return out
}

// declaredWindowCheck holds the packager to the DVR depth its own manifest
// advertises.
//
// @timeShiftBufferDepth is a promise to players: seek back this far and the
// segments will be there. A packager that has restarted, or a CDN that has had
// its older objects purged, offers a window far shorter than the promise while
// looking perfectly healthy at the live edge -- every current segment fetches,
// the edge advances, nothing else here fires. The first anyone hears of it is
// a viewer pausing for a minute and finding they cannot resume.
//
// Unlike short_window this needs no configuration: the number to compare
// against is in the document.
//
// The measurement spans periods, and must. The promise is made by the
// presentation, not by whichever period happens to hold the live edge, and a
// stream that has just crossed an ad boundary has most of its DVR in the
// period behind it. Measuring one period against a presentation-wide promise
// reported every multi-period live stream as broken, which is how this was
// found.
func declaredWindowCheck(now time.Time, t config.Target, m *dash.MPD) []alert.Finding {
	depth := m.TimeShiftBufferDepth.Or(0)
	if !m.Dynamic() || depth <= 0 {
		return nil
	}

	var (
		earliest, latest time.Duration
		segment          time.Duration
		found            bool
	)
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			if len(as.Representations) == 0 {
				continue
			}
			// One representation per adaptation set: the rungs of a ladder are
			// segmented alike, and expanding all of them would cost the same
			// answer several times over.
			segs := as.Representations[0].SegmentsAt(now)
			if len(segs) == 0 {
				continue
			}
			// Period-relative times become presentation times here, which is
			// what makes the spans across periods comparable.
			from, to := p.Start+segs[0].Start, p.Start+segs[len(segs)-1].End()
			if !found || from < earliest {
				earliest = from
			}
			if !found || to > latest {
				latest = to
			}
			if d := segs[len(segs)-1].Duration; d > segment {
				segment = d
			}
			found = true
		}
	}
	if !found {
		return nil
	}

	// A live window is legitimately a little short of the promise: the oldest
	// segment expires while the newest is still being produced, and the
	// availability calculation trims at both ends. Allowance is the larger of
	// a quarter of the depth and two segments, so a short DVR on long segments
	// is not reported and a genuinely collapsed one is.
	slack := depth / 4
	if two := 2 * segment; two > slack {
		slack = two
	}
	window := latest - earliest
	if window >= depth-slack {
		return nil
	}
	return []alert.Finding{finding(now, t, "", alert.Warning, "window_below_declared",
		"the manifest promises a "+ftoa(depth.Seconds())+"s DVR window but offers "+
			ftoa(window.Seconds())+"s: a player seeking back further than that finds nothing")}
}

// segmentDurationCheck holds the packager to @maxSegmentDuration.
//
// Players size their buffers from it, and a segment longer than the stated
// maximum is the case the buffer was not sized for: a rebuffer on a stream
// whose every request succeeded. It is a manifest bug rather than a delivery
// one, which is why nothing else here can see it.
func segmentDurationCheck(now time.Time, t config.Target, variant string,
	r *dash.Representation, segs []dash.Segment) []alert.Finding {

	// A ceiling of zero is no ceiling: the attribute is optional.
	max := r.Period().MPD().MaxSegmentDuration.Or(0)
	if max <= 0 {
		return nil
	}
	longest := segs[0]
	for _, s := range segs[1:] {
		if s.Duration > longest.Duration {
			longest = s
		}
	}
	// The tolerance absorbs a packager rounding an ISO-8601 duration on the
	// way out; it is far below anything a buffer notices.
	if longest.Duration <= max+10*time.Millisecond {
		return nil
	}
	return []alert.Finding{finding(now, t, variant, alert.Warning, "segment_duration_violation",
		"segment "+i64toa(longest.Number)+" is "+ftoa(longest.Duration.Seconds())+
			"s, longer than the declared maxSegmentDuration of "+ftoa(max.Seconds())+
			"s that players size their buffers from")}
}
