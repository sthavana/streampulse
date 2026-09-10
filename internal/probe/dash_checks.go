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

	// Period boundaries are DASH's splice points -- the same ad-break
	// awareness EXT-X-DISCONTINUITY gives on the HLS side, so it reuses that
	// check name rather than inventing a parallel vocabulary for one concept.
	if n := len(m.Periods); n > 1 {
		out = append(out, finding(now, t, "", alert.Info, "discontinuity_present",
			itoa(n)+" periods in the presentation (each boundary is a splice point)"))
	}
	return out
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
	if !last.Available.IsZero() {
		if behind := now.Sub(last.Available); behind > stalenessThreshold(last.Duration, r.Period().MPD()) {
			out = append(out, finding(now, t, variant, alert.Warning, "edge_stale",
				"live edge is "+ftoa(behind.Seconds())+"s behind wall-clock"))
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

// edgeKey identifies one representation's edge state across polls. The target
// URL is part of it because a representation id is only unique within its own
// manifest.
func edgeKey(t config.Target, variant string) string { return t.URL + "#" + variant }
