package probe

import (
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
	"streampulse/internal/metrics"
)

// fakeClock drives the cross-poll checks deterministically.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestProber(start time.Time) (*Prober, *fakeClock) {
	clk := &fakeClock{t: start}
	pr := New(metrics.New(), &capture{})
	pr.now = clk.now
	return pr, clk
}

// livePlaylist builds a live media playlist of segs x segDur segments. When
// pdt is non-nil it is attached to the FIRST segment only, which is how many
// packagers emit it -- the case that used to break the staleness check.
func livePlaylist(seq, targetDur, segs int, segDur float64, pdt *time.Time) *hls.MediaPlaylist {
	pl := &hls.MediaPlaylist{Version: 3, TargetDuration: targetDur, MediaSequence: seq}
	for i := 0; i < segs; i++ {
		s := hls.Segment{URI: "seg" + itoa(seq+i) + ".ts", Duration: segDur}
		if i == 0 && pdt != nil {
			t := *pdt
			s.ProgramDateTime = &t
		}
		pl.Segments = append(pl.Segments, s)
	}
	return pl
}

func checks(fs []alert.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Check)
	}
	return out
}

func hasCheck(fs []alert.Finding, name string) bool {
	for _, f := range fs {
		if f.Check == name {
			return true
		}
	}
	return false
}

var target = config.Target{Name: "t", ExpectLive: true}

// --- liveEdgePDT: the projection the staleness check depends on ---

func TestLiveEdgePDTProjectsFromAnchorToEndOfWindow(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 6 segments x 6s, PDT on the first only: the edge is anchor + 36s.
	pl := livePlaylist(100, 6, 6, 6.0, &anchor)

	edge := liveEdgePDT(pl)
	if edge == nil {
		t.Fatal("liveEdgePDT returned nil")
	}
	if want := anchor.Add(36 * time.Second); !edge.Equal(want) {
		t.Errorf("edge = %v, want %v", edge, want)
	}
}

func TestLiveEdgePDTNilWhenNoProgramDateTime(t *testing.T) {
	if edge := liveEdgePDT(livePlaylist(1, 6, 3, 6.0, nil)); edge != nil {
		t.Errorf("expected nil edge with no PDT, got %v", edge)
	}
}

// --- pdt_stale ---

func TestPDTStaleNotFiredAtHealthyLiveEdge(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pl := livePlaylist(100, 6, 6, 6.0, &anchor)
	// Wall clock sits exactly at the projected live edge: nothing is stale.
	now := anchor.Add(36 * time.Second)

	if fs := edgeStalenessCheck(now, target, "v", pl, cacheInfo{}); len(fs) != 0 {
		t.Errorf("expected no findings at a healthy edge, got %v", checks(fs))
	}
}

func TestPDTStaleFiresWhenEdgeGenuinelyBehind(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pl := livePlaylist(100, 6, 6, 6.0, &anchor)
	// A full minute past the edge, well beyond 3 x TARGETDURATION.
	now := anchor.Add(36*time.Second + 60*time.Second)

	fs := edgeStalenessCheck(now, target, "v", pl, cacheInfo{})
	if !hasCheck(fs, "pdt_stale") {
		t.Fatalf("expected pdt_stale, got %v", checks(fs))
	}
}

// --- pdt_not_advancing ---

func TestPDTNotAdvancingSilentOnIdenticalConsecutivePolls(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))

	pl := livePlaylist(100, 6, 6, 6.0, &anchor)
	pr.runChecks(target, "u", "v", pl, cacheInfo{}) // baseline poll

	// Poll again 2s later, faster than the 6s segment duration: the packager has
	// not published a new segment yet, so the identical playlist is correct.
	clk.advance(2 * time.Second)
	fs := pr.runChecks(target, "u", "v", pl, cacheInfo{})

	if hasCheck(fs, "pdt_not_advancing") {
		t.Errorf("pdt_not_advancing fired on an unchanged playlist: %v", checks(fs))
	}
	if hasCheck(fs, "playlist_stalled") {
		t.Errorf("playlist_stalled fired inside the threshold: %v", checks(fs))
	}
}

func TestPDTNotAdvancingFiresWhenWindowMovesButPDTFrozen(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))

	pr.runChecks(target, "u", "v", livePlaylist(100, 6, 6, 6.0, &anchor), cacheInfo{})

	// Sequence advances but the packager re-emits the same PDT: a real fault.
	clk.advance(6 * time.Second)
	fs := pr.runChecks(target, "u", "v", livePlaylist(101, 6, 6, 6.0, &anchor), cacheInfo{})

	if !hasCheck(fs, "pdt_not_advancing") {
		t.Fatalf("expected pdt_not_advancing, got %v", checks(fs))
	}
}

func TestPDTAdvancingNormallyIsSilent(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))

	pr.runChecks(target, "u", "v", livePlaylist(100, 6, 6, 6.0, &anchor), cacheInfo{})

	// A healthy slide: sequence +1 and the anchor PDT moves forward one segment.
	clk.advance(6 * time.Second)
	next := anchor.Add(6 * time.Second)
	fs := pr.runChecks(target, "u", "v", livePlaylist(101, 6, 6, 6.0, &next), cacheInfo{})

	if len(fs) != 0 {
		t.Errorf("expected a clean poll, got %v", checks(fs))
	}
}

// --- playlist_stalled ---

func TestPlaylistStalledFiresPastThreshold(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))
	pl := livePlaylist(100, 6, 6, 6.0, nil)

	pr.runChecks(target, "u", "v", pl, cacheInfo{})

	// Threshold is 3 x TARGETDURATION = 18s; go past it with the same playlist.
	clk.advance(20 * time.Second)
	fs := pr.runChecks(target, "u", "v", pl, cacheInfo{})

	if !hasCheck(fs, "playlist_stalled") {
		t.Fatalf("expected playlist_stalled, got %v", checks(fs))
	}
}

func TestPlaylistStalledSilentInsideThreshold(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))
	pl := livePlaylist(100, 6, 6, 6.0, nil)

	pr.runChecks(target, "u", "v", pl, cacheInfo{})
	clk.advance(10 * time.Second) // inside the 18s threshold
	fs := pr.runChecks(target, "u", "v", pl, cacheInfo{})

	if hasCheck(fs, "playlist_stalled") {
		t.Errorf("playlist_stalled fired inside the threshold: %v", checks(fs))
	}
}

// --- playlist_rollback ---

func TestPlaylistRollbackFiresAndIsNotReportedAsStalled(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))

	pr.runChecks(target, "u", "v", livePlaylist(100, 6, 6, 6.0, nil), cacheInfo{})

	// Origin failover serves an older window.
	clk.advance(6 * time.Second)
	fs := pr.runChecks(target, "u", "v", livePlaylist(90, 6, 6, 6.0, nil), cacheInfo{})

	if !hasCheck(fs, "playlist_rollback") {
		t.Fatalf("expected playlist_rollback, got %v", checks(fs))
	}
	if hasCheck(fs, "playlist_stalled") {
		t.Error("a rollback must not also be reported as a frozen edge")
	}
	for _, f := range fs {
		if f.Check == "playlist_rollback" && f.Severity != alert.Critical {
			t.Errorf("playlist_rollback severity = %v, want critical", f.Severity)
		}
	}
}

func TestRollbackResetsBaselineSoRecoveryIsClean(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, clk := newTestProber(anchor.Add(36 * time.Second))

	pr.runChecks(target, "u", "v", livePlaylist(100, 6, 6, 6.0, nil), cacheInfo{})
	clk.advance(6 * time.Second)
	pr.runChecks(target, "u", "v", livePlaylist(90, 6, 6, 6.0, nil), cacheInfo{}) // rollback

	// The stale origin keeps serving, now advancing normally from 90.
	clk.advance(6 * time.Second)
	fs := pr.runChecks(target, "u", "v", livePlaylist(91, 6, 6, 6.0, nil), cacheInfo{})

	if len(fs) != 0 {
		t.Errorf("expected recovery to be silent, got %v", checks(fs))
	}
}

// --- first poll ---

func TestFirstPollEstablishesBaselineWithoutCrossPollFindings(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr, _ := newTestProber(anchor.Add(36 * time.Second))

	fs := pr.runChecks(target, "u", "v", livePlaylist(100, 6, 6, 6.0, &anchor), cacheInfo{})

	for _, name := range []string{"playlist_stalled", "playlist_rollback", "pdt_not_advancing"} {
		if hasCheck(fs, name) {
			t.Errorf("%s fired on the first poll, with nothing to compare against", name)
		}
	}
}

// --- sparse PDT, as emitted by real packagers ---

// sparsePDTWindow builds the window starting at global segment startIdx, with
// EXT-X-PROGRAM-DATE-TIME attached only every pdtEvery segments of the global
// timeline. This mirrors Unified Streaming, which tags only discontinuity
// boundaries: ~11 PDT tags across a 313-segment window.
func sparsePDTWindow(startIdx, segs, pdtEvery int, segDur float64, epoch time.Time, targetDur int) *hls.MediaPlaylist {
	pl := &hls.MediaPlaylist{Version: 4, TargetDuration: targetDur, MediaSequence: startIdx}
	for i := 0; i < segs; i++ {
		g := startIdx + i
		s := hls.Segment{URI: "seg" + itoa(g) + ".ts", Duration: segDur}
		if g%pdtEvery == 0 {
			t := epoch.Add(time.Duration(float64(g) * segDur * float64(time.Second)))
			s.ProgramDateTime = &t
		}
		pl.Segments = append(pl.Segments, s)
	}
	return pl
}

func TestLiveEdgePDTCorrectWhenAnchorIsMidWindow(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Window covers global segments 1..50; the only anchor inside it is at 40.
	pl := sparsePDTWindow(1, 50, 40, 2.0, epoch, 3)

	edge := liveEdgePDT(pl)
	if edge == nil {
		t.Fatal("liveEdgePDT returned nil")
	}
	// Global segment 50 is last, so the true edge is epoch + 51 segments.
	if want := epoch.Add(time.Duration(51 * 2.0 * float64(time.Second))); !edge.Equal(want) {
		t.Errorf("edge = %v, want %v", edge, want)
	}
}

func TestSparsePDTSlidingWindowDoesNotWarn(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const segDur, pdtEvery, segs = 2.0, 40, 50

	// Start the clock at the live edge of the first window.
	start := epoch.Add(time.Duration(float64(segs) * segDur * float64(time.Second)))
	pr, clk := newTestProber(start)

	// Slide one segment at a time. Throughout, the last PDT *tag* in the window
	// stays pinned at global segment 40, which is exactly what used to trip
	// pdt_not_advancing on every poll.
	for i := 0; i < 8; i++ {
		pl := sparsePDTWindow(i, segs, pdtEvery, segDur, epoch, 3)
		fs := pr.runChecks(target, "u", "v", pl, cacheInfo{})
		if hasCheck(fs, "pdt_not_advancing") {
			t.Fatalf("poll %d: pdt_not_advancing fired on a healthy sparse-PDT stream: %v", i, checks(fs))
		}
		if hasCheck(fs, "pdt_stale") {
			t.Fatalf("poll %d: pdt_stale fired at a healthy live edge: %v", i, checks(fs))
		}
		clk.advance(time.Duration(segDur * float64(time.Second)))
	}
}

func TestSparsePDTFrozenTimelineStillWarns(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const segDur, pdtEvery, segs = 2.0, 40, 50

	start := epoch.Add(time.Duration(float64(segs) * segDur * float64(time.Second)))
	pr, clk := newTestProber(start)

	pr.runChecks(target, "u", "v", sparsePDTWindow(0, segs, pdtEvery, segDur, epoch, 3), cacheInfo{})

	// Sequence advances, but the packager republishes the same PDT timeline:
	// the window slid without the clock moving. A genuine fault.
	clk.advance(time.Duration(segDur * float64(time.Second)))
	frozen := sparsePDTWindow(0, segs, pdtEvery, segDur, epoch, 3)
	frozen.MediaSequence = 1

	fs := pr.runChecks(target, "u", "v", frozen, cacheInfo{})
	if !hasCheck(fs, "pdt_not_advancing") {
		t.Fatalf("expected pdt_not_advancing on a frozen timeline, got %v", checks(fs))
	}
}
