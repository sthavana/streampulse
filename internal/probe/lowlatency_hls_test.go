package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

// llPlaylist is a healthy low-latency playlist: parts inside their target, a
// hold-back of three of them, blocking reload declared, one independent part.
func llPlaylist(extra string) string {
	return `#EXTM3U
#EXT-X-VERSION:9
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:1547
#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=1.020
#EXT-X-PART-INF:PART-TARGET=0.34000
` + extra + `#EXTINF:4.00000,
seg1547.m4s
#EXT-X-PART:DURATION=0.34000,URI="seg1548.0.m4s",INDEPENDENT=YES
#EXT-X-PART:DURATION=0.34000,URI="seg1548.1.m4s"
`
}

func llCheck(t *testing.T, body string) []alert.Finding {
	t.Helper()
	pl := hls.ParseMedia(body)
	return lowLatencyChecks(time.Now(), config.Target{Name: "ll"}, "v", pl)
}

// The one that keeps the rest honest.
func TestHealthyLowLatencyPlaylistIsSilent(t *testing.T) {
	if got := llCheck(t, llPlaylist("")); len(got) != 0 {
		t.Fatalf("a well-formed low-latency playlist produced findings: %+v", got)
	}
}

// An ordinary playlist must not acquire low-latency checks it cannot satisfy.
func TestOrdinaryPlaylistGetsNoLowLatencyChecks(t *testing.T) {
	body := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXTINF:6.00000,
seg.ts
`
	if got := llCheck(t, body); len(got) != 0 {
		t.Fatalf("a standard playlist was held to low-latency rules: %+v", got)
	}
}

func TestPartLongerThanPartTarget(t *testing.T) {
	body := strings.Replace(llPlaylist(""),
		`#EXT-X-PART:DURATION=0.34000,URI="seg1548.1.m4s"`,
		`#EXT-X-PART:DURATION=0.90000,URI="seg1548.1.m4s"`, 1)

	got := llCheck(t, body)
	if !hasCheck(got, "part_target_violation") {
		t.Fatalf("a part at nearly three times PART-TARGET went unreported: %v", checks(got))
	}
}

// A few milliseconds over a 340ms target is the packager's frame arithmetic,
// not a fault, and reporting it every poll would make the check worthless.
func TestPartMarginallyOverTargetIsSilent(t *testing.T) {
	body := strings.Replace(llPlaylist(""),
		`#EXT-X-PART:DURATION=0.34000,URI="seg1548.1.m4s"`,
		`#EXT-X-PART:DURATION=0.35000,URI="seg1548.1.m4s"`, 1)

	if got := llCheck(t, body); hasCheck(got, "part_target_violation") {
		t.Fatalf("10ms of rounding was reported as a violation: %v", checks(got))
	}
}

func TestPartsWithoutPartTarget(t *testing.T) {
	body := strings.Replace(llPlaylist(""), "#EXT-X-PART-INF:PART-TARGET=0.34000\n", "", 1)

	if got := llCheck(t, body); !hasCheck(got, "part_target_missing") {
		t.Fatalf("parts published with no PART-TARGET went unreported: %v", checks(got))
	}
}

// Publishing parts without offering blocking reload builds half the mechanism
// and pays none of the latency back.
func TestPartsWithoutBlockingReload(t *testing.T) {
	body := strings.Replace(llPlaylist(""),
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=1.020",
		"#EXT-X-SERVER-CONTROL:PART-HOLD-BACK=1.020", 1)

	if got := llCheck(t, body); !hasCheck(got, "blocking_reload_undeclared") {
		t.Fatalf("a part-publishing playlist with no blocking reload passed: %v", checks(got))
	}
}

// The spec requires at least three part durations. Below that a player is
// playing content whose successor may not exist yet.
func TestPartHoldBackBelowThreeParts(t *testing.T) {
	body := strings.Replace(llPlaylist(""), "PART-HOLD-BACK=1.020", "PART-HOLD-BACK=0.500", 1)

	got := llCheck(t, body)
	if !hasCheck(got, "part_hold_back_too_small") {
		t.Fatalf("a hold-back of 1.5 parts went unreported: %v", checks(got))
	}
	for _, f := range got {
		if f.Check == "part_hold_back_too_small" && !strings.Contains(f.Message, "1.0s") {
			t.Errorf("message does not say what the minimum is: %q", f.Message)
		}
	}
}

// Exactly three is compliant, and a check that fires on the boundary fires on
// every correctly configured stream.
func TestPartHoldBackOfExactlyThreePartsIsSilent(t *testing.T) {
	body := strings.Replace(llPlaylist(""), "PART-HOLD-BACK=1.020", "PART-HOLD-BACK=1.020", 1)
	if got := llCheck(t, body); hasCheck(got, "part_hold_back_too_small") {
		t.Fatalf("three exact part durations were reported as too small: %v", checks(got))
	}
}

func TestPartHoldBackMissing(t *testing.T) {
	body := strings.Replace(llPlaylist(""),
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=1.020",
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES", 1)

	if got := llCheck(t, body); !hasCheck(got, "part_hold_back_missing") {
		t.Fatalf("a missing PART-HOLD-BACK went unreported: %v", checks(got))
	}
}

// A player joining can only start on a part that begins with an IDR frame.
func TestNoIndependentPartIsReported(t *testing.T) {
	body := strings.Replace(llPlaylist(""), `,INDEPENDENT=YES`, ``, 1)

	got := llCheck(t, body)
	if !hasCheck(got, "no_independent_part") {
		t.Fatalf("a window of parts with no join point went unreported: %v", checks(got))
	}
	for _, f := range got {
		if f.Check == "no_independent_part" && f.Severity != alert.Info {
			t.Errorf("severity = %s, want info: it costs a join, it does not break playback", f.Severity)
		}
	}
}

// --- the behavioural half ---

// llOrigin serves a low-latency playlist. When block is true it holds a
// request carrying _HLS_msn for the given delay, the way a conforming origin
// holds one until the part exists.
func llHLSOrigin(t *testing.T, block bool, delay time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var blocking int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("_HLS_msn") != "" {
			atomic.AddInt32(&blocking, 1)
			if block {
				time.Sleep(delay)
			}
		}
		_, _ = w.Write([]byte(llPlaylist("")))
	}))
	return srv, &blocking
}

// The failure this exists for: an origin that advertises blocking reload and
// answers immediately. Nothing else can see it -- the playlist is valid, every
// part serves, players play, and every one of them is back to polling.
func TestBlockingReloadNotHonoured(t *testing.T) {
	srv, hits := llHLSOrigin(t, false, 0)
	defer srv.Close()

	pr, _ := newTestProber(time.Now())
	pl := hls.ParseMedia(llPlaylist(""))
	got := pr.blockingReloadCheck(context.Background(), config.Target{Name: "ll"}, srv.URL+"/m.m3u8", "v", pl)

	if !hasCheck(got, "blocking_reload_missing") {
		t.Fatalf("an origin ignoring _HLS_msn was not reported: %v", checks(got))
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("the blocking request was made %d times, want 1", *hits)
	}
}

// An origin that does hold the request must be silent, or the check reports
// every correctly implemented low-latency stream.
func TestBlockingReloadHonouredIsSilent(t *testing.T) {
	srv, _ := llHLSOrigin(t, true, 300*time.Millisecond)
	defer srv.Close()

	pr, _ := newTestProber(time.Now())
	pl := hls.ParseMedia(llPlaylist(""))
	got := pr.blockingReloadCheck(context.Background(), config.Target{Name: "ll"}, srv.URL+"/m.m3u8", "v", pl)

	if len(got) != 0 {
		t.Fatalf("an origin that held the request for most of a part was reported: %v", checks(got))
	}
}

// Nothing to honour: a stream that never claimed blocking reload must not be
// probed for it, and a standard playlist must not be probed at all.
func TestBlockingReloadIsNotProbedWhenUndeclared(t *testing.T) {
	srv, hits := llHLSOrigin(t, false, 0)
	defer srv.Close()
	pr, _ := newTestProber(time.Now())

	undeclared := hls.ParseMedia(strings.Replace(llPlaylist(""), "CAN-BLOCK-RELOAD=YES,", "", 1))
	if got := pr.blockingReloadCheck(context.Background(), config.Target{Name: "ll"}, srv.URL+"/m.m3u8", "v", undeclared); len(got) != 0 {
		t.Errorf("a stream that never claimed blocking reload was reported: %v", checks(got))
	}

	ordinary := hls.ParseMedia("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg.ts\n")
	if got := pr.blockingReloadCheck(context.Background(), config.Target{Name: "ll"}, srv.URL+"/m.m3u8", "v", ordinary); len(got) != 0 {
		t.Errorf("a standard playlist was probed for blocking reload: %v", checks(got))
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("%d blocking requests were made when none should have been", n)
	}
}

// The request has to name a part that does not exist yet, or a conforming
// origin answers at once and the check reports every healthy stream.
func TestBlockingRequestAsksForTheNextPart(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("_HLS_msn") != "" {
			gotQuery = r.URL.RawQuery
			time.Sleep(300 * time.Millisecond)
		}
		_, _ = w.Write([]byte(llPlaylist("")))
	}))
	defer srv.Close()

	pr, _ := newTestProber(time.Now())
	pl := hls.ParseMedia(llPlaylist(""))
	pr.blockingReloadCheck(context.Background(), config.Target{Name: "ll"}, srv.URL+"/m.m3u8", "v", pl)

	// One segment listed from media sequence 1547, and two parts published.
	if !strings.Contains(gotQuery, "_HLS_msn=1548") || !strings.Contains(gotQuery, "_HLS_part=2") {
		t.Errorf("query = %q, want the next unpublished part (msn 1548, part 2)", gotQuery)
	}
}

// Token-authenticated origins carry a query string already, and replacing it
// turns this check into an authentication failure.
func TestBlockingQueryPreservesAnExistingQueryString(t *testing.T) {
	if got := blockingQuery("https://cdn/x.m3u8?token=abc", 5, 2); got != "&_HLS_msn=5&_HLS_part=2" {
		t.Errorf("query = %q, want it appended with &", got)
	}
	if got := blockingQuery("https://cdn/x.m3u8", 5, 2); got != "?_HLS_msn=5&_HLS_part=2" {
		t.Errorf("query = %q, want it started with ?", got)
	}
}
