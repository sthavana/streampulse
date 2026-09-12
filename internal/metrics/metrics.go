// Package metrics is a tiny, dependency-free Prometheus exposition layer.
// It supports labelled gauges and counters and serves the standard text format
// on /metrics. For a production build this is the natural place to swap in
// prometheus/client_golang; the Registry interface used by the prober would
// not change.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Registry struct {
	mu    sync.Mutex
	vals  map[string]float64 // full series key ("name" or `name{a="b"}`) -> value
	help  map[string]string  // metric name -> HELP text
	types map[string]string  // metric name -> TYPE
	order []string           // metric names in first-seen order
	// meta remembers each series in structured form. The exposition format
	// only ever needs the flattened key, but a reader -- the web UI -- needs
	// the labels back, and re-parsing them out of the key would be a parser
	// nobody should have to write twice.
	meta map[string]Sample
	// constant labels are attached to every series. Vantage lives here: a
	// prober in Frankfurt and one in Ohio watching the same channel produce
	// the same series names, and without something to tell them apart the
	// second one silently overwrites the first in Prometheus.
	constant map[string]string
}

// Sample is one series in structured form.
type Sample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

func New() *Registry {
	return &Registry{
		vals:  make(map[string]float64),
		help:  make(map[string]string),
		types: make(map[string]string),
		meta:  make(map[string]Sample),
	}
}

// SetConstantLabels attaches labels to every series the registry holds.
// Called once at startup, before anything is recorded.
func (r *Registry) SetConstantLabels(labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.constant = labels
}

// merge folds the constant labels in. Called with the lock held.
func (r *Registry) merge(labels map[string]string) map[string]string {
	if len(r.constant) == 0 {
		return labels
	}
	out := make(map[string]string, len(labels)+len(r.constant))
	for k, v := range labels {
		out[k] = v
	}
	// Constant labels win, so a caller cannot accidentally shadow the vantage.
	for k, v := range r.constant {
		out[k] = v
	}
	return out
}

func (r *Registry) register(name, typ, help string) {
	if _, ok := r.types[name]; !ok {
		r.types[name] = typ
		r.help[name] = help
		r.order = append(r.order, name)
	}
}

// SetGauge sets a gauge series to value.
func (r *Registry) SetGauge(name, help string, value float64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.register(name, "gauge", help)
	labels = r.merge(labels)
	key := seriesKey(name, labels)
	r.vals[key] = value
	r.remember(key, name, labels)
}

// IncCounter increments a counter series by one.
func (r *Registry) IncCounter(name, help string, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.register(name, "counter", help)
	labels = r.merge(labels)
	key := seriesKey(name, labels)
	r.vals[key]++
	r.remember(key, name, labels)
}

// remember stores the structured form of a series. Called with the lock held.
func (r *Registry) remember(key, name string, labels map[string]string) {
	if _, ok := r.meta[key]; ok {
		return
	}
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	r.meta[key] = Sample{Name: name, Labels: cp}
}

// Snapshot returns every series with its labels and current value.
func (r *Registry) Snapshot() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sample, 0, len(r.vals))
	for key, v := range r.vals {
		s := r.meta[key]
		s.Value = v
		if s.Name == "" {
			s.Name = key // a series recorded before meta existed; should not happen
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DropLabel removes every series carrying label=value, and reports how many
// went.
//
// A prober that stops watching a target must stop reporting on it too.
// Otherwise its last reading stands forever: probe_up frozen at 0 for a stream
// nobody asked about any more, which the shipped alert rules quite reasonably
// treat as an outage. A series that is simply absent is how Prometheus is told
// there is nothing to say.
func (r *Registry) DropLabel(label, value string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	dropped := 0
	for key, s := range r.meta {
		if s.Labels[label] != value {
			continue
		}
		delete(r.meta, key)
		delete(r.vals, key)
		dropped++
	}
	// r.order and the help text are left alone: they are per metric name, not
	// per series, and the name will be used again by the next target.
	return dropped
}

// Handler serves the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		for _, name := range r.order {
			fmt.Fprintf(w, "# HELP %s %s\n", name, r.help[name])
			fmt.Fprintf(w, "# TYPE %s %s\n", name, r.types[name])
			keys := make([]string, 0)
			for k := range r.vals {
				if k == name || strings.HasPrefix(k, name+"{") {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "%s %s\n", k, strconv.FormatFloat(r.vals[k], 'g', -1, 64))
			}
		}
	})
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}
	b.WriteByte('}')
	return b.String()
}
