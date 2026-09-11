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
