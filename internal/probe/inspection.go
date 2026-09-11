package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
	"streampulse/internal/hls"
	"streampulse/internal/inspect"
)

// inspectMedia opens the media and reports where it disagrees with the
// manifest.
//
// Encrypted content is skipped rather than reported. ffprobe can read the
// container of a protected stream but not decode it, and the errors it returns
// look exactly like corruption -- reporting those would mean every DRM stream
// permanently failing a check it cannot pass.
func (p *Prober) inspectMedia(ctx context.Context, t config.Target, variant, url string,
	d inspect.Declared, encrypted bool, byteRange span) {

	if !t.Inspect || !p.inspector.Available() || url == "" || encrypted {
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}

	ictx, cancel := context.WithTimeout(ctx, p.inspectFor)
	defer cancel()

	start := time.Now()
	path, err := p.download(ictx, t, url, byteRange)
	if err != nil {
		// A fetch failure here is the segment checks' business, not this
		// one's; reporting it twice under two names helps nobody.
		return
	}
	defer os.Remove(path)

	media, err := p.inspector.Probe(ictx, path)
	p.reg.SetGauge("streampulse_inspect_seconds", helpInspectSeconds, time.Since(start).Seconds(), labels)
	if err != nil {
		p.reg.SetGauge("streampulse_media_readable", helpMediaReadable, 0, labels)
		// The media could not be read at all. That is a stronger statement
		// than a segment 404: the bytes arrived and are not usable.
		p.emit(t.Name, variant, alert.Critical, "media_unreadable",
			"ffprobe could not read the media: "+err.Error()+" ("+url+")")
		return
	}
	p.reg.SetGauge("streampulse_media_readable", helpMediaReadable, 1, labels)
	p.reg.SetGauge("streampulse_media_streams", helpMediaStreams, float64(len(media.Streams)), labels)

	for _, diff := range inspect.Compare(d, media) {
		p.emit(t.Name, variant, alert.Warning, diff.Kind, diff.Detail)
	}
}

// inspectRepresentation is the DASH entry point: a representation states its
// codecs and dimensions directly, so there is more to compare than in HLS.
//
// Only template-addressed representations are inspected. A SegmentBase one is
// a single indexed file whose initialisation is a byte range we would have to
// read the sidx to find, and guessing at it would mean handing ffprobe the
// front of a whole movie.
func (p *Prober) inspectRepresentation(ctx context.Context, t config.Target,
	r *dash.Representation, variant string) {

	if r.Addressing() == dash.AddressingBase {
		return
	}
	p.inspectMedia(ctx, t, variant, r.InitURI(), inspect.Declared{
		Codecs: r.Codecs, Width: r.Width, Height: r.Height,
	}, len(r.ContentProtections) > 0, span{})
}

// span is a byte range of a resource, as EXT-X-MAP BYTERANGE states one.
type span struct {
	offset, length int64
}

// maxInspectBytes caps an unranged download. A transport stream segment is a
// couple of megabytes and an initialisation segment a few kilobytes; anything
// far larger is a resource we have misidentified, and reading all of it would
// cost more than the check is worth.
const maxInspectBytes = 16 << 20

// download fetches the bytes to inspect into a temporary file, through the
// prober's own HTTP client so the request carries the same headers and
// timeouts as every other fetch.
func (p *Prober) download(ctx context.Context, t config.Target, url string, r span) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	applyHeaders(req, t)
	if r.length > 0 {
		req.Header.Set("Range", "bytes="+i64toa(r.offset)+"-"+i64toa(r.offset+r.length-1))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "streampulse-inspect-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, maxInspectBytes))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// separateKinds lists the track kinds a variant declares in CODECS but does
// not carry itself, because a rendition group supplies them.
//
// This is the normal shape of modern HLS, not an edge case: Apple's own fMP4
// example declares avc1 and mp4a on a variant whose segments hold video only,
// with the audio in an EXT-X-MEDIA group. Without this the audio check fires
// on every demuxed stream, which was the first thing real ffprobe reported.
func separateKinds(v hls.Variant) []string {
	var out []string
	if v.AudioGroup != "" {
		out = append(out, "audio")
	}
	if v.VideoGroup != "" {
		out = append(out, "video")
	}
	return out
}
