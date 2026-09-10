package alert

import (
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
	last       Finding // most recent observation, for its message
	firstSeen  time.Time
	lastSeen   time.Time
	count      int
	firing     bool
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

	out Notifier
	mx  Metrics
	cfg TrackerConfig
	now func() time.Time
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
}

// Active reports how many incidents are currently firing.
func (t *Tracker) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, inc := range t.incidents {
		if inc.firing {
			n++
		}
	}
	return n
}

func (t *Tracker) observe(f Finding) []Finding {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	key := f.Target + "\x00" + f.Variant + "\x00" + f.Check

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

	switch {
	case !inc.firing && now.Sub(inc.firstSeen) >= t.cfg.For:
		inc.firing = true
		inc.notifiedAt = now
		return []Finding{render(inc, Firing, now)}

	case inc.firing && t.cfg.RepeatEvery > 0 && now.Sub(inc.notifiedAt) >= t.cfg.RepeatEvery:
		inc.notifiedAt = now
		return []Finding{render(inc, Firing, now)}
	}
	return nil
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
		// An incident that never reached For was a blip: drop it silently
		// rather than announce the clearing of something never announced.
		if inc.firing {
			out = append(out, render(inc, Resolved, now))
		}
		delete(t.incidents, key)
	}
	return out
}

// render turns incident state into the Finding that goes out to a notifier.
func render(inc *incident, st Status, now time.Time) Finding {
	f := inc.last
	first := inc.firstSeen
	f.Time = now
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
