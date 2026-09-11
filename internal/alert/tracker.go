package alert

import (
	"log"
	"sort"
	"strconv"
	"sync"
	"time"
)

// TrackerConfig tunes the incident lifecycle.
type TrackerConfig struct {
	// For is how long a condition must persist before it is worth telling a
	// human about. Zero notifies on the first observation, which preserves
	// detection latency; raising it damps flapping checks such as
	// segment_availability, at the cost of that much delay.
	For time.Duration

	// ResolveAfter is how long an incident must go without being re-observed
	// before it is declared cleared.
	ResolveAfter time.Duration

	// RepeatEvery re-notifies a still-firing incident on this interval.
	// Zero disables reminders entirely.
	RepeatEvery time.Duration
}

func (c TrackerConfig) withDefaults() TrackerConfig {
	if c.ResolveAfter <= 0 {
		c.ResolveAfter = 90 * time.Second
	}
	return c
}

// Metrics is the slice of the metrics registry the Tracker needs, declared here
// so package alert keeps no dependency on package metrics.
type Metrics interface {
	SetGauge(name, help string, value float64, labels map[string]string)
	IncCounter(name, help string, labels map[string]string)
}

type incident struct {
	last      Finding // most recent observation, for its message
	firstSeen time.Time
	lastSeen  time.Time
	count     int

	// open means the condition has persisted past For and is a real incident.
	// announced means a human was actually told about it. They differ during a
	// maintenance window: the incident is tracked but stays quiet, and is
	// announced later if it is still open when the window closes.
	open       bool
	announced  bool
	notifiedAt time.Time
}

// Tracker collapses a stream of repeating findings into incidents, so a fault
// that a prober re-observes on every poll produces one notification when it
// opens and one when it clears, rather than one per poll.
//
// It implements Notifier, so it drops in wherever the raw notifier went, and
// forwards to the wrapped Notifier only on a state transition.
//
// Resolution is by expiry rather than by observing a clean evaluation: a probe
// that fails early (an unreachable manifest) never evaluates the downstream
// playlist checks at all, so "evaluated and clean" would wrongly clear them.
type Tracker struct {
	mu        sync.Mutex
	incidents map[string]*incident

	out      Notifier
	mx       Metrics
	cfg      TrackerConfig
	schedule Schedule
	now      func() time.Time

	// statePath, when set, is where open incidents are persisted so a restart
	// does not re-announce faults a human has already been told about.
	statePath   string
	lastSaved   string
	lastSaveErr string
}

// SetSchedule installs the maintenance windows during which alerting is
// suppressed. Safe to call before the prober starts.
func (t *Tracker) SetSchedule(s Schedule) {
	t.mu.Lock()
	t.schedule = s
	t.mu.Unlock()
}

// NewTracker wraps out. mx may be nil.
func NewTracker(cfg TrackerConfig, out Notifier, mx Metrics) *Tracker {
	return &Tracker{
		incidents: make(map[string]*incident),
		out:       out,
		mx:        mx,
		cfg:       cfg.withDefaults(),
		now:       time.Now,
	}
}

// SetClock overrides the tracker's clock. Intended for tests.
func (t *Tracker) SetClock(now func() time.Time) {
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
}

// Notify records one observation, forwarding it only if that opens an incident
// or a reminder is due.
func (t *Tracker) Notify(f Finding) {
	t.publish(t.observe(f)...)
}

// Sweep closes incidents that have not been re-observed within ResolveAfter.
// The caller drives it on a ticker; it is the only path that emits Resolved.
func (t *Tracker) Sweep() {
	t.publish(t.expire()...)
	t.refreshWindowGauges()
	t.mu.Lock()
	err := t.saveLocked()
	t.mu.Unlock()
	if err != nil {
		t.saveErr(err)
	}
}

// saveErr reports a failed snapshot once per distinct message. A state file
// that cannot be written is worth knowing about, but not worth a line every
// sweep interval for the life of the process.
func (t *Tracker) saveErr(err error) {
	t.mu.Lock()
	repeat := err.Error() == t.lastSaveErr
	t.lastSaveErr = err.Error()
	t.mu.Unlock()
	if !repeat {
		log.Printf("alert: could not write incident state: %v", err)
	}
}

// refreshWindowGauges publishes which maintenance windows are open right now.
func (t *Tracker) refreshWindowGauges() {
	if t.mx == nil {
		return
	}
	t.mu.Lock()
	sched, now := t.schedule, t.now()
	t.mu.Unlock()

	for _, w := range sched {
		v := 0.0
		if w.Active(now) {
			v = 1
		}
		t.mx.SetGauge("streampulse_maintenance_active",
			"1 while a maintenance window is open", v, map[string]string{"window": w.Name})
	}
}

// Active reports how many incidents have been announced and not yet cleared.
func (t *Tracker) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, inc := range t.incidents {
		if inc.announced {
			n++
		}
	}
	return n
}

// Incident is a read-only view of one tracked incident, for the web UI.
type Incident struct {
	Target    string    `json:"target"`
	Variant   string    `json:"variant,omitempty"`
	Check     string    `json:"check"`
	Severity  Severity  `json:"severity"`
	Message   string    `json:"message"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
	// Announced is false for an incident being held quiet by a maintenance
	// window. Showing it anyway is the point: suppression should be visible,
	// not indistinguishable from health.
	Announced bool `json:"announced"`
}

// Incidents lists what is open right now, worst first.
func (t *Tracker) Incidents() []Incident {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Incident, 0, len(t.incidents))
	for _, inc := range t.incidents {
		if !inc.open {
			continue
		}
		out = append(out, Incident{
			Target: inc.last.Target, Variant: inc.last.Variant, Check: inc.last.Check,
			Severity: inc.last.Severity, Message: inc.last.Message,
			FirstSeen: inc.firstSeen.UTC(), LastSeen: inc.lastSeen.UTC(),
			Count: inc.count, Announced: inc.announced,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := severityRank(out[i].Severity), severityRank(out[j].Severity); a != b {
			return a < b
		}
		return out[i].FirstSeen.Before(out[j].FirstSeen)
	})
	return out
}

func severityRank(s Severity) int {
	switch s {
	case Critical:
		return 0
	case Warning:
		return 1
	default:
		return 2
	}
}

// Tracking reports how many incidents are open, announced or not. During a
// maintenance window this exceeds Active.
func (t *Tracker) Tracking() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, inc := range t.incidents {
		if inc.open {
			n++
		}
	}
	return n
}

// incidentKey is the identity of an incident: one target, one variant, one
// check. The message is deliberately excluded -- segment_availability names a
// different segment URI every poll.
func incidentKey(f Finding) string {
	return f.Target + "\x00" + f.Variant + "\x00" + f.Check
}

func (t *Tracker) observe(f Finding) []Finding {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	key := incidentKey(f)

	inc := t.incidents[key]
	if inc == nil {
		inc = &incident{firstSeen: now}
		t.incidents[key] = inc
	}
	// The message is kept fresh but is deliberately not part of the key:
	// segment_availability names a different segment URI on every poll, and
	// keying on that would defeat the deduplication entirely.
	inc.last = f
	inc.lastSeen = now
	inc.count++

	if !inc.open && now.Sub(inc.firstSeen) >= t.cfg.For {
		inc.open = true
	}
	if !inc.open {
		return nil
	}

	window, quiet := t.schedule.Suppressed(f, now)
	if quiet {
		t.suppressed(f, window)
		return nil
	}

	switch {
	case !inc.announced:
		// Either this incident just opened, or it opened inside a maintenance
		// window that has since closed while the fault persisted.
		inc.announced = true
		inc.notifiedAt = now
		return []Finding{render(inc, Firing, now)}

	case t.cfg.RepeatEvery > 0 && now.Sub(inc.notifiedAt) >= t.cfg.RepeatEvery:
		inc.notifiedAt = now
		return []Finding{render(inc, Firing, now)}
	}
	return nil
}

// suppressed counts a notification withheld by a maintenance window, so the
// silence is visible on a dashboard rather than indistinguishable from health.
// Called with the lock held; IncCounter takes its own.
func (t *Tracker) suppressed(f Finding, window string) {
	if t.mx == nil {
		return
	}
	t.mx.IncCounter("streampulse_notifications_suppressed_total",
		"Notifications withheld by a maintenance window", map[string]string{
			"target": f.Target, "check": f.Check, "window": window,
		})
}

func (t *Tracker) expire() []Finding {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	var out []Finding
	for key, inc := range t.incidents {
		if now.Sub(inc.lastSeen) < t.cfg.ResolveAfter {
			continue
		}
		// Only announce a clearing for something that was announced. A blip
		// that never reached For, or one that opened and cleared entirely
		// inside a maintenance window, is dropped silently.
		//
		// Note this is deliberately not gated on the schedule: a resolve is
		// never a page, and withholding it would leave a human who was told
		// about the fault believing it is still open.
		if inc.announced {
			out = append(out, render(inc, Resolved, now))
		}
		delete(t.incidents, key)
	}
	return out
}

// render turns incident state into the Finding that goes out to a notifier.
func render(inc *incident, st Status, now time.Time) Finding {
	f := inc.last
	first := inc.firstSeen.UTC()
	// Normalise to UTC: the prober stamps findings in UTC and the tracker
	// must not reintroduce local time on the way out.
	f.Time = now.UTC()
	f.Status = st
	f.Count = inc.count
	f.FirstSeen = &first
	if st == Resolved {
		f.Message = "cleared after " + strconv.Itoa(inc.count) + " observation(s) over " +
			inc.lastSeen.Sub(inc.firstSeen).Round(time.Second).String()
	}
	return f
}

// publish updates metrics and forwards, deliberately outside the lock: a Slack
// webhook can block for seconds and must not stall every prober goroutine.
func (t *Tracker) publish(fs ...Finding) {
	for _, f := range fs {
		if t.mx != nil {
			labels := map[string]string{
				"target": f.Target, "variant": f.Variant,
				"check": f.Check, "severity": string(f.Severity),
			}
			active := 1.0
			if f.Status == Resolved {
				active = 0
			}
			t.mx.SetGauge("streampulse_incident_active",
				"1 while an incident is firing, 0 once it has cleared", active, labels)
			if f.Status == Resolved {
				t.mx.IncCounter("streampulse_incidents_resolved_total", "Incidents cleared", labels)
			} else {
				t.mx.IncCounter("streampulse_incidents_opened_total", "Incidents opened", labels)
			}
		}
		if t.out != nil {
			t.out.Notify(f)
		}
	}
}

// setIncidentGauge publishes the firing state of one incident.
func (t *Tracker) setIncidentGauge(f Finding, v float64) {
	if t.mx == nil {
		return
	}
	t.mx.SetGauge("streampulse_incident_active",
		"1 while an incident is firing, 0 once it has cleared", v, map[string]string{
			"target": f.Target, "variant": f.Variant,
			"check": f.Check, "severity": string(f.Severity),
		})
}
