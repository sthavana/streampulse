package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streampulse/internal/config"
	"streampulse/internal/inspect"
	"streampulse/internal/metrics"
)

func withStubFFprobe(t *testing.T, pr *Prober, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	i, err := inspect.New(path)
	if err != nil {
		t.Fatal(err)
	}
	pr.SetInspector(i, 5*time.Second)
}

const fmp4Master = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"
media.m3u8
`

func fmp4Server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fmp4Master))
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fmp4Media))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x00\x00\x00\x18"))
	})
	return httptest.NewServer(mux)
}

const streamsMatch = `cat <<'JSON'
{"streams":[{"codec_name":"h264","codec_type":"video","width":1280,"height":720,"sample_aspect_ratio":"1:1"},
            {"codec_name":"aac","codec_type":"audio","channels":2}]}
JSON`

// The fault nothing else in the tool can see: the manifest is correct, every
// segment serves, and the media is not what was promised.
func TestInspectionCatchesAResolutionLie(t *testing.T) {
	origin := fmp4Server(t)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	withStubFFprobe(t, pr, `cat <<'JSON'
{"streams":[{"codec_name":"h264","codec_type":"video","width":640,"height":360,"sample_aspect_ratio":"1:1"},
            {"codec_name":"aac","codec_type":"audio"}]}
JSON`)

	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/master.m3u8", SegmentSample: 1, Inspect: true,
	})

	if !cap.has("resolution_mismatch") {
		t.Fatalf("a 720p manifest serving 360p went undetected: %+v", cap.findings)
	}
	for _, f := range cap.findings {
		if f.Check == "resolution_mismatch" && !strings.Contains(f.Message, "640x360") {
			t.Errorf("the finding should say what was actually there, got %q", f.Message)
		}
	}
}

func TestInspectionIsSilentWhenTheMediaMatches(t *testing.T) {
	origin := fmp4Server(t)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	withStubFFprobe(t, pr, streamsMatch)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/master.m3u8", SegmentSample: 1, Inspect: true,
	})
	if len(cap.findings) != 0 {
		t.Fatalf("a matching stream produced %+v", cap.findings)
	}
}

// Off by default: the check spawns a process and makes its own fetch.
func TestInspectionIsOptInPerTarget(t *testing.T) {
	origin := fmp4Server(t)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	withStubFFprobe(t, pr, `echo "should not have run" >&2; exit 1`)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/master.m3u8", SegmentSample: 1, // Inspect not set
	})
	if cap.has("media_unreadable") {
		t.Errorf("ffprobe ran on a target that did not ask for it: %+v", cap.findings)
	}
}

// The whole point of the optional dependency: no ffprobe changes nothing else.
func TestNoFFprobeLeavesEveryOtherCheckRunning(t *testing.T) {
	origin := fmp4Server(t)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap) // no SetInspector at all
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/master.m3u8", SegmentSample: 1, Inspect: true,
	})
	if len(cap.findings) != 0 {
		t.Fatalf("a healthy stream with no inspector produced %+v", cap.findings)
	}
	body := scrape(t, metricsOf(pr))
	if !strings.Contains(body, "streampulse_segment_available") {
		t.Error("the ordinary checks should still have run")
	}
}

// Bytes that arrive and cannot be decoded is a stronger statement than a 404.
func TestUnreadableMediaIsCritical(t *testing.T) {
	origin := fmp4Server(t)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	withStubFFprobe(t, pr, `echo "moov atom not found" >&2; exit 1`)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/master.m3u8", SegmentSample: 1, Inspect: true,
	})

	if !cap.has("media_unreadable") {
		t.Fatalf("got %+v, want media_unreadable", cap.findings)
	}
	for _, f := range cap.findings {
		if f.Check == "media_unreadable" && !strings.Contains(f.Message, "moov atom not found") {
			t.Errorf("the finding should carry ffprobe's reason, got %q", f.Message)
		}
	}
}

// ffprobe cannot decode protected content, and its failure looks exactly like
// corruption. Inspecting a DRM stream would mean it permanently failing a
// check it cannot pass.
func TestEncryptedMediaIsNotInspected(t *testing.T) {
	encrypted := strings.Replace(fmp4Media, "#EXT-X-MAP:",
		"#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"\n#EXT-X-MAP:", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(encrypted))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x00\x00"))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	withStubFFprobe(t, pr, `echo "invalid data found when processing input" >&2; exit 1`)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/media.m3u8", SegmentSample: 1, Inspect: true,
	})

	if cap.has("media_unreadable") {
		t.Errorf("an encrypted stream was inspected and failed: %+v", cap.findings)
	}
}

func metricsOf(p *Prober) *metrics.Registry { return p.reg }

// EXT-X-MAP can slice a few kilobytes out of an enormous file -- Apple's fMP4
// example points at 150MB -- so only the declared range may be fetched.
// Handing the URL to ffprobe instead made it download all 150MB and take 24
// seconds, on every poll.
func TestOnlyTheDeclaredByteRangeIsDownloaded(t *testing.T) {
	var gotRange string
	var served int64
	huge := make([]byte, 4<<20)

	mux := http.NewServeMux()
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Replace(fmp4Media,
			`#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:URI="big.mp4",BYTERANGE="1024@2048"`, 1)))
	})
	mux.HandleFunc("/big.mp4", func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" && !strings.HasSuffix(rng, "0-1") {
			gotRange = rng
			w.WriteHeader(http.StatusPartialContent)
			n, _ := w.Write(huge[:1024])
			served += int64(n)
			return
		}
		n, _ := w.Write(huge)
		served += int64(n)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("\x00\x00")) })
	origin := httptest.NewServer(mux)
	defer origin.Close()

	pr := New(metrics.New(), &capture{})
	withStubFFprobe(t, pr, streamsMatch)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: origin.URL + "/media.m3u8", SegmentSample: 1, Inspect: true,
	})

	if gotRange != "bytes=2048-3071" {
		t.Errorf("Range = %q, want bytes=2048-3071 (offset 2048, length 1024)", gotRange)
	}
	if served > 64<<10 {
		t.Errorf("downloaded %d bytes for inspection; the whole resource was fetched", served)
	}
}

// --- transport stream analysis ---

// tsSegment builds a transport stream segment with n continuity breaks
// injected into the video PID.
func tsSegment(breaks int) []byte {
	var out []byte
	packet := func(pid uint16, cc byte, pusi bool, body []byte) {
		p := make([]byte, 188)
		for i := range p {
			p[i] = 0xFF
		}
		p[0] = 0x47
		p[1] = byte(pid >> 8 & 0x1F)
		if pusi {
			p[1] |= 0x40
		}
		p[2] = byte(pid & 0xFF)
		p[3] = 0x10 | (cc & 0x0F)
		copy(p[4:], body)
		out = append(out, p...)
	}
	packet(0x0000, 0, true, []byte{
		0x00, 0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, 0xE0, 0x20, 0x00, 0x00, 0x00, 0x00,
	})
	packet(0x0020, 0, true, []byte{
		0x00, 0x02, 0xB0, 0x12, 0x00, 0x01, 0xC1, 0x00, 0x00, 0xE0, 0x21, 0xF0, 0x00,
		0x1B, 0xE0, 0x21, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00,
	})
	cc := byte(0)
	for i := 0; i < 8; i++ {
		packet(0x0021, cc, i == 0, nil)
		cc = (cc + 1) & 0x0F
		if breaks > 0 {
			cc = (cc + 3) & 0x0F // skip three: packets went missing
			breaks--
		}
	}
	return out
}

func tsOrigin(t *testing.T, segment []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.000,\nseg0.ts\n"))
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(segment)
	})
	return httptest.NewServer(mux)
}

// Packets that went missing are a visible glitch and nothing else in the tool
// sees them: the manifest is right, the segment is the right length, and it
// fetches with a 200.
func TestContinuityErrorsAreReported(t *testing.T) {
	origin := tsOrigin(t, tsSegment(2))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ts", URL: origin.URL + "/media.m3u8", TSAnalysis: true,
	})

	if !cap.has("ts_continuity_errors") {
		t.Fatalf("missing packets went undetected: %+v", cap.findings)
	}
	for _, f := range cap.findings {
		if f.Check == "ts_continuity_errors" && !strings.Contains(f.Message, "0x0021") {
			t.Errorf("the finding should name the PID that lost them, got %q", f.Message)
		}
	}
}

func TestCleanTransportStreamIsSilent(t *testing.T) {
	origin := tsOrigin(t, tsSegment(0))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ts", URL: origin.URL + "/media.m3u8", TSAnalysis: true,
	})
	if len(cap.findings) != 0 {
		t.Fatalf("a clean transport stream produced %+v", cap.findings)
	}
}

// It needs no ffprobe and no initialisation segment, so tying it to
// segment_sample would make the flag on its own a silent no-op.
func TestTSAnalysisRunsWithoutSegmentSampling(t *testing.T) {
	origin := tsOrigin(t, tsSegment(3))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap) // no inspector at all
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ts", URL: origin.URL + "/media.m3u8", TSAnalysis: true, // SegmentSample 0
	})
	if !cap.has("ts_continuity_errors") {
		t.Errorf("ts_analysis alone produced %+v", cap.findings)
	}
}

// An fMP4 stream has no transport stream in it. Reporting that absence as a
// fault would flag every DASH and every modern HLS stream.
func TestFMP4IsNotAnalysedAsTransportStream(t *testing.T) {
	origin, _ := fmp4Origin(fmp4Media, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: origin.URL + "/media.m3u8", TSAnalysis: true, SegmentSample: 1,
	})
	for _, f := range cap.findings {
		if strings.HasPrefix(f.Check, "ts_") {
			t.Errorf("an fMP4 stream produced %s: %s", f.Check, f.Message)
		}
	}
}

func TestOffByDefault(t *testing.T) {
	origin := tsOrigin(t, tsSegment(5))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ts", URL: origin.URL + "/media.m3u8", // TSAnalysis not set
	})
	for _, f := range cap.findings {
		if strings.HasPrefix(f.Check, "ts_") {
			t.Errorf("analysis ran on a target that did not ask for it: %s", f.Check)
		}
	}
}

// A CDN serving something that is not a transport stream at all.
func TestNonTransportStreamSegmentIsReported(t *testing.T) {
	origin := tsOrigin(t, []byte("<html><body>404</body></html>"))
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "ts", URL: origin.URL + "/media.m3u8", TSAnalysis: true,
	})
	if !cap.has("ts_sync_loss") {
		t.Errorf("got %+v, want ts_sync_loss", cap.findings)
	}
}
