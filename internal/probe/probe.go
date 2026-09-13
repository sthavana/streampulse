// Package probe is the synthetic prober: it pulls manifests and segments the
// way a player would, parses them, runs health checks, and emits metrics and
// findings. It holds a little per-playlist state so it can detect changes
// across polls (e.g. a frozen live edge).
package probe

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
	"streampulse/internal/hls"
	"streampulse/internal/inspect"
	"streampulse/internal/metrics"
)

// Help text for the series both manifest formats emit. Registry.register keeps
// whichever help text arrives first, so the HLS and DASH paths declaring the
// same series differently would silently ship whichever ran first.
const (
	helpProbeUp        = "1 if the target manifest is reachable"
	helpManifestFetch  = "Time to fetch the top-level manifest"
	helpSequence       = "Live-edge sequence (EXT-X-MEDIA-SEQUENCE, or the newest DASH segment number)"
	helpTimelineBreaks = "Holes and overlaps in the fetchable part of a DASH SegmentTimeline"
	helpWindow         = "Length of the live/DVR window in seconds"
	helpSegmentCount   = "Segments in the current window"
	helpSegmentUp      = "1 if a sampled segment is fetchable"
	helpSegmentTTFB    = "Time-to-first-byte for a sampled segment"
	helpKeyCount       = "Distinct encrypting keys (HLS) or DRM systems (DASH) in force"
	helpInitUp         = "1 if the initialisation segment is fetchable"
	helpManifestAge    = "Age header on the manifest response: seconds since the origin generated it"
	helpCacheHit       = "1 if the manifest response was a CDN cache hit, 0 if a miss"
	helpChunked        = "1 if a low-latency segment is delivered chunked, 0 if buffered whole"
	helpInspectSeconds = "Time ffprobe took to read the media"
	helpMediaReadable  = "1 if ffprobe could read the media, 0 if not"
	helpMediaStreams   = "Elementary streams ffprobe found in the media"
	helpAudioPeak      = "Peak audio level of the sampled segment, dBFS"
	helpAudioMean      = "Mean audio level of the sampled segment, dBFS"
	helpTSAligned      = "1 if the sampled segment aligns as a transport stream"
	helpTSPackets      = "Transport stream packets in the sampled segment"
	helpTSContinuity   = "Continuity counter breaks in the sampled segment"
	helpTSTransport    = "Packets carrying the transport error indicator"
	helpBlackSeconds   = "Longest run of black video in the sampled segment"
	helpFreezeSeconds  = "Longest run of frozen video in the sampled segment"
	helpStreamLive     = "1 if the stream is live, 0 if it is complete (EXT-X-ENDLIST, or MPD type=static)"
)

type Prober struct {
	client   *http.Client
	reg      *metrics.Registry
	notifier alert.Notifier
	// inspector is nil-safe: a disabled one reports unavailable and every
	// inspection check is skipped.
	inspector  *inspect.Inspector
	inspectFor time.Duration
	// blackFor and freezeFor are what fraction of a segment must be black or
	// frozen before it is worth reporting. freezeFor of 0 disables the check.
	blackFor  float64
	freezeFor float64
	// vantage names where this prober is running, for a fleet watching the
	// same streams from several places.
	vantage string
	// frames holds the latest picture and audio level per stream, for the UI.
	frames *inspect.Frames
	now    func() time.Time // injectable so the cross-poll checks are testable

	mu    sync.Mutex
	state map[string]*plState   // keyed by media-playlist URL
	edges map[string]*edgeState // keyed by target URL + representation label
}

// edgeState is the previous poll's view of one DASH representation's
// manifest-declared live edge.
type edgeState struct {
	tick       int64
	lastChange time.Time
	// The DRM the representation declared last time, and when it last
	// differed. Kept beside the edge because both are per representation and
	// both are only meaningful across polls.
	lastKeyID     string
	lastKeyChange time.Time
}

type plState struct {
	lastSequence  int
	lastSeqChange time.Time
	lastEdge      *time.Time // projected live edge, not the raw PDT anchor tag
	lastKeyID     string
	lastKeyChange time.Time
}

func New(reg *metrics.Registry, n alert.Notifier) *Prober {
	return &Prober{
		client:     &http.Client{Timeout: 15 * time.Second},
		reg:        reg,
		notifier:   n,
		now:        time.Now,
		state:      make(map[string]*plState),
		edges:      make(map[string]*edgeState),
		inspector:  &inspect.Inspector{},
		inspectFor: 20 * time.Second,
		blackFor:   0.9,
		freezeFor:  0, // off unless asked for; see contentChecks
		frames:     inspect.NewFrames(200),
	}
}

// Frames exposes the latest captured picture per stream, for the web UI.
func (p *Prober) Frames() *inspect.Frames { return p.frames }

// SetInspector enables media inspection. Passing a disabled inspector, or
// never calling this, leaves every other check untouched.
func (p *Prober) SetInspector(i *inspect.Inspector, timeout time.Duration) {
	p.inspector = i
	if timeout > 0 {
		p.inspectFor = timeout
	}
}

// SetVantage names where this prober observes from. Empty, the default, adds
// no label anywhere, so a single-prober setup is unchanged.
func (p *Prober) SetVantage(name string) { p.vantage = name }

// SetContentThresholds sets what fraction of a segment must be black or frozen
// before it is reported. Either at 0 disables that check; a negative black
// threshold is how the black check is turned off, since its default is on.
func (p *Prober) SetContentThresholds(black, freeze float64) {
	if black != 0 {
		p.blackFor = black
	}
	if freeze != 0 {
		p.freezeFor = freeze
	}
}

// ProbeTarget runs one full probe cycle for a target.
func (p *Prober) ProbeTarget(ctx context.Context, t config.Target) {
	labels := map[string]string{"target": t.Name}

	res := p.fetch(ctx, t, t.URL)
	p.reg.SetGauge("streampulse_manifest_fetch_seconds", helpManifestFetch, res.dur.Seconds(), labels)
	if res.err != nil || res.status != http.StatusOK {
		p.reg.SetGauge("streampulse_probe_up", helpProbeUp, 0, labels)
		msg := "manifest returned HTTP " + itoa(res.status)
		if res.err != nil {
			msg = res.err.Error()
		}
		p.emit(t.Name, "", alert.Critical, "manifest_fetch", msg)
		return
	}
	p.reg.SetGauge("streampulse_probe_up", helpProbeUp, 1, labels)
	p.recordCache(labels, res.cache)

	if isDASH(t, res.body) {
		p.probeDASH(ctx, t, res)
		return
	}
	p.probeHLS(ctx, t, res)
}

// recordCache exports what the CDN said about this response's freshness. Age
// is the series worth graphing: a step change in it is an edge that stopped
// revalidating, which is visible well before anything times out.
func (p *Prober) recordCache(labels map[string]string, c cacheInfo) {
	if c.HasAge {
		p.reg.SetGauge("streampulse_manifest_age_seconds", helpManifestAge, c.Age.Seconds(), labels)
	}
	switch c.Hit {
	case "HIT", "STALE":
		p.reg.SetGauge("streampulse_manifest_cache_hit", helpCacheHit, 1, labels)
	case "MISS":
		p.reg.SetGauge("streampulse_manifest_cache_hit", helpCacheHit, 0, labels)
	}
}

// isDASH decides which parser a response belongs to. An explicit target type
// wins; otherwise the body decides, which keeps a plain URL working with no
// configuration. Note that a target declared "dash" is not second-guessed
// here: it goes to dash.Parse and comes back with a parse error naming what
// the body actually was, which is more use than "unrecognized".
func isDASH(t config.Target, raw string) bool {
	switch strings.ToLower(t.Type) {
	case "dash":
		return true
	case "hls":
		return false
	}
	return dash.LooksLikeMPD([]byte(raw))
}

func (p *Prober) probeHLS(ctx context.Context, t config.Target, res fetchResult) {
	raw := res.body
	labels := map[string]string{"target": t.Name}

	switch hls.DetectType(raw) {
	case hls.Master:
		master := hls.ParseMaster(raw)
		p.reg.SetGauge("streampulse_variant_count", "Variants declared in the master playlist", float64(len(master.Variants)), labels)
		p.reg.SetGauge("streampulse_rendition_count", "EXT-X-MEDIA renditions declared in the master playlist", float64(len(master.Renditions)), labels)
		if len(master.Variants) == 0 {
			p.emit(t.Name, "", alert.Warning, "master_empty", "master playlist declared no variants")
			return
		}
		for _, f := range masterChecks(p.now().UTC(), t, master) {
			p.record(f)
		}

		variants := master.Variants
		if t.MaxVariants > 0 && len(variants) > t.MaxVariants {
			variants = variants[:t.MaxVariants]
		}
		for _, v := range variants {
			p.probeMedia(ctx, t, resolveURL(t.URL, v.URI), variantLabel(v),
				inspect.Declared{
					Codecs: v.Codecs, Resolution: v.Resolution,
					SeparateKinds: separateKinds(v),
				})
		}

		// Renditions are declared once at the master level, so they are probed
		// once per cycle regardless of how many variants reference them.
		// A rendition declares no CODECS and no RESOLUTION, so there is
		// nothing to compare its media against.
		for _, r := range selectRenditions(t, master) {
			p.probeMedia(ctx, t, resolveURL(t.URL, r.URI), r.Label(), inspect.Declared{})
		}
	case hls.Media:
		// The target is itself a media playlist: reuse the body ProbeTarget
		// already fetched rather than asking the origin for it again.
		p.checkMedia(ctx, t, t.URL, "direct", res, inspect.Declared{})
	default:
		p.emit(t.Name, "", alert.Warning, "unknown_playlist", "response was not a recognizable HLS playlist or DASH MPD")
	}
}

func (p *Prober) probeMedia(ctx context.Context, t config.Target, mediaURL, variant string, d inspect.Declared) {
	p.checkMedia(ctx, t, mediaURL, variant, p.fetch(ctx, t, mediaURL), d)
}

// checkMedia runs the media-playlist path against a response that has already
// been fetched.
//
// It is split from the fetch so that a target which *is* a media playlist is
// not fetched twice per cycle: ProbeTarget has to read the body to tell a
// media playlist from a master one, and re-reading it doubled the request rate
// against the origin for nothing. The metrics are recorded from that same
// response, so media_fetch_seconds for such a target now reports the fetch
// that actually happened rather than a second one made only to measure it.
func (p *Prober) checkMedia(ctx context.Context, t config.Target, mediaURL, variant string, res fetchResult, d inspect.Declared) {
	labels := map[string]string{"target": t.Name, "variant": variant}

	p.reg.SetGauge("streampulse_media_fetch_seconds", "Time to fetch a media playlist", res.dur.Seconds(), labels)
	if res.err != nil || res.status != http.StatusOK {
		p.reg.SetGauge("streampulse_variant_up", "1 if the media playlist is reachable", 0, labels)
		p.emit(t.Name, variant, alert.Critical, "media_fetch", "media playlist fetch failed (HTTP "+itoa(res.status)+")")
		return
	}
	p.reg.SetGauge("streampulse_variant_up", "1 if the media playlist is reachable", 1, labels)
	p.recordCache(labels, res.cache)

	pl := hls.ParseMedia(res.body)
	p.reg.SetGauge("streampulse_stream_live", helpStreamLive, boolGauge(!pl.EndList), labels)
	p.reg.SetGauge("streampulse_media_sequence", helpSequence, float64(pl.MediaSequence), labels)
	p.reg.SetGauge("streampulse_playlist_window_seconds", helpWindow, pl.Duration(), labels)
	p.reg.SetGauge("streampulse_segment_count", helpSegmentCount, float64(len(pl.Segments)), labels)

	for _, f := range p.runChecks(t, mediaURL, variant, pl, res.cache) {
		p.record(f)
	}
	for _, f := range p.keyChecks(ctx, t, mediaURL, variant, pl) {
		p.record(f)
	}

	if t.SegmentSample > 0 && len(pl.Segments) > 0 {
		// A playlist has more than one initialisation section when it changes
		// mid-stream, which happens at a discontinuity; each is fetched once
		// however many segments reference it.
		// The initialisation segment describes the tracks, so it is what gets
		// inspected. A media segment on its own describes nothing.
		for _, mp := range pl.DistinctMaps() {
			// A URI-less EXT-X-MAP is reported by runChecks as the spec
			// violation it is. It must not be probed: resolving an empty
			// reference yields the playlist's own URL, which would fetch
			// successfully and report a broken stream as healthy.
			if mp.URI == "" {
				continue
			}
			offset, _ := mp.Offset()
			initURL := resolveURL(mediaURL, mp.URI)
			p.probeInit(ctx, t, variant, initURL, offset)
			length, _ := mp.Length()
			media := p.inspectMedia(ctx, t, variant, initURL, d, pl.Encrypted(), span{offset, length})
			if n := len(pl.Segments); n > 0 {
				p.capture(ctx, t, variant, initURL, span{offset, length},
					resolveURL(mediaURL, pl.Segments[n-1].URI), media)
			}
		}
		if len(pl.Maps) == 0 && len(pl.Segments) > 0 {
			// Transport stream: no init segment, and a TS segment is
			// self-describing, so the newest one is what gets inspected.
			last := pl.Segments[len(pl.Segments)-1]
			segURL := resolveURL(mediaURL, last.URI)
			media := p.inspectMedia(ctx, t, variant, segURL, d, pl.Encrypted(), span{})
			p.capture(ctx, t, variant, "", span{}, segURL, media)
		}
		urls := make([]string, len(pl.Segments))
		for i, s := range pl.Segments {
			urls[i] = resolveURL(mediaURL, s.URI)
		}
		p.sampleSegments(ctx, t, variant, urls, res.cache)
	}

	// Transport stream analysis stands outside the sampling block: it needs no
	// external binary and no initialisation segment, so tying it to
	// segment_sample would make ts_analysis on its own a silent no-op.
	// A playlist with an EXT-X-MAP is fMP4 and has no transport stream in it.
	if len(pl.Maps) == 0 && len(pl.Segments) > 0 {
		last := pl.Segments[len(pl.Segments)-1]
		p.analyseTransport(ctx, t, variant, resolveURL(mediaURL, last.URI))
	}
}

// sampleSegments fetch-checks the most recent N of the given segment URLs (the
// live edge, where availability problems usually first appear). It takes
// resolved URLs rather than a playlist so that both manifest formats share it.
// cache is the manifest's, not the segment's, and that is the useful one: a
// segment that 404s because we are acting on a minutes-old cached manifest
// naming segments that have since aged out is a different fault from one the
// origin never produced, and the Age header is what tells them apart.
func (p *Prober) sampleSegments(ctx context.Context, t config.Target, variant string, urls []string, cache cacheInfo) {
	labels := map[string]string{"target": t.Name, "variant": variant}
	n := t.SegmentSample
	if n > len(urls) {
		n = len(urls)
	}
	for _, u := range urls[len(urls)-n:] {
		ttfb, status, err := p.probeSegment(ctx, u, 0)
		if err != nil || status >= 400 {
			p.reg.SetGauge("streampulse_segment_available", helpSegmentUp, 0, labels)
			detail := "HTTP " + itoa(status)
			if err != nil {
				detail = err.Error()
			}
			p.emit(t.Name, variant, alert.Critical, "segment_availability",
				"segment not available ("+detail+"): "+u+cache.describe())
			return
		}
		p.reg.SetGauge("streampulse_segment_available", helpSegmentUp, 1, labels)
		p.reg.SetGauge("streampulse_segment_ttfb_seconds", helpSegmentTTFB, ttfb.Seconds(), labels)
	}
}

// --- HTTP helpers ---

// fetchResult is one manifest fetch. The cache headers travel with the body
// because the checks that need them run several calls further down.
type fetchResult struct {
	body   string
	status int
	dur    time.Duration
	cache  cacheInfo
	err    error
}

func (p *Prober) fetch(ctx context.Context, t config.Target, u string) fetchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fetchResult{err: err}
	}
	applyHeaders(req, t)

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return fetchResult{dur: time.Since(start), err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return fetchResult{
		body:   string(b),
		status: resp.StatusCode,
		dur:    time.Since(start),
		cache:  readCache(resp.Header),
		err:    err,
	}
}

// userAgent identifies probe traffic in an operator's access logs. Synthetic
// requests that look like a player are hard to separate from real viewers when
// someone is reading origin logs during an incident.
const userAgent = "StreamPulse/0.1 (synthetic prober)"

// applyHeaders sets the request headers for a target.
//
// NoCache is opt-in rather than the default. Bypassing the edge would measure
// the origin, but viewers do not watch the origin: a stale edge is a real
// outage, and a prober that never sees one is measuring the wrong thing. The
// option exists so a second target can be pointed past the cache deliberately,
// and the two compared.
func applyHeaders(req *http.Request, t config.Target) {
	req.Header.Set("User-Agent", userAgent)
	if t.NoCache {
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
}

// probeSegment measures availability and approximate time-to-first-byte using a
// tiny range request, so it does not download whole segments.
//
// offset is where in the resource to read those two bytes. It is 0 for a whole
// segment and the BYTERANGE offset for an initialisation section that is a
// slice of a larger file: asking at the offset proves the range is actually
// satisfiable, where reading the first two bytes would pass against a file
// truncated before the part we need.
func (p *Prober) probeSegment(ctx context.Context, u string, offset int64) (time.Duration, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Range", "bytes="+i64toa(offset)+"-"+i64toa(offset+1))
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return time.Since(start), 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 2)
	_, _ = io.ReadFull(resp.Body, buf)
	ttfb := time.Since(start)
	_, _ = io.Copy(io.Discard, resp.Body)
	return ttfb, resp.StatusCode, nil
}

// probeInit fetch-checks an initialisation segment.
//
// It is worth a request of its own because its failure mode is invisible to
// every other check: the manifest parses, every media segment is served, and
// playback still cannot start, because a player loads the initialisation
// section first and has nothing to initialise the decoder with. Unlike a media
// segment at the live edge, it is static and should never not be there, so a
// 404 here is unambiguous.
//
// Shared by both manifest formats: EXT-X-MAP and a DASH Initialization are the
// same object under two names, and a stream should not be judged differently
// for saying it in a different dialect.
func (p *Prober) probeInit(ctx context.Context, t config.Target, variant, initURL string, offset int64) {
	if initURL == "" {
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}
	_, status, err := p.probeSegment(ctx, initURL, offset)
	if err != nil || status >= 400 {
		detail := "HTTP " + itoa(status)
		if err != nil {
			detail = err.Error()
		}
		p.reg.SetGauge("streampulse_init_segment_available", helpInitUp, 0, labels)
		p.emit(t.Name, variant, alert.Critical, "init_segment_availability",
			"initialisation segment not available ("+detail+"): "+initURL)
		return
	}
	p.reg.SetGauge("streampulse_init_segment_available", helpInitUp, 1, labels)
}

// probeStreaming asks for a resource without a Range header and reports
// whether the response declared its length, then abandons the body.
//
// The Range request every other probe uses cannot answer this question: a
// range response always carries a length, whatever the origin would have done
// for a whole-resource request. And the body must not be read -- on a segment
// still being produced it would not end until production did.
func (p *Prober) probeStreaming(ctx context.Context, t config.Target, u string) (length int64, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, err
	}
	applyHeaders(req, t)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	// Headers are in; the body is of no interest and may not have been
	// written yet.
	_ = resp.Body.Close()
	return resp.ContentLength, resp.StatusCode, nil
}

// --- finding delivery ---

func (p *Prober) record(f alert.Finding) {
	// Stamped here, once, on the single path every finding leaves by. Setting
	// it at each of the fifty call sites would mean forgetting it at one.
	f.Vantage = p.vantage
	p.notifier.Notify(f)
	p.reg.IncCounter("streampulse_findings_total", "Total findings emitted", map[string]string{
		"target": f.Target, "variant": f.Variant, "check": f.Check, "severity": string(f.Severity),
	})
}

func (p *Prober) emit(target, variant string, sev alert.Severity, check, msg string) {
	p.record(alert.Finding{
		Time: p.now().UTC(), Target: target, Variant: variant,
		Severity: sev, Check: check, Message: msg,
	})
}

// --- small utilities ---

// boolGauge renders a condition as a Prometheus gauge.
func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func variantLabel(v hls.Variant) string {
	if v.Resolution != "" {
		return v.Resolution
	}
	if v.Bandwidth > 0 {
		return itoa(v.Bandwidth) + "bps"
	}
	return "v"
}
