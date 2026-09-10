package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
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
