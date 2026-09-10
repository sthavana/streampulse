package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/metrics"
)

// Covers the seam between the prober and the tracker: the prober re-emits a
// finding on every poll, and the tracker must collapse that into one open and
// one resolve. Exercised end to end over HTTP against a switchable origin.
func TestProberFindingsAreDeduplicatedIntoIncidents(t *testing.T) {
	var broken atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nmedia.m3u8\n"))
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("\x47\x40\x00\x10"))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	sink := &capture{}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := alert.NewTracker(alert.TrackerConfig{ResolveAfter: 30 * time.Second}, sink, nil)
	tr.SetClock(clk.now)

	pr := New(metrics.New(), tr)
	pr.now = clk.now
	target := config.Target{Name: "origin", URL: origin.URL + "/master.m3u8", SegmentSample: 1}

	poll := func(n int) {
		for i := 0; i < n; i++ {
			pr.ProbeTarget(context.Background(), target)
			clk.advance(2 * time.Second)
			tr.Sweep()
		}
	}

	poll(3) // healthy
	if got := len(sink.findings); got != 0 {
		t.Fatalf("healthy origin produced %d notifications, want 0", got)
	}

	broken.Store(true)
	poll(10) // ten failing polls
	if got := len(sink.findings); got != 1 {
		t.Fatalf("ten failing polls produced %d notifications, want 1: %+v", got, sink.findings)
	}
	if got := sink.findings[0]; got.Status != alert.Firing || got.Check != "segment_availability" {
		t.Fatalf("first notification = %+v, want a firing segment_availability", got)
	}

	broken.Store(false)
	poll(20) // recover, then let the incident expire
	if got := len(sink.findings); got != 2 {
		t.Fatalf("after recovery got %d notifications, want 2: %+v", got, sink.findings)
	}
	res := sink.findings[1]
	if res.Status != alert.Resolved {
		t.Errorf("second notification status = %q, want %q", res.Status, alert.Resolved)
	}
	if res.Count != 10 {
		t.Errorf("resolved Count = %d, want the 10 observations", res.Count)
	}
}
