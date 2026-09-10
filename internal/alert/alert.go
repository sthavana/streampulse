// Package alert defines findings (the output of a health check) and notifiers
// that deliver them. Notifier is a small interface so new sinks (PagerDuty,
// webhook, email, a database) can be added without touching the prober.
package alert

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// Severity tiers, loosely mirroring the operational language broadcast NOCs use.
type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Finding is a single health observation about a target/variant.
type Finding struct {
	Time     time.Time `json:"time"`
	Target   string    `json:"target"`
	Variant  string    `json:"variant,omitempty"`
	Check    string    `json:"check"`
	Severity Severity  `json:"severity"`
	Message  string    `json:"message"`
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
	text := "[" + string(f.Severity) + "] " + f.Target
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
