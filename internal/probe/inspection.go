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
	d inspect.Declared, encrypted bool, byteRange span) *inspect.Media {

	if !t.Inspect || !p.inspector.Available() || url == "" || encrypted {
		return nil
	}
	labels := map[string]string{"target": t.Name, "variant": variant}

	ictx, cancel := context.WithTimeout(ctx, p.inspectFor)
	defer cancel()

	start := time.Now()
	path, err := p.download(ictx, t, url, byteRange)
	if err != nil {
		// A fetch failure here is the segment checks' business, not this
		// one's; reporting it twice under two names helps nobody.
		return nil
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
		return nil
	}
	p.reg.SetGauge("streampulse_media_readable", helpMediaReadable, 1, labels)
	p.reg.SetGauge("streampulse_media_streams", helpMediaStreams, float64(len(media.Streams)), labels)

	for _, diff := range inspect.Compare(d, media) {
		p.emit(t.Name, variant, alert.Warning, diff.Kind, diff.Detail)
	}
	return media
}

// capture pulls one frame and one audio level out of the newest segment, for
// the picture and the meter in the web UI.
//
// It has to concatenate the initialisation segment with a media segment before
// anything can be decoded: an fMP4 media segment carries samples and no
// description of them, so on its own it is undecodable. A transport stream
// segment is self-describing and needs no prefix.
func (p *Prober) capture(ctx context.Context, t config.Target, variant string,
	initURL string, initRange span, segmentURL string, media *inspect.Media) {

	if !t.Thumbnails || !p.inspector.CanCapture() || segmentURL == "" || media == nil {
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}

	cctx, cancel := context.WithTimeout(ctx, p.inspectFor)
	defer cancel()

	path, err := p.download(cctx, t, segmentURL, span{})
	if err != nil {
		return
	}
	defer os.Remove(path)

	if initURL != "" {
		if err := p.prefix(cctx, t, initURL, initRange, path); err != nil {
			return
		}
	}

	hasVideo, hasAudio := media.Video() != nil, media.Audio() != nil

	frame := inspect.Frame{At: p.now().UTC()}
	if hasVideo {
		if jpeg, err := p.inspector.Thumbnail(cctx, path, 320); err == nil {
			frame.JPEG = jpeg
		}
	}

	content, err := p.inspector.Analyse(cctx, path, hasVideo, hasAudio)
	if err == nil {
		frame.MeanDBFS, frame.PeakDBFS, frame.HasAudio = content.MeanDBFS, content.PeakDBFS, content.HasAudio
		frame.BlackSeconds, frame.FreezeSeconds = content.BlackSeconds, content.FreezeSeconds
		if content.HasAudio {
			p.reg.SetGauge("streampulse_audio_peak_dbfs", helpAudioPeak, content.PeakDBFS, labels)
			p.reg.SetGauge("streampulse_audio_mean_dbfs", helpAudioMean, content.MeanDBFS, labels)
		}
		if hasVideo {
			p.reg.SetGauge("streampulse_black_seconds", helpBlackSeconds, content.BlackSeconds, labels)
			p.reg.SetGauge("streampulse_freeze_seconds", helpFreezeSeconds, content.FreezeSeconds, labels)
		}
		p.contentChecks(t, variant, content)
	}
	if frame.JPEG != nil || frame.HasAudio {
		p.frames.Put(inspect.FrameKey(t.Name, variant), frame)
	}
}

// contentChecks turns the measurements into findings.
//
// All three are warnings rather than critical, and the thresholds are
// generous, because content is allowed to be black, frozen and silent. A fade
// at an ad boundary, a slate between programmes, a pause in dialogue: each
// looks exactly like the fault it is not. The thresholds separate them, and
// alerting.for_seconds adds the second layer if a stream needs it -- damping
// belongs in one place, and that place is already the tracker.
func (p *Prober) contentChecks(t config.Target, variant string, c *inspect.Content) {
	now := p.now().UTC()
	if c.HasVideo && c.Seconds > 0 {
		if f := c.Fraction(c.BlackSeconds); p.blackFor > 0 && f >= p.blackFor {
			p.record(finding(now, t, variant, alert.Warning, "black_frames",
				"video is black for "+ftoa(c.BlackSeconds)+"s of a "+ftoa(c.Seconds)+
					"s segment ("+pct(f)+")"))
		}
		// Off unless asked for. A static picture is a fault on a news channel
		// and the entire programme on a slate or a test pattern, and nothing
		// in the segment distinguishes them -- only whoever knows the channel
		// can. Unified Streaming's demo, which is colour bars, reads as
		// frozen for 1.4s of every 1.92s segment, and is perfectly healthy.
		if f := c.Fraction(c.FreezeSeconds); p.freezeFor > 0 && f >= p.freezeFor {
			p.record(finding(now, t, variant, alert.Warning, "frozen_video",
				"video is frozen for "+ftoa(c.FreezeSeconds)+"s of a "+ftoa(c.Seconds)+
					"s segment ("+pct(f)+")"))
		}
	}
	// Digital silence is the one audio judgement worth making without knowing
	// the programme: below -60 dBFS there is nothing there at all.
	if c.HasAudio && c.PeakDBFS <= silenceFloor {
		p.record(finding(now, t, variant, alert.Warning, "silent_audio",
			"audio peaks at "+ftoa(c.PeakDBFS)+" dBFS across the sampled segment, which is silence"))
	}
}

// silenceFloor is where "quiet" stops and "nothing" starts.
const silenceFloor = -60

func pct(f float64) string { return ftoa(f*100) + "%" }

// prefix puts the initialisation segment in front of the media segment, in
// place, so ffmpeg is handed one decodable file.
func (p *Prober) prefix(ctx context.Context, t config.Target, initURL string, r span, mediaPath string) error {
	initPath, err := p.download(ctx, t, initURL, r)
	if err != nil {
		return err
	}
	defer os.Remove(initPath)

	init, err := os.ReadFile(initPath)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(mediaPath)
	if err != nil {
		return err
	}
	return os.WriteFile(mediaPath, append(init, body...), 0o600)
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
	media := p.inspectMedia(ctx, t, variant, r.InitURI(), inspect.Declared{
		Codecs: r.Codecs, Width: r.Width, Height: r.Height,
	}, len(r.ContentProtections) > 0, span{})

	if segs := r.SegmentsAt(p.now().UTC()); len(segs) > 0 {
		p.capture(ctx, t, variant, r.InitURI(), span{}, segs[len(segs)-1].URI, media)
	}
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
