package probe

import (
	"context"
	"sort"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
)

// probeDASH runs one cycle against a DASH target.
//
// The shape of the walk differs from HLS in one way worth naming: an MPD is a
// single document that already describes every representation, so there is no
// second fetch per rung of the ladder. Where the HLS path fetches a media
// playlist per variant, this one goes straight from the manifest to segments.
// That is why streampulse_variant_up and streampulse_media_fetch_seconds have
// no DASH equivalent -- there is no per-representation manifest to be up.
func (p *Prober) probeDASH(ctx context.Context, t config.Target, raw string) {
	labels := map[string]string{"target": t.Name}

	m, err := dash.Parse([]byte(raw), t.URL)
	if err != nil {
		p.emit(t.Name, "", alert.Critical, "manifest_parse", "MPD did not parse: "+err.Error())
		return
	}

	reps := m.Representations()
	p.reg.SetGauge("streampulse_period_count", "Periods declared in the MPD", float64(len(m.Periods)), labels)
	p.reg.SetGauge("streampulse_representation_count", "Representations declared in the MPD", float64(len(reps)), labels)
	if len(reps) == 0 {
		p.emit(t.Name, "", alert.Warning, "manifest_empty", "MPD declared no representations")
		return
	}

	// One clock reading for the whole cycle. Every representation's
	// availability window is computed from it, so taking it once keeps the
	// ladder consistent with itself even if the cycle takes a second or two.
	now := p.now().UTC()
	for _, f := range dashManifestChecks(now, t, m) {
		p.record(f)
	}
	for _, r := range selectRepresentations(t, m) {
		p.probeRepresentation(ctx, t, r, now)
	}
}

// probeRepresentation records the state of one representation and fetch-checks
// its most recent segments.
func (p *Prober) probeRepresentation(ctx context.Context, t config.Target, r *dash.Representation, now time.Time) {
	variant := r.Label()
	labels := map[string]string{"target": t.Name, "variant": variant}

	segs := r.SegmentsAt(now)
	p.reg.SetGauge("streampulse_segment_count", helpSegmentCount, float64(len(segs)), labels)
	if len(segs) == 0 {
		// Nothing is fetchable right now. On a live stream that means the
		// packager has published nothing inside the DVR window, which is the
		// DASH form of an empty playlist.
		p.emit(t.Name, variant, alert.Critical, "no_segments",
			"no segments are available in the current window ("+string(r.Addressing())+" addressing)")
		return
	}

	last := segs[len(segs)-1]
	p.reg.SetGauge("streampulse_media_sequence", helpSequence, float64(last.Number), labels)
	p.reg.SetGauge("streampulse_playlist_window_seconds", helpWindow, (last.End() - segs[0].Start).Seconds(), labels)

	for _, f := range p.dashRepChecks(now, t, edgeKey(t, variant), variant, r, segs) {
		p.record(f)
	}
	for _, f := range p.dashDRMChecks(now, t, r, variant) {
		p.record(f)
	}

	if t.SegmentSample > 0 {
		p.checkInitSegment(ctx, t, r, variant)
		urls := make([]string, len(segs))
		for i, s := range segs {
			urls[i] = s.URI
		}
		p.sampleSegments(ctx, t, variant, urls)
	}
}

// checkInitSegment proves the initialisation segment is fetchable.
//
// It is worth a request of its own because its failure mode is invisible to
// every other check: the manifest parses, every media segment is served, and
// playback still cannot start, because a player fetches the init segment first
// and has nothing to initialise the decoder with. It is also the one fetch
// where a 404 is unambiguous -- unlike a media segment at the live edge, an
// init segment is static and should never not be there.
func (p *Prober) checkInitSegment(ctx context.Context, t config.Target, r *dash.Representation, variant string) {
	init := r.InitURI()
	if init == "" {
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}
	_, status, err := p.probeSegment(ctx, init)
	if err != nil || status >= 400 {
		detail := "HTTP " + itoa(status)
		if err != nil {
			detail = err.Error()
		}
		p.reg.SetGauge("streampulse_init_segment_available", helpInitUp, 0, labels)
		p.emit(t.Name, variant, alert.Critical, "init_segment_availability",
			"initialisation segment not available ("+detail+"): "+init)
		return
	}
	p.reg.SetGauge("streampulse_init_segment_available", helpInitUp, 1, labels)
}

// selectRepresentations picks which representations to probe.
//
// MaxRepresentations caps per adaptation set rather than per manifest, and
// keeps the highest-bandwidth rungs. A flat cap over the flattened ladder
// would truncate wherever the packager happened to put the boundary -- on a
// document that lists six video rungs before its audio, "max 3" would stop
// probing audio entirely, which is exactly the break this tool exists to
// catch. Per set, "1" means "the top rung of every track".
func selectRepresentations(t config.Target, m *dash.MPD) []*dash.Representation {
	var out []*dash.Representation
	for _, period := range m.Periods {
		for _, as := range period.AdaptationSets {
			if !typeWanted(t.RepresentationTypes, as.ContentType) {
				continue
			}
			picked := make([]*dash.Representation, len(as.Representations))
			copy(picked, as.Representations)
			if n := t.MaxRepresentations; n > 0 && len(picked) > n {
				sort.SliceStable(picked, func(i, j int) bool {
					return picked[i].Bandwidth > picked[j].Bandwidth
				})
				picked = picked[:n]
			}
			out = append(out, picked...)
		}
	}
	return out
}
