package probe

import (
	"context"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

// lowLatencyChecks validates a playlist that publishes EXT-X-PART.
//
// Low-latency HLS is the format's answer to the same problem low-latency DASH
// solves, by a different mechanism: instead of one segment delivered in chunks
// over a held-open response, the segment in production is published as a
// series of small complete parts, and a player is told about the next one
// before it exists. The failure modes differ accordingly, and so do the
// checks -- the DASH `chunked_delivery_missing` test, which looks for a
// Content-Length on an unfinished segment, is meaningless here because every
// part legitimately has one.
//
// What is checkable from the playlist alone is whether the declarations are
// self-consistent. Whether the origin actually honours them is a separate
// question with its own check; see blockingReloadCheck.
func lowLatencyChecks(now time.Time, t config.Target, variant string, pl *hls.MediaPlaylist) []alert.Finding {
	if !pl.LowLatency() {
		return nil
	}
	var out []alert.Finding

	// PART-TARGET is to parts what TARGETDURATION is to segments: the maximum,
	// and the number a player sizes its behaviour from. A playlist publishing
	// parts without declaring it leaves a player unable to compute how close
	// to the edge it may play.
	if pl.PartTarget <= 0 {
		out = append(out, finding(now, t, variant, alert.Warning, "part_target_missing",
			"playlist publishes EXT-X-PART but declares no EXT-X-PART-INF:PART-TARGET"))
	} else {
		// The tolerance mirrors the one used for TARGETDURATION: a part a few
		// milliseconds over a 340ms target is a rounding artefact of the
		// packager's frame arithmetic, not a fault.
		limit := pl.PartTarget * 1.1
		for _, part := range pl.Parts {
			if part.Duration > limit {
				out = append(out, finding(now, t, variant, alert.Warning, "part_target_violation",
					"part duration "+ftoa(part.Duration)+"s exceeds PART-TARGET "+
						ftoa(pl.PartTarget)+"s: "+part.URI))
				break
			}
		}
	}

	sc := pl.ServerControl
	// Without blocking playlist reload a player has to poll, and polling at
	// part cadence is both the latency low-latency HLS exists to remove and a
	// request rate no origin enjoys. A stream publishing parts and not
	// offering it has built half the mechanism.
	if !sc.CanBlockReload {
		out = append(out, finding(now, t, variant, alert.Warning, "blocking_reload_undeclared",
			"playlist publishes parts but EXT-X-SERVER-CONTROL does not declare "+
				"CAN-BLOCK-RELOAD=YES, so players must poll for updates"))
	}

	// PART-HOLD-BACK is how close to the live edge a player may play. The
	// specification requires at least three part durations, and the reason is
	// concrete: at less than that a player is playing content whose successor
	// may not be published yet, and it stalls at the edge on every jitter.
	switch {
	case sc.PartHoldBack <= 0 && pl.PartTarget > 0:
		out = append(out, finding(now, t, variant, alert.Warning, "part_hold_back_missing",
			"playlist publishes parts but declares no PART-HOLD-BACK, so players "+
				"have nothing to tell them how close to the edge is safe"))
	case pl.PartTarget > 0 && sc.PartHoldBack > 0 && sc.PartHoldBack < 3*pl.PartTarget:
		out = append(out, finding(now, t, variant, alert.Warning, "part_hold_back_too_small",
			"PART-HOLD-BACK "+ftoa(sc.PartHoldBack)+"s is below the three part durations "+
				"the spec requires ("+ftoa(3*pl.PartTarget)+"s): players will stall at the edge"))
	}

	// A player joining a stream, or switching rungs, can only start on a part
	// that begins with an IDR frame. A window of parts with none means the
	// join has to wait for the next segment boundary, which is the latency
	// being paid for elsewhere.
	if len(pl.Parts) > 0 {
		independent := false
		for _, part := range pl.Parts {
			if part.Independent {
				independent = true
				break
			}
		}
		if !independent {
			out = append(out, finding(now, t, variant, alert.Info, "no_independent_part",
				"none of the "+itoa(len(pl.Parts))+" published parts is INDEPENDENT, so a "+
					"player joining now must wait for the next segment"))
		}
	}

	return out
}

// blockingReloadCheck asks whether the origin does what the playlist says it
// will, rather than whether it says it.
//
// This is the low-latency HLS counterpart of the DASH chunked-delivery check,
// and it exists for the same reason: the declaration is cheap and the
// behaviour is what matters. An origin that advertises CAN-BLOCK-RELOAD but
// answers a blocking request immediately with the playlist it already had has
// broken nothing visible -- the playlist is valid, every part serves, players
// play -- and every player is back to polling, seconds behind where the design
// says. Nothing else here can see it.
//
// The mechanism is the one from the specification: request the playlist with
// _HLS_msn and _HLS_part naming a part that does not exist yet. A conforming
// origin holds the response until it does. One that returns at once either
// ignored the parameters or served a cached copy, and either way the low
// latency is not there.
func (p *Prober) blockingReloadCheck(ctx context.Context, t config.Target, plURL, variant string,
	pl *hls.MediaPlaylist) []alert.Finding {

	if !pl.LowLatency() || !pl.ServerControl.CanBlockReload || pl.PartTarget <= 0 {
		return nil
	}
	// The next part of the segment in production: the first thing that does
	// not exist yet. Asking for something further ahead would be answered
	// immediately with an error by a conforming server, which is not what is
	// being tested.
	msn := pl.MediaSequence + len(pl.Segments)
	part := len(pl.Parts)

	// fetch measures its own wall-clock duration. Timing this with the
	// prober's injectable clock would measure nothing: that clock is frozen
	// under test and does not advance across a real request.
	res := p.fetch(ctx, t, plURL+blockingQuery(plURL, msn, part))
	waited := res.dur

	now := p.now().UTC()
	if res.err != nil || res.status != 200 {
		// Availability is another check's business. A server that rejects the
		// query parameters outright is a different fault from one that
		// accepts and ignores them, and not one this check should guess at.
		return nil
	}

	// A response that arrives in appreciably less than a part duration cannot
	// have waited for a part that did not exist. Half is the threshold because
	// a part may already have been in flight when the request arrived, and
	// reporting a stream that was merely quick would make the check useless.
	target := time.Duration(pl.PartTarget * float64(time.Second))
	if waited < target/2 {
		return []alert.Finding{finding(now, t, variant, alert.Warning, "blocking_reload_missing",
			"origin declares CAN-BLOCK-RELOAD=YES but answered a request for part "+
				itoa(part)+" of segment "+itoa(msn)+" in "+ftoa(waited.Seconds())+
				"s, less than half a part: it is not holding the request, so players "+
				"must poll and the stream runs at polling latency")}
	}
	return nil
}

// blockingQuery builds the _HLS_msn / _HLS_part query the specification
// defines, respecting any query string the playlist URL already carries --
// token-authenticated origins put one there, and dropping it turns this check
// into an authentication failure.
func blockingQuery(plURL string, msn, part int) string {
	sep := "?"
	for i := 0; i < len(plURL); i++ {
		if plURL[i] == '?' {
			sep = "&"
			break
		}
	}
	return sep + "_HLS_msn=" + itoa(msn) + "&_HLS_part=" + itoa(part)
}
