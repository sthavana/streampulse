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
	"streampulse/internal/metrics"
)

// Help text for the series both manifest formats emit. Registry.register keeps
// whichever help text arrives first, so the HLS and DASH paths declaring the
// same series differently would silently ship whichever ran first.
const (
	helpProbeUp       = "1 if the target manifest is reachable"
	helpManifestFetch = "Time to fetch the top-level manifest"
	helpSequence      = "Live-edge sequence (EXT-X-MEDIA-SEQUENCE, or the newest DASH segment number)"
	helpWindow        = "Length of the live/DVR window in seconds"
	helpSegmentCount  = "Segments in the current window"
	helpSegmentUp     = "1 if a sampled segment is fetchable"
	helpSegmentTTFB   = "Time-to-first-byte for a sampled segment"
)

type Prober struct {
	client   *http.Client
	reg      *metrics.Registry
	notifier alert.Notifier
	now      func() time.Time // injectable so the cross-poll checks are testable

	mu    sync.Mutex
	state map[string]*plState // keyed by media-playlist URL
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
		client:   &http.Client{Timeout: 15 * time.Second},
		reg:      reg,
		notifier: n,
		now:      time.Now,
		state:    make(map[string]*plState),
	}
}

// ProbeTarget runs one full probe cycle for a target.
func (p *Prober) ProbeTarget(ctx context.Context, t config.Target) {
	labels := map[string]string{"target": t.Name}

	raw, dur, status, err := p.fetch(ctx, t.URL)
	p.reg.SetGauge("streampulse_manifest_fetch_seconds", helpManifestFetch, dur.Seconds(), labels)
	if err != nil || status != http.StatusOK {
		p.reg.SetGauge("streampulse_probe_up", helpProbeUp, 0, labels)
		msg := "manifest returned HTTP " + itoa(status)
		if err != nil {
			msg = err.Error()
		}
		p.emit(t.Name, "", alert.Critical, "manifest_fetch", msg)
		return
	}
	p.reg.SetGauge("streampulse_probe_up", helpProbeUp, 1, labels)

	if isDASH(t, raw) {
		p.probeDASH(ctx, t, raw)
		return
	}
	p.probeHLS(ctx, t, raw)
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

func (p *Prober) probeHLS(ctx context.Context, t config.Target, raw string) {
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
			p.probeMedia(ctx, t, resolveURL(t.URL, v.URI), variantLabel(v))
		}

		// Renditions are declared once at the master level, so they are probed
		// once per cycle regardless of how many variants reference them.
		for _, r := range selectRenditions(t, master) {
			p.probeMedia(ctx, t, resolveURL(t.URL, r.URI), r.Label())
		}
	case hls.Media:
		p.probeMedia(ctx, t, t.URL, "direct")
	default:
		p.emit(t.Name, "", alert.Warning, "unknown_playlist", "response was not a recognizable HLS playlist or DASH MPD")
	}
}

func (p *Prober) probeMedia(ctx context.Context, t config.Target, mediaURL, variant string) {
	labels := map[string]string{"target": t.Name, "variant": variant}

	raw, dur, status, err := p.fetch(ctx, mediaURL)
	p.reg.SetGauge("streampulse_media_fetch_seconds", "Time to fetch a media playlist", dur.Seconds(), labels)
	if err != nil || status != http.StatusOK {
		p.reg.SetGauge("streampulse_variant_up", "1 if the media playlist is reachable", 0, labels)
		p.emit(t.Name, variant, alert.Critical, "media_fetch", "media playlist fetch failed (HTTP "+itoa(status)+")")
		return
	}
	p.reg.SetGauge("streampulse_variant_up", "1 if the media playlist is reachable", 1, labels)

	pl := hls.ParseMedia(raw)
	p.reg.SetGauge("streampulse_media_sequence", helpSequence, float64(pl.MediaSequence), labels)
	p.reg.SetGauge("streampulse_playlist_window_seconds", helpWindow, pl.Duration(), labels)
	p.reg.SetGauge("streampulse_segment_count", helpSegmentCount, float64(len(pl.Segments)), labels)

	for _, f := range p.runChecks(t, mediaURL, variant, pl) {
		p.record(f)
	}
	for _, f := range p.keyChecks(ctx, t, mediaURL, variant, pl) {
		p.record(f)
	}

	if t.SegmentSample > 0 && len(pl.Segments) > 0 {
		urls := make([]string, len(pl.Segments))
		for i, s := range pl.Segments {
			urls[i] = resolveURL(mediaURL, s.URI)
		}
		p.sampleSegments(ctx, t, variant, urls)
	}
}

// sampleSegments fetch-checks the most recent N of the given segment URLs (the
// live edge, where availability problems usually first appear). It takes
// resolved URLs rather than a playlist so that both manifest formats share it.
func (p *Prober) sampleSegments(ctx context.Context, t config.Target, variant string, urls []string) {
	labels := map[string]string{"target": t.Name, "variant": variant}
	n := t.SegmentSample
	if n > len(urls) {
		n = len(urls)
	}
	for _, u := range urls[len(urls)-n:] {
		ttfb, status, err := p.probeSegment(ctx, u)
		if err != nil || status >= 400 {
			p.reg.SetGauge("streampulse_segment_available", helpSegmentUp, 0, labels)
			detail := "HTTP " + itoa(status)
			if err != nil {
				detail = err.Error()
			}
			p.emit(t.Name, variant, alert.Critical, "segment_availability", "segment not available ("+detail+"): "+u)
			return
		}
		p.reg.SetGauge("streampulse_segment_available", helpSegmentUp, 1, labels)
		p.reg.SetGauge("streampulse_segment_ttfb_seconds", helpSegmentTTFB, ttfb.Seconds(), labels)
	}
}

// --- HTTP helpers ---

func (p *Prober) fetch(ctx context.Context, u string) (body string, dur time.Duration, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, 0, err
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return "", time.Since(start), 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), time.Since(start), resp.StatusCode, err
}

// probeSegment measures availability and approximate time-to-first-byte using a
// tiny range request, so it does not download whole segments.
func (p *Prober) probeSegment(ctx context.Context, u string) (time.Duration, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Range", "bytes=0-1")
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

// --- finding delivery ---

func (p *Prober) record(f alert.Finding) {
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
