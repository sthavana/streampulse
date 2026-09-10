package alert

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type sink struct {
	mu sync.Mutex
	fs []Finding
}

func (s *sink) Notify(f Finding) {
	s.mu.Lock()
	s.fs = append(s.fs, f)
	s.mu.Unlock()
}

func (s *sink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fs)
}

func (s *sink) at(i int) Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fs[i]
}

func (s *sink) statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.fs))
	for _, f := range s.fs {
		out = append(out, f.Status)
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTracker(cfg TrackerConfig) (*Tracker, *sink, *clock) {
	s := &sink{}
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := NewTracker(cfg, s, nil)
	tr.now = clk.now
	return tr, s, clk
}

func obs(check string) Finding {
	return Finding{Target: "chan1", Variant: "720p", Check: check,
		Severity: Critical, Message: "live edge frozen"}
}

// The headline behaviour: a fault re-observed on every poll is one notification.
func TestRepeatedObservationsNotifyOnce(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})

	// 4s poll interval for five minutes: 75 observations of the same fault.
	for i := 0; i < 75; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(4 * time.Second)
	}

	if s.len() != 1 {
		t.Fatalf("got %d notifications, want 1 (statuses: %v)", s.len(), s.statuses())
	}
	if got := s.at(0).Status; got != Firing {
		t.Errorf("status = %q, want %q", got, Firing)
	}
	if tr.Active() != 1 {
		t.Errorf("Active() = %d, want 1", tr.Active())
	}
}

func TestResolveEmittedAfterGracePeriod(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})

	tr.Notify(obs("playlist_stalled"))
	clk.advance(4 * time.Second)
	tr.Notify(obs("playlist_stalled"))

	// Fault clears: no further observations. Sweeping inside the grace period
	// must stay quiet.
	clk.advance(60 * time.Second)
	tr.Sweep()
	if s.len() != 1 {
		t.Fatalf("resolved early: %v", s.statuses())
	}

	clk.advance(40 * time.Second) // now past ResolveAfter
	tr.Sweep()

	if s.len() != 2 {
		t.Fatalf("got %d notifications, want 2: %v", s.len(), s.statuses())
	}
	res := s.at(1)
	if res.Status != Resolved {
		t.Errorf("status = %q, want %q", res.Status, Resolved)
	}
	if res.Count != 2 {
		t.Errorf("Count = %d, want 2", res.Count)
	}
	if !strings.Contains(res.Message, "cleared after 2 observation") {
		t.Errorf("message = %q, want a cleared summary", res.Message)
	}
	if tr.Active() != 0 {
		t.Errorf("Active() = %d after resolve, want 0", tr.Active())
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 30 * time.Second})

	tr.Notify(obs("playlist_stalled"))
	clk.advance(40 * time.Second)
	tr.Sweep()
	tr.Sweep()
	tr.Sweep()

	if s.len() != 2 {
		t.Errorf("got %d notifications, want 2 (one resolve): %v", s.len(), s.statuses())
	}
}

func TestRecurrenceAfterResolveOpensAgain(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 30 * time.Second})

	tr.Notify(obs("playlist_stalled"))
	clk.advance(40 * time.Second)
	tr.Sweep() // resolved
	tr.Notify(obs("playlist_stalled"))

	want := []Status{Firing, Resolved, Firing}
	got := s.statuses()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A changing message must not defeat deduplication: segment_availability names
// a different segment URI every poll.
func TestChangingMessageDoesNotReopen(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})

	for i := 0; i < 20; i++ {
		f := obs("segment_availability")
		f.Message = "segment not available (HTTP 404): seg" + string(rune('a'+i)) + ".ts"
		tr.Notify(f)
		clk.advance(4 * time.Second)
	}

	if s.len() != 1 {
		t.Fatalf("got %d notifications, want 1: %v", s.len(), s.statuses())
	}
}

func TestDistinctScopesAreDistinctIncidents(t *testing.T) {
	tr, s, _ := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})

	base := obs("playlist_stalled")
	tr.Notify(base)

	other := base
	other.Check = "segment_availability"
	tr.Notify(other)

	otherVariant := base
	otherVariant.Variant = "1080p"
	tr.Notify(otherVariant)

	otherTarget := base
	otherTarget.Target = "chan2"
	tr.Notify(otherTarget)

	if s.len() != 4 {
		t.Errorf("got %d notifications, want 4 distinct incidents", s.len())
	}
	if tr.Active() != 4 {
		t.Errorf("Active() = %d, want 4", tr.Active())
	}
}

// --- For: flap damping ---

func TestForSuppressesSingleBlipEntirely(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{For: 20 * time.Second, ResolveAfter: 60 * time.Second})

	tr.Notify(obs("segment_availability")) // one transient 404, then gone

	clk.advance(90 * time.Second)
	tr.Sweep()

	if s.len() != 0 {
		t.Errorf("a blip that never opened produced %d notifications: %v", s.len(), s.statuses())
	}
}

func TestForOpensOncePersistent(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{For: 20 * time.Second, ResolveAfter: 60 * time.Second})

	// Poll every 4s; the condition must persist 20s before it is worth telling
	// anyone about, so the first five observations stay silent.
	for i := 0; i < 5; i++ {
		tr.Notify(obs("segment_availability"))
		if s.len() != 0 {
			t.Fatalf("notified after %ds, before For elapsed", i*4)
		}
		clk.advance(4 * time.Second)
	}
	tr.Notify(obs("segment_availability")) // t=20s

	if s.len() != 1 {
		t.Fatalf("got %d notifications, want 1: %v", s.len(), s.statuses())
	}
}

// --- RepeatEvery: reminders ---

func TestRepeatEveryRemindsOnInterval(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{
		ResolveAfter: 90 * time.Second,
		RepeatEvery:  5 * time.Minute,
	})

	// A 10s-interval probe on a persistent fault, observations at t=0s..1790s.
	for i := 0; i < 180; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(10 * time.Second)
	}

	// Opens at t=0, then reminds at t=300,600,900,1200,1500. The t=1800
	// reminder falls after the last observation, so it does not land.
	if s.len() != 6 {
		t.Fatalf("got %d notifications, want 6 (1 open + 5 reminders): %v", s.len(), s.statuses())
	}
	for i, f := range s.statuses() {
		if f != Firing {
			t.Errorf("notification %d status = %q, want %q", i, f, Firing)
		}
	}
}

func TestRepeatEveryOffByDefault(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})
	for i := 0; i < 180; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(10 * time.Second)
	}
	if s.len() != 1 {
		t.Errorf("got %d notifications with reminders off, want 1", s.len())
	}
}

// --- defaults and metrics ---

func TestResolveAfterDefaultApplied(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{}) // no ResolveAfter given
	tr.Notify(obs("playlist_stalled"))

	clk.advance(80 * time.Second)
	tr.Sweep()
	if s.len() != 1 {
		t.Fatalf("resolved before the 90s default elapsed: %v", s.statuses())
	}
	clk.advance(20 * time.Second)
	tr.Sweep()
	if s.len() != 2 {
		t.Errorf("did not resolve after the 90s default: %v", s.statuses())
	}
}

type fakeMetrics struct {
	mu       sync.Mutex
	gauges   map[string]float64
	counters map[string]int
}

func (m *fakeMetrics) SetGauge(name, _ string, v float64, labels map[string]string) {
	m.mu.Lock()
	m.gauges[name+"|"+labels["check"]] = v
	m.mu.Unlock()
}

func (m *fakeMetrics) IncCounter(name, _ string, labels map[string]string) {
	m.mu.Lock()
	m.counters[name+"|"+labels["check"]]++
	m.mu.Unlock()
}

func TestMetricsTrackIncidentLifecycle(t *testing.T) {
	mx := &fakeMetrics{gauges: map[string]float64{}, counters: map[string]int{}}
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := NewTracker(TrackerConfig{ResolveAfter: 30 * time.Second}, &sink{}, mx)
	tr.now = clk.now

	tr.Notify(obs("playlist_stalled"))
	if got := mx.gauges["streampulse_incident_active|playlist_stalled"]; got != 1 {
		t.Errorf("active gauge = %v while firing, want 1", got)
	}

	clk.advance(40 * time.Second)
	tr.Sweep()
	if got := mx.gauges["streampulse_incident_active|playlist_stalled"]; got != 0 {
		t.Errorf("active gauge = %v after resolve, want 0", got)
	}
	if got := mx.counters["streampulse_incidents_opened_total|playlist_stalled"]; got != 1 {
		t.Errorf("opened counter = %d, want 1", got)
	}
	if got := mx.counters["streampulse_incidents_resolved_total|playlist_stalled"]; got != 1 {
		t.Errorf("resolved counter = %d, want 1", got)
	}
}

// The prober runs a goroutine per target, so Notify and Sweep race by design.
func TestConcurrentObserveAndSweep(t *testing.T) {
	tr := NewTracker(TrackerConfig{ResolveAfter: time.Millisecond}, &sink{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				f := obs("playlist_stalled")
				f.Target = "chan" + string(rune('a'+n))
				tr.Notify(f)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			tr.Sweep()
		}
	}()
	wg.Wait()
}
