package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/metrics"
)

type capture struct {
	mu       sync.Mutex
	findings []alert.Finding
}

func (c *capture) Notify(f alert.Finding) {
	c.mu.Lock()
	c.findings = append(c.findings, f)
	c.mu.Unlock()
}

func (c *capture) has(check string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

// A master -> media -> segments origin, deliberately containing a
// TARGETDURATION violation and referencing one segment that 404s.
func newFakeOrigin() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(`#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
media.m3u8
`))
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:4.000,
seg0.ts
#EXTINF:9.500,
seg_missing.ts
#EXT-X-ENDLIST
`))
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x47\x40\x00\x10")) // a couple of MPEG-TS-looking bytes
	})
	// seg_missing.ts is intentionally not registered -> 404
	return httptest.NewServer(mux)
}

func TestProbePipeline(t *testing.T) {
	origin := newFakeOrigin()
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)

	target := config.Target{
		Name:          "fake",
		URL:           origin.URL + "/master.m3u8",
		SegmentSample: 2,
	}
	pr.ProbeTarget(context.Background(), target)

	if !cap.has("targetduration_violation") {
		t.Error("expected a targetduration_violation finding")
	}
	if !cap.has("segment_availability") {
		t.Error("expected a segment_availability finding for the 404 segment")
	}
}

func TestProbeUnreachable(t *testing.T) {
	cap := &capture{}
	pr := New(metrics.New(), cap)
	// A closed server address: connection should fail and surface manifest_fetch.
	s := httptest.NewServer(http.NewServeMux())
	url := s.URL + "/master.m3u8"
	s.Close()

	pr.ProbeTarget(context.Background(), config.Target{Name: "down", URL: url})
	if !cap.has("manifest_fetch") {
		t.Error("expected a manifest_fetch finding when the origin is unreachable")
	}
}

// --- EXT-X-MAP: the initialisation section ---

// fmp4Origin serves a master -> media -> fMP4 playlist. Anything named in
// broken 404s.
func fmp4Origin(mediaBody string, broken map[string]bool) (*httptest.Server, *hits) {
	h := &hits{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.add(r.URL.Path)
		switch {
		case r.URL.Path == "/media.m3u8":
			_, _ = w.Write([]byte(mediaBody))
		case broken[r.URL.Path]:
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte("\x00\x00\x00\x18"))
		}
	})
	return httptest.NewServer(mux), h
}

const fmp4Media = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg0.m4s
#EXTINF:4.000,
seg1.m4s
#EXT-X-ENDLIST
`

// The gap this closes: an fMP4 stream whose media segments all serve but whose
// initialisation section is gone. Playback never starts and every other check
// passes.
func TestHLSInitSegmentUnavailable(t *testing.T) {
	origin, _ := fmp4Origin(fmp4Media, map[string]bool{"/init.mp4": true})
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: origin.URL + "/media.m3u8", SegmentSample: 2,
	})

	if !cap.has("init_segment_availability") {
		t.Fatalf("a dead EXT-X-MAP went undetected: %+v", cap.findings)
	}
	if cap.has("segment_availability") {
		t.Error("the media segments were fine; only the init section was not")
	}
}

func TestHLSInitSegmentHealthy(t *testing.T) {
	origin, h := fmp4Origin(fmp4Media, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: origin.URL + "/media.m3u8", SegmentSample: 2,
	})

	if len(cap.findings) != 0 {
		t.Fatalf("a healthy fMP4 stream produced findings: %+v", cap.findings)
	}
	if !h.got("/init.mp4") {
		t.Errorf("the initialisation section was never fetched: %v", h.paths())
	}
}

// One fetch per distinct initialisation section, not one per segment.
func TestInitSegmentFetchedOncePerDistinctMap(t *testing.T) {
	origin, h := fmp4Origin(fmp4Media, nil)
	defer origin.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: origin.URL + "/media.m3u8", SegmentSample: 2,
	})

	n := 0
	for _, p := range h.paths() {
		if p == "/init.mp4" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("init.mp4 fetched %d times for 2 segments, want 1", n)
	}
}

// A BYTERANGE init section is probed at its offset, so a file truncated
// before the part that matters is caught rather than passed.
func TestInitSegmentByteRangeIsProbedAtItsOffset(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			_, _ = w.Write([]byte(strings.Replace(fmp4Media,
				`#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:URI="init.mp4",BYTERANGE="800@1024"`, 1)))
			return
		}
		if r.URL.Path == "/init.mp4" {
			gotRange = r.Header.Get("Range")
		}
		_, _ = w.Write([]byte("\x00\x00"))
	}))
	defer srv.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: srv.URL + "/media.m3u8", SegmentSample: 1,
	})
	if gotRange != "bytes=1024-1025" {
		t.Errorf("Range = %q, want bytes=1024-1025 (the declared offset)", gotRange)
	}
}

// An EXT-X-MAP with no URI is a spec violation that must be reported -- and
// must never be probed, because resolving an empty reference yields the
// playlist's own URL, which fetches fine and would report a broken stream as
// healthy.
func TestMapWithoutURIIsReportedNotProbed(t *testing.T) {
	body := strings.Replace(fmp4Media, `#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:BYTERANGE="800@0"`, 1)
	origin, h := fmp4Origin(body, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "fmp4", URL: origin.URL + "/media.m3u8", SegmentSample: 1,
	})

	if !cap.has("map_missing_uri") {
		t.Fatalf("a URI-less EXT-X-MAP went unreported: %+v", cap.findings)
	}
	if cap.has("init_segment_availability") {
		t.Error("a map with no URI must not be probed as if it had one")
	}
	// A direct media-playlist target is fetched twice per poll as it is: once
	// as the top-level manifest and once as the media playlist. A *third*
	// fetch would mean the empty URI had resolved back to the playlist and
	// been probed as an initialisation section.
	n := 0
	for _, p := range h.paths() {
		if p == "/media.m3u8" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the playlist was fetched %d times, want 2 -- a third means the empty URI resolved back to it", n)
	}
}

// The structural check runs whether or not segments are being sampled.
func TestMapMissingURIReportedWithoutSampling(t *testing.T) {
	body := strings.Replace(fmp4Media, `#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:BYTERANGE="800@0"`, 1)
	origin, _ := fmp4Origin(body, nil)
	defer origin.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{Name: "fmp4", URL: origin.URL + "/media.m3u8"})
	if !cap.has("map_missing_uri") {
		t.Errorf("expected map_missing_uri with sampling off, got %+v", cap.findings)
	}
}
