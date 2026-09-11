// Package alert defines findings (the output of a health check) and notifiers
// that deliver them. Notifier is a small interface so new sinks (PagerDuty,
// webhook, email, a database) can be added without touching the prober.
package alert

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// Severity tiers, loosely mirroring the operational language broadcast NOCs use.
type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Status distinguishes an incident that is currently firing from one that has
// cleared. The prober emits bare observations; the Tracker stamps a status on
// the ones it decides are worth telling a human about.
type Status string

const (
	Firing   Status = "firing"
	Resolved Status = "resolved"
)

// Finding is a single health observation about a target/variant. The last three
// fields are filled in by the Tracker when a finding becomes a notification.
type Finding struct {
	Time      time.Time  `json:"time"`
	Target    string     `json:"target"`
	Variant   string     `json:"variant,omitempty"`
	Check     string     `json:"check"`
	Severity  Severity   `json:"severity"`
	Message   string     `json:"message"`
	Status    Status     `json:"status,omitempty"`
	Count     int        `json:"count,omitempty"`
	FirstSeen *time.Time `json:"first_seen,omitempty"`
}

// Notifier delivers findings somewhere.
type Notifier interface {
	Notify(f Finding)
}

// JSONNotifier writes each finding as a JSON line to stdout — trivially
// pipeable into Loki, Vector, jq, or a file.
type JSONNotifier struct {
	enc *json.Encoder
}

func NewJSONNotifier() *JSONNotifier {
	return &JSONNotifier{enc: json.NewEncoder(os.Stdout)}
}

func (n *JSONNotifier) Notify(f Finding) {
	_ = n.enc.Encode(f)
}

// SlackNotifier posts warning/critical findings to a Slack incoming webhook.
// Info findings are suppressed to avoid channel noise.
type SlackNotifier struct {
	url    string
	client *http.Client
}

func NewSlackNotifier(url string) *SlackNotifier {
	return &SlackNotifier{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

func (s *SlackNotifier) Notify(f Finding) {
	if f.Severity == Info {
		return
	}
	tag := string(f.Severity)
	if f.Status == Resolved {
		tag = "resolved"
	}
	text := "[" + tag + "] " + f.Target
	if f.Variant != "" {
		text += " (" + f.Variant + ")"
	}
	text += " — " + f.Check + ": " + f.Message

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// Multi fans a finding out to several notifiers.
func Multi(ns ...Notifier) Notifier { return multi(ns) }

type multi []Notifier

func (m multi) Notify(f Finding) {
	for _, n := range m {
		n.Notify(f)
	}
}

// Recorder keeps the most recent notifications in memory so the web UI can
// show what has happened lately.
//
// It sits in the notifier chain rather than reading from the tracker because
// what belongs in a log is the transitions -- opened, cleared -- and those are
// exactly what reaches a notifier. The tracker's own state answers the
// different question of what is open right now.
type Recorder struct {
	mu   sync.Mutex
	max  int
	ring []Finding
}

func NewRecorder(max int) *Recorder {
	if max <= 0 {
		max = 200
	}
	return &Recorder{max: max}
}

func (r *Recorder) Notify(f Finding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring = append(r.ring, f)
	if len(r.ring) > r.max {
		r.ring = r.ring[len(r.ring)-r.max:]
	}
}

// Recent returns the notifications newest first.
func (r *Recorder) Recent() []Finding {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Finding, 0, len(r.ring))
	for i := len(r.ring) - 1; i >= 0; i-- {
		out = append(out, r.ring[i])
	}
	return out
}
