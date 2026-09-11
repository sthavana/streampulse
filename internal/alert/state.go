package alert

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateVersion guards the on-disk format. A snapshot written by a different
// version is ignored rather than guessed at: starting with no memory is a
// known state, and starting with a misread one is not.
const stateVersion = 1

// maxSnapshotAge is how old a snapshot may be and still be trusted.
//
// The point of persistence is surviving a restart or a redeploy -- seconds to
// minutes. Beyond an hour the world has moved on, and reopening incidents from
// it would announce a resolve for a fault that ended long ago and was probably
// already dealt with by hand.
const maxSnapshotAge = time.Hour

type snapshot struct {
	Version   int                `json:"version"`
	SavedAt   time.Time          `json:"saved_at"`
	Incidents []incidentSnapshot `json:"incidents"`
}

type incidentSnapshot struct {
	Last       Finding   `json:"last"`
	FirstSeen  time.Time `json:"first_seen"`
	Count      int       `json:"count"`
	Announced  bool      `json:"announced"`
	NotifiedAt time.Time `json:"notified_at,omitempty"`
}

// SetStateFile makes the tracker persist open incidents to path, so a restart
// does not re-page for faults everyone has already been told about.
//
// Passing "" disables persistence, which is the default. Sweep writes the file
// when the state has changed, so a crash loses at most one sweep interval.
func (t *Tracker) SetStateFile(path string) {
	t.mu.Lock()
	t.statePath = path
	t.mu.Unlock()
}

// LoadState restores incidents from the configured state file and returns how
// many came back.
//
// A missing file is not an error: it is what the first run looks like. Neither
// is an unreadable one -- monitoring that refuses to start because its own
// scratch file is corrupt is worse than monitoring with no memory, so the
// error is returned for the caller to log and startup continues.
func (t *Tracker) LoadState() (int, error) {
	t.mu.Lock()
	path := t.statePath
	t.mu.Unlock()
	if path == "" {
		return 0, nil
	}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return 0, fmt.Errorf("state file %s is unreadable: %w", path, err)
	}
	if snap.Version != stateVersion {
		return 0, fmt.Errorf("state file %s is version %d, want %d", path, snap.Version, stateVersion)
	}

	t.mu.Lock()
	now := t.now()
	if age := now.Sub(snap.SavedAt); age > maxSnapshotAge {
		t.mu.Unlock()
		return 0, fmt.Errorf("state file %s is %s old, ignoring it", path, age.Round(time.Second))
	}

	restored := make([]*incident, 0, len(snap.Incidents))
	for _, s := range snap.Incidents {
		inc := &incident{
			last:      s.Last,
			firstSeen: s.FirstSeen,
			// lastSeen is reset to now rather than restored. The tracker's
			// model is "presumed firing until ResolveAfter passes with no
			// observation", and after a restart there have been no
			// observations at all. Restoring the old timestamp would make the
			// first sweep resolve everything instantly -- announcing a
			// clearing for faults that are probably still live, and re-opening
			// them a poll later. This gives each one a full grace period
			// instead: still broken, and the next poll refreshes it silently;
			// fixed while we were down, and it resolves properly.
			lastSeen:   now,
			count:      s.Count,
			open:       true,
			announced:  s.Announced,
			notifiedAt: s.NotifiedAt,
		}
		t.incidents[incidentKey(s.Last)] = inc
		restored = append(restored, inc)
	}
	t.mu.Unlock()

	// Republish the gauges, so a dashboard shows what is open the moment the
	// process is back rather than only after the next state change.
	for _, inc := range restored {
		if inc.announced {
			t.setIncidentGauge(inc.last, 1)
		}
	}
	return len(restored), nil
}

// SaveState writes the open incidents to the configured state file.
func (t *Tracker) SaveState() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveLocked()
}

// saveLocked writes the snapshot if it differs from what is already on disk.
// Called with the lock held.
func (t *Tracker) saveLocked() error {
	if t.statePath == "" {
		return nil
	}

	snap := snapshot{Version: stateVersion, SavedAt: t.now().UTC()}
	for _, inc := range t.incidents {
		// Only incidents that actually opened. One still inside its For window
		// is a blip that may never become anything, and carrying blips across
		// a restart buys nothing worth the complication.
		if !inc.open {
			continue
		}
		snap.Incidents = append(snap.Incidents, incidentSnapshot{
			Last:       inc.last,
			FirstSeen:  inc.firstSeen.UTC(),
			Count:      inc.count,
			Announced:  inc.announced,
			NotifiedAt: inc.notifiedAt.UTC(),
		})
	}
	sortIncidents(snap.Incidents)

	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	// SavedAt changes on every call, so compare the incidents rather than the
	// bytes: otherwise an idle tracker rewrites the file on every sweep.
	fingerprint, err := json.Marshal(snap.Incidents)
	if err != nil {
		return err
	}
	if string(fingerprint) == t.lastSaved {
		return nil
	}

	if err := writeFileAtomic(t.statePath, b); err != nil {
		return err
	}
	t.lastSaved = string(fingerprint)
	return nil
}

// writeFileAtomic writes via a temporary file in the same directory and
// renames it into place, so a crash mid-write cannot leave a half-written
// snapshot that the next start would refuse to read.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename has succeeded

	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sortIncidents keeps the file stable between writes, so the change comparison
// is about content rather than map iteration order.
func sortIncidents(in []incidentSnapshot) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && incidentKey(in[j].Last) < incidentKey(in[j-1].Last); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
