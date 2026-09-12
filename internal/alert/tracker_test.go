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

// --- maintenance window suppression ---

// windowAround builds a schedule open for the whole test unless narrowed.
func alwaysOpen(name string, targets, checks []string) Schedule {
	return Schedule{{Name: name, Targets: targets, Checks: checks,
		Loc: time.UTC, Daily: &Daily{StartMin: 0, EndMin: 1440}}}
}

func TestMaintenanceSuppressesOpenNotification(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})
	tr.SetSchedule(alwaysOpen("planned", nil, nil))

	for i := 0; i < 20; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(4 * time.Second)
	}

	if s.len() != 0 {
		t.Fatalf("maintenance window did not suppress: %v", s.statuses())
	}
	// The incident is tracked even though nobody was told.
	if tr.Tracking() != 1 {
		t.Errorf("Tracking() = %d, want 1", tr.Tracking())
	}
	if tr.Active() != 0 {
		t.Errorf("Active() = %d, want 0 while suppressed", tr.Active())
	}
}

// The "did I break something?" case: a fault that starts during maintenance and
// is still broken when the window closes must be announced then.
func TestFaultStillBrokenWhenWindowClosesIsAnnounced(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})

	// Window covers 02:00-04:00 UTC; start the clock inside it.
	tr.SetSchedule(Schedule{{Name: "upgrade", Loc: time.UTC,
		Daily: &Daily{StartMin: 120, EndMin: 240}}})
	clk.t = time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ { // still inside the window
		tr.Notify(obs("playlist_stalled"))
		clk.advance(4 * time.Second)
	}
	if s.len() != 0 {
		t.Fatalf("notified inside the window: %v", s.statuses())
	}

	// Move past 04:00; the fault is still there.
	clk.t = time.Date(2026, 1, 1, 4, 0, 1, 0, time.UTC)
	tr.Notify(obs("playlist_stalled"))

	if s.len() != 1 {
		t.Fatalf("got %d notifications after the window closed, want 1: %v", s.len(), s.statuses())
	}
	f := s.at(0)
	if f.Status != Firing {
		t.Errorf("status = %q, want %q", f.Status, Firing)
	}
	// It carries the full history, so the operator sees it did not just start.
	if f.Count != 11 {
		t.Errorf("Count = %d, want 11 observations including the suppressed ones", f.Count)
	}
}

// A fault that opens and clears entirely inside the window is never announced.
func TestFaultOpeningAndClearingInsideWindowStaysSilent(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 30 * time.Second})
	tr.SetSchedule(alwaysOpen("planned", nil, nil))

	for i := 0; i < 5; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(4 * time.Second)
	}
	clk.advance(60 * time.Second) // fault clears, incident expires
	tr.Sweep()

	if s.len() != 0 {
		t.Errorf("expected total silence, got: %v", s.statuses())
	}
	if tr.Tracking() != 0 {
		t.Errorf("Tracking() = %d after expiry, want 0", tr.Tracking())
	}
}

// A resolve for an already-announced incident is delivered even if a window
// opened in the meantime: withholding it would leave the operator believing a
// fault they were told about is still open.
func TestResolveDeliveredEvenIfWindowOpensLater(t *testing.T) {
	tr, s, clk := newTracker(TrackerConfig{ResolveAfter: 30 * time.Second})

	tr.Notify(obs("playlist_stalled")) // announced, no window yet
	if s.len() != 1 {
		t.Fatalf("expected the open notification, got %v", s.statuses())
	}

	tr.SetSchedule(alwaysOpen("planned", nil, nil)) // maintenance starts
	clk.advance(40 * time.Second)
	tr.Sweep()

	if s.len() != 2 {
		t.Fatalf("got %d notifications, want 2: %v", s.len(), s.statuses())
	}
	if got := s.at(1).Status; got != Resolved {
		t.Errorf("status = %q, want %q", got, Resolved)
	}
}

func TestMaintenanceScopedToTargetLeavesOthersAlone(t *testing.T) {
	tr, s, _ := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})
	tr.SetSchedule(alwaysOpen("chan1 only", []string{"chan1"}, nil))

	tr.Notify(obs("playlist_stalled")) // Target chan1: suppressed

	other := obs("playlist_stalled")
	other.Target = "chan2"
	tr.Notify(other) // not covered: must alert

	if s.len() != 1 {
		t.Fatalf("got %d notifications, want 1 (chan2 only): %v", s.len(), s.statuses())
	}
	if got := s.at(0).Target; got != "chan2" {
		t.Errorf("notified target = %q, want chan2", got)
	}
}

func TestMaintenanceScopedToCheckLeavesOthersAlone(t *testing.T) {
	tr, s, _ := newTracker(TrackerConfig{ResolveAfter: 90 * time.Second})
	tr.SetSchedule(alwaysOpen("stall only", nil, []string{"playlist_stalled"}))

	tr.Notify(obs("playlist_stalled"))     // suppressed
	tr.Notify(obs("segment_availability")) // must alert

	if s.len() != 1 {
		t.Fatalf("got %d notifications, want 1: %v", s.len(), s.statuses())
	}
	if got := s.at(0).Check; got != "segment_availability" {
		t.Errorf("notified check = %q, want segment_availability", got)
	}
}

func TestSuppressionIsCounted(t *testing.T) {
	mx := &fakeMetrics{gauges: map[string]float64{}, counters: map[string]int{}}
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, &sink{}, mx)
	tr.SetClock(clk.now)
	tr.SetSchedule(alwaysOpen("planned", nil, nil))

	for i := 0; i < 5; i++ {
		tr.Notify(obs("playlist_stalled"))
		clk.advance(4 * time.Second)
	}

	got := mx.counters["streampulse_notifications_suppressed_total|playlist_stalled"]
	if got != 5 {
		t.Errorf("suppressed counter = %d, want 5", got)
	}
}

func TestMaintenanceGaugePublishedOnSweep(t *testing.T) {
	mx := &fakeMetrics{gauges: map[string]float64{}, counters: map[string]int{}}
	clk := &clock{t: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)}
	tr := NewTracker(TrackerConfig{ResolveAfter: 90 * time.Second}, &sink{}, mx)
	tr.SetClock(clk.now)
	tr.SetSchedule(Schedule{{Name: "nightly", Loc: time.UTC,
		Daily: &Daily{StartMin: 120, EndMin: 240}}})

	tr.Sweep() // 03:00, inside
	if got := mx.gauges["streampulse_maintenance_active|"]; got != 1 {
		t.Errorf("maintenance gauge = %v inside the window, want 1", got)
	}

	clk.t = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tr.Sweep() // midday, outside
	if got := mx.gauges["streampulse_maintenance_active|"]; got != 0 {
		t.Errorf("maintenance gauge = %v outside the window, want 0", got)
	}
}

func TestNotificationTimestampsAreUTC(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := &sink{}
	clk := &clock{t: time.Date(2026, 1, 1, 3, 0, 0, 0, la)}
	tr := NewTracker(TrackerConfig{ResolveAfter: 30 * time.Second}, s, nil)
	tr.SetClock(clk.now)

	tr.Notify(obs("playlist_stalled"))
	clk.advance(40 * time.Second)
	tr.Sweep()

	if s.len() != 2 {
		t.Fatalf("got %d notifications, want 2", s.len())
	}
	for i := 0; i < 2; i++ {
		f := s.at(i)
		if f.Time.Location() != time.UTC {
			t.Errorf("notification %d Time in %v, want UTC", i, f.Time.Location())
		}
		if f.FirstSeen == nil || f.FirstSeen.Location() != time.UTC {
			t.Errorf("notification %d FirstSeen not UTC", i)
		}
	}
}

// The same channel frozen in Frankfurt and fine in Ohio is two facts, not one.
// Collapsing them into a single incident would resolve the real one the moment
// the healthy vantage reported in.
func TestVantageIsPartOfIncidentIdentity(t *testing.T) {
	out := &sink{}
	tr := NewTracker(TrackerConfig{ResolveAfter: time.Minute}, out, nil)

	tr.Notify(Finding{Vantage: "eu-west", Target: "ch1", Check: "playlist_stalled", Severity: Critical})
	tr.Notify(Finding{Vantage: "us-east", Target: "ch1", Check: "playlist_stalled", Severity: Critical})

	if out.len() != 2 {
		t.Fatalf("got %d notifications, want one per vantage", out.len())
	}
	if n := tr.Tracking(); n != 2 {
		t.Errorf("tracking %d incidents, want 2", n)
	}
	// And the same fault from the same place is still one incident.
	tr.Notify(Finding{Vantage: "eu-west", Target: "ch1", Check: "playlist_stalled", Severity: Critical})
	if out.len() != 2 {
		t.Errorf("a repeat from the same vantage produced %d notifications", out.len())
	}
}

// A single-prober setup names no vantage, and nothing about it changes.
func TestNoVantageBehavesAsBefore(t *testing.T) {
	out := &sink{}
	tr := NewTracker(TrackerConfig{ResolveAfter: time.Minute}, out, nil)
	tr.Notify(Finding{Target: "ch1", Check: "no_segments", Severity: Critical})
	tr.Notify(Finding{Target: "ch1", Check: "no_segments", Severity: Critical})
	if out.len() != 1 {
		t.Errorf("got %d notifications, want 1", out.len())
	}
	if out.at(0).Vantage != "" {
		t.Errorf("an unset vantage should stay unset, got %q", out.at(0).Vantage)
	}
}

// Removing a target must close out its incidents rather than leave them to
// expire. Someone was told about the fault; they should be told it is over,
// and told now.
func TestForgetResolvesOpenIncidents(t *testing.T) {
	out := &sink{}
	tr := NewTracker(TrackerConfig{ResolveAfter: time.Hour}, out, nil)

	tr.Notify(Finding{Target: "gone", Check: "manifest_fetch", Severity: Critical, Message: "refused"})
	tr.Notify(Finding{Target: "stays", Check: "manifest_fetch", Severity: Critical, Message: "refused"})
	if out.len() != 2 {
		t.Fatalf("setup produced %d notifications", out.len())
	}

	if n := tr.Forget("gone"); n != 1 {
		t.Errorf("forgot %d incidents, want 1", n)
	}
	if out.len() != 3 {
		t.Fatalf("got %d notifications, want a resolve for the removed target", out.len())
	}
	res := out.at(2)
	if res.Status != Resolved || res.Target != "gone" {
		t.Errorf("resolve = %+v", res)
	}
	// The message must not imply the fault got better. It may not have.
	if !strings.Contains(res.Message, "removed from the configuration") {
		t.Errorf("message = %q, should say the target was removed", res.Message)
	}
	if tr.Tracking() != 1 {
		t.Errorf("tracking %d incidents, want just the one that stayed", tr.Tracking())
	}
}

// An incident that never reached anyone is dropped quietly: announcing a
// clearing for something nobody was told about is noise.
func TestForgetIsQuietForUnannouncedIncidents(t *testing.T) {
	out := &sink{}
	tr := NewTracker(TrackerConfig{For: time.Hour, ResolveAfter: time.Hour}, out, nil)
	tr.Notify(Finding{Target: "gone", Check: "no_segments", Severity: Critical})
	if out.len() != 0 {
		t.Fatalf("setup announced something it should not have")
	}
	if n := tr.Forget("gone"); n != 1 {
		t.Errorf("forgot %d, want 1", n)
	}
	if out.len() != 0 {
		t.Errorf("a never-announced incident produced %d notifications", out.len())
	}
}

func TestForgetAnUnknownTargetDoesNothing(t *testing.T) {
	out := &sink{}
	tr := NewTracker(TrackerConfig{ResolveAfter: time.Hour}, out, nil)
	tr.Notify(Finding{Target: "a", Check: "x", Severity: Critical})
	if n := tr.Forget("never-existed"); n != 0 {
		t.Errorf("forgot %d incidents for a target that was never there", n)
	}
	if out.len() != 1 || tr.Tracking() != 1 {
		t.Error("forgetting an unknown target disturbed a real one")
	}
}
