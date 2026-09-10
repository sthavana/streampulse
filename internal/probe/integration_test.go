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

// The gap this closes: a stream whose video is perfectly healthy but whose
// audio rendition is broken. Before EXT-X-MEDIA was parsed, the prober fetched
// only the variants and reported the stream as fine.
func TestBrokenAudioRenditionIsDetected(t *testing.T) {
	var audioBroken atomic.Bool

	media := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000,\nseg0.ts\n#EXT-X-ENDLIST\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="aac"
video.m3u8
`))
	})
	mux.HandleFunc("/video.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(media))
	})
	mux.HandleFunc("/audio.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if audioBroken.Load() {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(media))
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x47\x40\x00\x10"))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	sink := &capture{}
	pr := New(metrics.New(), sink)
	target := config.Target{Name: "chan", URL: origin.URL + "/master.m3u8", SegmentSample: 1}

	pr.ProbeTarget(context.Background(), target)
	if len(sink.findings) != 0 {
		t.Fatalf("healthy stream produced findings: %+v", sink.findings)
	}

	// Video keeps working; only the audio rendition dies.
	audioBroken.Store(true)
	pr.ProbeTarget(context.Background(), target)

	if !sink.has("media_fetch") {
		t.Fatalf("a broken audio rendition went undetected: %+v", sink.findings)
	}
	var got string
	for _, f := range sink.findings {
		if f.Check == "media_fetch" {
			got = f.Variant
		}
	}
	if got != "audio/aac/English" {
		t.Errorf("finding attributed to variant %q, want audio/aac/English", got)
	}
}

// Renditions are declared once at the master level, so a group shared by every
// rung of the ladder must still be fetched once per cycle, not once per variant.
func TestSharedRenditionProbedOncePerCycle(t *testing.T) {
	var audioHits atomic.Int32

	media := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000,\nseg0.ts\n#EXT-X-ENDLIST\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",URI="audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO="aac"
720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="aac"
360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=400000,RESOLUTION=426x240,AUDIO="aac"
240p.m3u8
`))
	})
	for _, p := range []string{"/720p.m3u8", "/360p.m3u8", "/240p.m3u8"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(media))
		})
	}
	mux.HandleFunc("/audio.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		audioHits.Add(1)
		_, _ = w.Write([]byte(media))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{Name: "chan", URL: origin.URL + "/master.m3u8"})

	if got := audioHits.Load(); got != 1 {
		t.Errorf("audio rendition fetched %d times for 3 variants, want 1", got)
	}
}
