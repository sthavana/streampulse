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
}

func New() *Registry {
	return &Registry{
		vals:  make(map[string]float64),
		help:  make(map[string]string),
		types: make(map[string]string),
	}
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
	r.vals[seriesKey(name, labels)] = value
}

// IncCounter increments a counter series by one.
func (r *Registry) IncCounter(name, help string, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.register(name, "counter", help)
	r.vals[seriesKey(name, labels)]++
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
