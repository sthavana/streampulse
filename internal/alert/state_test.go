package alert

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stateTracker(t *testing.T, dir string, clk *clock) (*Tracker, *sink) {
	t.Helper()
	sink := &sink{}
	tr := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, sink, nil)
	tr.SetClock(clk.now)
	tr.SetStateFile(filepath.Join(dir, "state.json"))
	return tr, sink
}

func fault(target, check string) Finding {
	return Finding{Target: target, Variant: "v", Check: check, Severity: Critical, Message: "broken"}
}

// The whole point: a redeploy must not re-announce a fault everyone has
// already been paged about.
func TestRestartDoesNotReannounce(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	before, sink1 := stateTracker(t, dir, clk)
	before.Notify(fault("chan1", "playlist_stalled"))
	if sink1.len() != 1 {
		t.Fatalf("expected one firing notification, got %+v", sink1.fs)
	}
	before.Sweep() // writes the snapshot
	if err := before.SaveState(); err != nil {
		t.Fatal(err)
	}

	// Restart: 20 seconds of downtime, and the fault is still there.
	clk.advance(20 * time.Second)
	after, sink2 := stateTracker(t, dir, clk)
	n, err := after.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored %d incidents, want 1", n)
	}

	after.Notify(fault("chan1", "playlist_stalled"))
	if sink2.len() != 0 {
		t.Errorf("re-announced a known fault after restart: %+v", sink2.fs)
	}
}

// Without persistence the same sequence pages twice, which is the behaviour
// being fixed. If this ever stops being true the test above proves nothing.
func TestRestartWithoutPersistenceDoesReannounce(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	before := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, &sink{}, nil)
	before.SetClock(clk.now)
	before.Notify(fault("chan1", "playlist_stalled"))

	sink := &sink{}
	after := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, sink, nil)
	after.SetClock(clk.now)
	after.Notify(fault("chan1", "playlist_stalled"))
	if sink.len() != 1 {
		t.Fatalf("expected the unpersisted tracker to page again, got %+v", sink.fs)
	}
}

// A fault that cleared while the process was down must resolve -- but only
// after a full grace period, not instantly. Restoring the old lastSeen would
// resolve it on the first sweep, before any observation had a chance to say
// whether it was still broken.
func TestFaultFixedDuringDowntimeResolvesAfterAGracePeriod(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	before, _ := stateTracker(t, dir, clk)
	before.Notify(fault("chan1", "no_segments"))
	if err := before.SaveState(); err != nil {
		t.Fatal(err)
	}

	// Down for ten minutes, far longer than ResolveAfter.
	clk.advance(10 * time.Minute)
	after, sink := stateTracker(t, dir, clk)
	if _, err := after.LoadState(); err != nil {
		t.Fatal(err)
	}

	after.Sweep()
	if sink.len() != 0 {
		t.Fatalf("resolved instantly on restart, before anything was observed: %+v", sink.fs)
	}

	// Nothing re-observes it, so it clears once the grace period is up.
	clk.advance(91 * time.Second)
	after.Sweep()
	if sink.len() != 1 || sink.at(0).Status != Resolved {
		t.Fatalf("expected one resolve after the grace period, got %+v", sink.fs)
	}
}

// The resolve message counts observations and elapsed time, and must not lose
// its history to a restart.
func TestRestoredIncidentKeepsItsHistory(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	before, _ := stateTracker(t, dir, clk)
	for i := 0; i < 5; i++ {
		before.Notify(fault("chan1", "segment_availability"))
		clk.advance(4 * time.Second)
	}
	if err := before.SaveState(); err != nil {
		t.Fatal(err)
	}

	clk.advance(10 * time.Second)
	after, sink := stateTracker(t, dir, clk)
	if _, err := after.LoadState(); err != nil {
		t.Fatal(err)
	}
	after.Notify(fault("chan1", "segment_availability")) // sixth observation
	clk.advance(91 * time.Second)
	after.Sweep()

	if sink.len() != 1 {
		t.Fatalf("expected a resolve, got %+v", sink.fs)
	}
	res := sink.at(0)
	if res.Count != 6 {
		t.Errorf("resolved Count = %d, want 6 across the restart", res.Count)
	}
	if res.FirstSeen == nil || !res.FirstSeen.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("FirstSeen = %v, want the original first sighting", res.FirstSeen)
	}
}

// Only incidents that actually opened are carried across. A blip still inside
// its For window may never become anything.
func TestUnopenedIncidentsAreNotPersisted(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	sink := &sink{}
	tr := NewTracker(TrackerConfig{For: time.Minute, ResolveAfter: 90 * time.Second}, sink, nil)
	tr.SetClock(clk.now)
	tr.SetStateFile(filepath.Join(dir, "state.json"))

	tr.Notify(fault("chan1", "pdt_stale")) // inside For, so not open
	if err := tr.SaveState(); err != nil {
		t.Fatal(err)
	}

	after, _ := stateTracker(t, dir, clk)
	n, err := after.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("restored %d incidents, want 0 -- a blip is not an incident", n)
	}
}

// --- the file itself ---

func TestMissingStateFileIsNotAnError(t *testing.T) {
	clk := &clock{t: time.Now()}
	tr, _ := stateTracker(t, t.TempDir(), clk)
	n, err := tr.LoadState()
	if err != nil || n != 0 {
		t.Errorf("first run should load nothing without complaint, got %d/%v", n, err)
	}
}

// Monitoring that refuses to start because its own scratch file is corrupt is
// worse than monitoring with no memory.
func TestCorruptStateFileIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now()}
	tr, _ := stateTracker(t, dir, clk)

	n, err := tr.LoadState()
	if err == nil {
		t.Error("a corrupt state file should be reported")
	}
	if n != 0 {
		t.Errorf("restored %d incidents from a corrupt file", n)
	}
	// The tracker must still work.
	sink := &sink{}
	tr.out = sink
	tr.Notify(fault("chan1", "manifest_fetch"))
	if sink.len() != 1 {
		t.Errorf("tracker unusable after a corrupt load: %+v", sink.fs)
	}
}

// A snapshot from long ago describes a world that has moved on.
func TestStaleSnapshotIsIgnored(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	before, _ := stateTracker(t, dir, clk)
	before.Notify(fault("chan1", "playlist_stalled"))
	if err := before.SaveState(); err != nil {
		t.Fatal(err)
	}

	clk.advance(maxSnapshotAge + time.Minute)
	after, _ := stateTracker(t, dir, clk)
	n, err := after.LoadState()
	if err == nil {
		t.Error("expected the stale snapshot to be reported as ignored")
	}
	if n != 0 {
		t.Errorf("restored %d incidents from a stale snapshot", n)
	}
}

func TestSnapshotVersionMismatchIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	b, _ := json.Marshal(snapshot{Version: stateVersion + 1, SavedAt: time.Now()})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	tr, _ := stateTracker(t, dir, &clock{t: time.Now()})
	if n, err := tr.LoadState(); err == nil || n != 0 {
		t.Errorf("a future format should be refused, got %d/%v", n, err)
	}
}

// Sweep persists, so a crash loses at most one sweep interval rather than
// everything since startup.
func TestSweepPersistsWithoutAnExplicitSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	tr, _ := stateTracker(t, dir, clk)
	tr.Notify(fault("chan1", "playlist_rollback"))
	tr.Sweep()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("sweep did not write the state file: %v", err)
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("sweep wrote something unreadable: %v", err)
	}
	if len(snap.Incidents) != 1 || snap.Incidents[0].Last.Check != "playlist_rollback" {
		t.Errorf("snapshot contents wrong: %+v", snap.Incidents)
	}
}

// An idle tracker must not rewrite the file every sweep for the life of the
// process.
func TestUnchangedStateIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	tr, _ := stateTracker(t, dir, clk)
	tr.Notify(fault("chan1", "no_segments"))
	tr.Sweep()

	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clk.advance(10 * time.Second)
		tr.Sweep()
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("the state file was rewritten though nothing changed")
	}
}

// Persistence is opt-in, and off by default.
func TestNoStateFileMeansNoFiles(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, &sink{}, nil)
	tr.Notify(fault("chan1", "no_segments"))
	tr.Sweep()
	if err := tr.SaveState(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d files with persistence disabled", len(entries))
	}
}

// The write is atomic, so a crash mid-write cannot leave a half-written
// snapshot that the next start refuses to read.
func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr, _ := stateTracker(t, dir, clk)

	tr.Notify(fault("chan1", "key_fetch"))
	if err := tr.SaveState(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want just state.json", names)
	}
}

// The claim atomicity makes is that a reader never sees a half-written file.
// A plain write leaves the file truncated for as long as the write takes, and
// the next start would refuse to parse it. This reproduces that: one goroutine
// writes a payload too large for a single syscall while another reads, and any
// read that is neither empty nor complete is the bug.
func TestWriteFileAtomicIsNeverPartiallyVisible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const marker = "-END-"
	payload := func(i int) []byte {
		b := make([]byte, 0, 1<<18)
		b = append(b, byte('a'+i%26))
		for len(b) < 1<<18-len(marker) {
			b = append(b, 'x')
		}
		return append(b, marker...)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			if err := writeFileAtomic(path, payload(i)); err != nil {
				t.Errorf("write failed: %v", err)
				return
			}
		}
	}()

	partial := 0
	for {
		select {
		case <-done:
			if partial > 0 {
				t.Errorf("a reader saw %d partially written files", partial)
			}
			return
		default:
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			continue // not created yet, or caught between rename and open
		}
		if !bytes.HasSuffix(b, []byte(marker)) {
			partial++
		}
	}
}

// A state directory the process cannot write to must be reported once, not
// once per sweep for the life of the process. Keying on the error text does
// not achieve that: the text carries a randomly named temp file, so every
// failure looks new.
func TestUnwritableStateIsReportedOnce(t *testing.T) {
	// The parent of the state file is a regular file, so creating the
	// temporary alongside it fails with ENOTDIR. A read-only directory would
	// have been the obvious way to arrange this and does not work: the tests
	// run as root in CI's container, and root writes to those regardless.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, &sink{}, nil)
	tr.SetClock(clk.now)
	tr.SetStateFile(filepath.Join(notADir, "state.json"))

	tr.Notify(fault("chan1", "no_segments"))

	var logged int
	for i := 0; i < 5; i++ {
		before := tr.saveFailing
		clk.advance(10 * time.Second)
		tr.Sweep()
		if !before && tr.saveFailing {
			logged++
		}
	}
	if logged != 1 {
		t.Errorf("the failure was announced %d times, want 1", logged)
	}
	if !tr.saveFailing {
		t.Error("the tracker should know it is still failing")
	}

	// And once a real directory is there, the next save clears the state so a
	// later failure is reported again rather than swallowed.
	if err := os.Remove(notADir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(notADir, 0o755); err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Second)
	tr.Sweep()
	if tr.saveFailing {
		t.Error("a successful save should clear the failure state")
	}
}
