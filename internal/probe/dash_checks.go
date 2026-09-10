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
	return out
}

// dashRepChecks evaluates the per-representation rules, including the ones
// that compare this poll against the last.
func (p *Prober) dashRepChecks(now time.Time, t config.Target, key, variant string,
	r *dash.Representation, segs []dash.Segment) []alert.Finding {

	var out []alert.Finding
	if len(segs) == 0 {
		return out
	}

	window := segs[len(segs)-1].End() - segs[0].Start
	if t.MinWindowSec > 0 && window.Seconds() < t.MinWindowSec {
		out = append(out, finding(now, t, variant, alert.Warning, "short_window",
			"window "+ftoa(window.Seconds())+"s is below the expected "+ftoa(t.MinWindowSec)+"s"))
	}
	return append(out, p.dashEdgeChecks(now, t, key, variant, r, segs)...)
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
	r *dash.Representation, segs []dash.Segment) []alert.Finding {

	tick, ok := manifestEdge(r, segs)
	if !ok {
		return nil
	}

	var out []alert.Finding
	last := segs[len(segs)-1]

	p.mu.Lock()
	defer p.mu.Unlock()

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
				" (stale origin or failover)"))
		st.tick, st.lastChange = tick, now

	default:
		// Unchanged. Only a fault once it has outlasted a few segments:
		// polling faster than the segment duration legitimately sees the same
		// manifest on consecutive polls.
		stalled := now.Sub(st.lastChange)
		if stalled > stallThreshold(last.Duration, r.Period().MPD().MinimumUpdatePeriod) {
			out = append(out, finding(now, t, variant, alert.Critical, "playlist_stalled",
				"timeline has not advanced for "+ftoa(stalled.Seconds())+"s (live edge frozen)"))
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

// edgeKey identifies one representation's edge state across polls. The target
// URL is part of it because a representation id is only unique within its own
// manifest.
func edgeKey(t config.Target, variant string) string { return t.URL + "#" + variant }
