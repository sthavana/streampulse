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
func (p *Prober) probeDASH(ctx context.Context, t config.Target, res fetchResult) {
	labels := map[string]string{"target": t.Name}

	m, err := dash.Parse([]byte(res.body), t.URL)
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
		p.probeRepresentation(ctx, t, r, now, res.cache)
	}
}

// probeRepresentation records the state of one representation and fetch-checks
// its most recent segments.
func (p *Prober) probeRepresentation(ctx context.Context, t config.Target, r *dash.Representation, now time.Time, cache cacheInfo) {
	variant := r.Label()
	labels := map[string]string{"target": t.Name, "variant": variant}

	p.reg.SetGauge("streampulse_stream_live", helpStreamLive, boolGauge(r.Period().MPD().Dynamic()), labels)

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

	for _, f := range p.dashRepChecks(now, t, edgeKey(t, variant), variant, r, segs, cache) {
		p.record(f)
	}
	for _, f := range p.dashDRMChecks(now, t, r, variant) {
		p.record(f)
	}

	if t.SegmentSample > 0 {
		p.probeInit(ctx, t, variant, r.InitURI(), 0)
		p.inspectRepresentation(ctx, t, r, variant)
		p.checkChunkedDelivery(ctx, t, r, variant, segs, now)
		urls := make([]string, len(segs))
		for i, s := range segs {
			urls[i] = s.URI
		}
		p.sampleSegments(ctx, t, variant, urls, cache)
	}
}

// checkChunkedDelivery proves a low-latency stream is actually being
// delivered chunk by chunk.
//
// This is the quietest way for low latency to fail. If the packager or a CDN
// in front of it buffers each segment and only responds once it is complete,
// nothing breaks: the manifest is right, every segment serves, players play.
// They just play seconds behind where the design says they should, and the
// entire low-latency build is inert. No other check can see it, because
// nothing is wrong with any single response -- only with when it started.
//
// The signal is the response declaring its own length. A segment still being
// produced cannot have a known length, so a Content-Length on one means the
// origin waited for the whole thing before answering.
func (p *Prober) checkChunkedDelivery(ctx context.Context, t config.Target,
	r *dash.Representation, variant string, segs []dash.Segment, now time.Time) {

	if !r.Period().MPD().LowLatency() {
		return
	}
	// Only a segment still in production can answer the question. A finished
	// one has a length legitimately, and asking about it would report every
	// healthy stream as broken.
	seg, ok := inProduction(segs, now)
	if !ok {
		return
	}

	length, status, err := p.probeStreaming(ctx, t, seg.URI)
	if err != nil || status >= 400 {
		// Availability is the segment sample's job; not this check's business.
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}
	if length >= 0 {
		p.reg.SetGauge("streampulse_chunked_delivery", helpChunked, 0, labels)
		p.emit(t.Name, variant, alert.Warning, "chunked_delivery_missing",
			"low-latency stream returned a complete segment ("+i64toa(length)+
				" bytes, Content-Length set) for one still being produced: "+
				"something between the packager and here is buffering whole segments")
		return
	}
	p.reg.SetGauge("streampulse_chunked_delivery", helpChunked, 1, labels)
}

// inProduction returns the newest segment whose production has not finished
// at now, which is the only one whose delivery can be judged.
func inProduction(segs []dash.Segment, now time.Time) (dash.Segment, bool) {
	for i := len(segs) - 1; i >= 0; i-- {
		if s := segs[i]; !s.CompleteAt.IsZero() && s.CompleteAt.After(now) {
			return s, true
		}
	}
	return dash.Segment{}, false
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
