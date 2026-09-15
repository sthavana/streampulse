// Package mosaic is StreamPulse's multiviewer: a tile wall that puts a picture
// next to the health the prober already knows.
//
// It fuses two inputs per target. Thumbnails come from a Grabber, one long-
// running ffmpeg per target decimated to a frame a second. Health comes whole
// from the prober's /api/state -- the same document the operator page is built
// from -- and is never accumulated here. That is deliberate: the prober owns
// what "critical" means, and a second copy of it would drift, leaving the wall
// confidently green during an outage. Everything in this package that looks
// like state is either a picture or a cache of the prober's answer.
//
// The package depends only on the standard library and knows nothing about
// internal/alert, internal/probe or internal/config. It talks to the prober
// over HTTP like any other client, which is what lets the wall run as its own
// process without the ffmpeg it needs ever reaching the alerting path.
package mosaic

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Severity mirrors the prober's finding tiers. It is defined locally rather
// than imported so that mosaic stays a client of the prober's JSON rather than
// of its Go packages.
type Severity int

const (
	SevOK Severity = iota
	SevInfo
	SevWarning
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "critical"
	case SevWarning:
		return "warning"
	case SevInfo:
		return "info"
	default:
		return "ok"
	}
}

// ParseSeverity converts a severity string from the prober's API.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit":
		return SevCritical
	case "warning", "warn":
		return SevWarning
	case "info":
		return SevInfo
	default:
		return SevOK
	}
}

// Health is one target's condition as the prober reports it.
//
// There is no decay and no hold window. A hold window is what you need when
// you are inferring health from a stream of events and have to guess when one
// has stopped mattering; here the prober states the current answer every time
// it is asked, so the tile shows the last answer and nothing expires.
type Health struct {
	Severity Severity
	// Summary is the worst open incident's message, or empty when a target is
	// healthy.
	Summary string
	// Up is nil for a target the prober has not managed to probe yet, which is
	// not the same as one it has probed and found down.
	Up *bool
	// The three numbers worth reading at a glance from across a room. Absent
	// as zero, which the tile renders as nothing rather than as "0".
	EdgeAge float64 // manifest_age_seconds
	TTFB    float64 // segment_ttfb_seconds, the worst variant of the target
	Window  float64 // playlist_window_seconds
}

// Status is the snapshot the browser consumes.
type Status struct {
	TargetID    string  `json:"target_id"`
	Name        string  `json:"name"`
	SeverityStr string  `json:"severity"`
	Summary     string  `json:"summary,omitempty"`
	Up          *bool   `json:"up,omitempty"`
	EdgeAge     float64 `json:"edge_age_s,omitempty"`
	TTFB        float64 `json:"ttfb_s,omitempty"`
	Window      float64 `json:"window_s,omitempty"`
	// FrameAge is how long ago the last thumbnail arrived, or -1 when none
	// ever has. It is the wall's own liveness signal, independent of anything
	// the prober says: a tile with a growing frame age has lost its grabber
	// even if the stream is perfectly healthy.
	FrameAge float64 `json:"frame_age_s"`
	// Stale marks a tile whose health is the last thing the prober said before
	// it became unreachable. Shown differently from a fault, because not
	// knowing is not the same as knowing something is wrong.
	Stale bool `json:"stale,omitempty"`
}

// TargetInfo is one tile's identity, in the order the wall lays them out.
type TargetInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type targetState struct {
	id, name  string
	frames    *frameHub
	mu        sync.RWMutex
	health    Health
	lastFrame time.Time
}

// Store is safe for concurrent use by grabbers, the state poller and HTTP
// handlers.
type Store struct {
	mu      sync.RWMutex
	targets map[string]*targetState
	order   []string
	stale   bool
}

func New() *Store {
	return &Store{targets: map[string]*targetState{}}
}

// Sync makes the wall's tiles match the given set, adding and removing as the
// prober's target list changes. Tiles that survive keep their frames, so a
// target added or removed elsewhere does not blank the whole wall.
//
// Returns the ids added and removed, which is what the grabber pool needs.
func (s *Store) Sync(targets []TargetInfo) (added, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t.ID] = true
		if existing, ok := s.targets[t.ID]; ok {
			existing.mu.Lock()
			existing.name = t.Name
			existing.mu.Unlock()
			continue
		}
		s.targets[t.ID] = &targetState{id: t.ID, name: t.Name, frames: newFrameHub()}
		added = append(added, t.ID)
	}
	for id := range s.targets {
		if !want[id] {
			removed = append(removed, id)
		}
	}
	for _, id := range removed {
		delete(s.targets, id)
	}

	// Ordered as the prober lists them, so the wall matches the operator page
	// and trouble stays near the top.
	s.order = s.order[:0]
	for _, t := range targets {
		s.order = append(s.order, t.ID)
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func (s *Store) get(id string) *targetState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targets[id]
}

// PushFrame stores the latest thumbnail and fans it out to viewers.
func (s *Store) PushFrame(id string, jpeg []byte) {
	ts := s.get(id)
	if ts == nil {
		return
	}
	ts.mu.Lock()
	ts.lastFrame = time.Now()
	ts.mu.Unlock()
	ts.frames.push(jpeg)
}

// SetHealth replaces what the wall believes about every target at once.
//
// Wholesale rather than per-target, because it is a copy of one document the
// prober produced at one instant. Merging half of a newer answer into half of
// an older one would invent a state that was never true anywhere.
func (s *Store) SetHealth(health map[string]Health) {
	s.mu.Lock()
	s.stale = false
	targets := make([]*targetState, 0, len(s.targets))
	for _, ts := range s.targets {
		targets = append(targets, ts)
	}
	s.mu.Unlock()

	for _, ts := range targets {
		h, ok := health[ts.id]
		ts.mu.Lock()
		if ok {
			ts.health = h
		} else {
			// The prober no longer mentions this target. Say nothing about it
			// rather than leaving the last thing it said standing.
			ts.health = Health{}
		}
		ts.mu.Unlock()
	}
}

// MarkStale records that the prober could not be reached. The tiles keep the
// last health they had, flagged, because a wall that blanks itself when its
// data source hiccups is worse than one that says how old its answer is.
func (s *Store) MarkStale() {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
}

// Snapshot returns every tile in wall order.
func (s *Store) Snapshot() []Status {
	s.mu.RLock()
	ids := append([]string(nil), s.order...)
	targets := make(map[string]*targetState, len(s.targets))
	for k, v := range s.targets {
		targets[k] = v
	}
	stale := s.stale
	s.mu.RUnlock()

	now := time.Now()
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		ts := targets[id]
		if ts == nil {
			continue
		}
		ts.mu.RLock()
		frameAge := -1.0
		if !ts.lastFrame.IsZero() {
			frameAge = now.Sub(ts.lastFrame).Seconds()
		}
		out = append(out, Status{
			TargetID: id, Name: ts.name,
			SeverityStr: ts.health.Severity.String(),
			Summary:     ts.health.Summary,
			Up:          ts.health.Up,
			EdgeAge:     ts.health.EdgeAge,
			TTFB:        ts.health.TTFB,
			Window:      ts.health.Window,
			FrameAge:    frameAge,
			Stale:       stale,
		})
		ts.mu.RUnlock()
	}
	return out
}

// Targets lists the tiles in wall order.
func (s *Store) Targets() []TargetInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TargetInfo, 0, len(s.order))
	for _, id := range s.order {
		if ts := s.targets[id]; ts != nil {
			out = append(out, TargetInfo{ID: id, Name: ts.name})
		}
	}
	return out
}

// SubscribeFrames returns a channel of JPEG frames (seeded with the latest)
// and a cancel the caller must invoke. Returns (nil, noop) for unknown ids.
func (s *Store) SubscribeFrames(id string) (<-chan []byte, func()) {
	ts := s.get(id)
	if ts == nil {
		return nil, func() {}
	}
	return ts.frames.subscribe()
}

// frameHub is a latest-wins fan-out. Slow viewers drop frames rather than
// applying backpressure to the grabber -- correct for a thumbnail wall.
type frameHub struct {
	mu     sync.RWMutex
	latest []byte
	subs   map[chan []byte]struct{}
}

func newFrameHub() *frameHub { return &frameHub{subs: map[chan []byte]struct{}{}} }

func (h *frameHub) push(b []byte) {
	h.mu.Lock()
	h.latest = b
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *frameHub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	if h.latest != nil {
		ch <- h.latest
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
