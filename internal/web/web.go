// Package web serves a read-only operator view of what the prober is seeing
// right now.
//
// It deliberately does not graph anything. Prometheus and Grafana already do
// that better than this ever will, and the stack in deploy/ wires them up. The
// question this answers is the different one you have while pointing the tool
// at a stream for the first time: what did it find, what is it probing, and is
// anything broken this second.
package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/inspect"
	"streampulse/internal/metrics"
)

//go:embed index.html
var assets embed.FS

// Sources are the read-only views the page is built from.
type Sources struct {
	Registry *metrics.Registry
	Tracker  *alert.Tracker
	Recorder *alert.Recorder
	// Frames is the latest picture and audio level per stream, when frame
	// capture is enabled. Nil is fine and simply means no pictures.
	Frames  *inspect.Frames
	Targets []config.Target
	Started time.Time
	Version string
}

// State is the whole page, as JSON.
type State struct {
	Now       time.Time        `json:"now"`
	UptimeSec float64          `json:"uptime_seconds"`
	Version   string           `json:"version,omitempty"`
	Summary   Summary          `json:"summary"`
	Targets   []Target         `json:"targets"`
	Incidents []alert.Incident `json:"incidents"`
	Recent    []alert.Finding  `json:"recent"`
}

type Summary struct {
	Targets    int `json:"targets"`
	TargetsUp  int `json:"targets_up"`
	Streams    int `json:"streams"`
	StreamsUp  int `json:"streams_up"`
	Firing     int `json:"firing"`
	Suppressed int `json:"suppressed"`
}

// Target is one configured stream and everything known about it.
type Target struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Format   string   `json:"format"`
	Interval string   `json:"interval"`
	Up       *bool    `json:"up,omitempty"`
	Live     *bool    `json:"live,omitempty"`
	Metrics  Values   `json:"metrics"`
	Streams  []Stream `json:"streams"`
	Firing   int      `json:"firing"`
}

// Stream is one variant, rendition or representation of a target.
type Stream struct {
	Name    string `json:"name"`
	Up      *bool  `json:"up,omitempty"`
	Live    *bool  `json:"live,omitempty"`
	Metrics Values `json:"metrics"`
	Firing  int    `json:"firing"`
	// Thumb is the URL of the latest captured frame, empty when there is
	// none. It carries the capture time so a browser fetches the new picture
	// rather than the one it already has.
	Thumb string `json:"thumb,omitempty"`
	// Audio is the level of the sampled segment, absent when the stream has
	// no audio or capture is off.
	Audio *AudioLevel `json:"audio,omitempty"`
}

// AudioLevel is a measurement, not a verdict: whether a level is a fault
// depends on the programme, so the number is reported and the judgement left
// to whoever knows it.
type AudioLevel struct {
	PeakDBFS float64 `json:"peak_dbfs"`
	MeanDBFS float64 `json:"mean_dbfs"`
	Silent   bool    `json:"silent"`
}

// Values is the metric set for one row, keyed by the metric name with the
// streampulse_ prefix stripped.
type Values map[string]float64

// Handler serves the UI at / and its data at /api/state.
func Handler(s Sources) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(Build(s))
	})
	mux.HandleFunc("/api/frame", func(w http.ResponseWriter, r *http.Request) {
		fr, ok := s.Frames.Get(inspect.FrameKey(r.URL.Query().Get("target"), r.URL.Query().Get("variant")))
		if !ok || len(fr.JPEG) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		// The URL carries the capture time, so the picture at it never
		// changes and may be cached hard.
		w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
		_, _ = w.Write(fr.JPEG)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := assets.ReadFile("index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	return mux
}

// Build assembles the page state from the metric registry and the tracker.
//
// The registry is the single source of truth for observations rather than a
// second copy kept for the UI: two records of the same thing drift, and the
// one on the dashboard would be the one nobody notices is wrong.
func Build(s Sources) State {
	now := time.Now().UTC()
	// Empty slices, never nil: they cross into JSON as [] rather than null,
	// and a page that has to defend against null on every list is a page that
	// will miss one.
	st := State{
		Now: now, Version: s.Version,
		Targets:   []Target{},
		Incidents: []alert.Incident{},
		Recent:    []alert.Finding{},
	}
	if !s.Started.IsZero() {
		st.UptimeSec = now.Sub(s.Started).Seconds()
	}

	// target -> variant -> metric name -> value. The empty variant is the
	// target's own row.
	byTarget := map[string]map[string]Values{}
	for _, sample := range s.Registry.Snapshot() {
		target := sample.Labels["target"]
		if target == "" {
			continue
		}
		variant := sample.Labels["variant"]
		if byTarget[target] == nil {
			byTarget[target] = map[string]Values{}
		}
		if byTarget[target][variant] == nil {
			byTarget[target][variant] = Values{}
		}
		byTarget[target][variant][strings.TrimPrefix(sample.Name, "streampulse_")] = sample.Value
	}

	if s.Tracker != nil {
		if inc := s.Tracker.Incidents(); inc != nil {
			st.Incidents = inc
		}
	}
	firingByTarget := map[string]int{}
	firingByStream := map[string]int{}
	for _, inc := range st.Incidents {
		firingByTarget[inc.Target]++
		firingByStream[inc.Target+"\x00"+inc.Variant]++
		if inc.Announced {
			st.Summary.Firing++
		} else {
			st.Summary.Suppressed++
		}
	}

	for _, t := range s.Targets {
		rows := byTarget[t.Name]
		tv := Target{
			Name: t.Name, URL: t.URL, Format: format(t), Interval: t.Interval().String(),
			Metrics: rows[""], Firing: firingByTarget[t.Name],
			Streams: []Stream{},
		}
		if tv.Metrics == nil {
			tv.Metrics = Values{}
		}
		tv.Up = boolOf(tv.Metrics, "probe_up")

		for variant, vals := range rows {
			if variant == "" {
				continue
			}
			stream := Stream{
				Name: variant, Metrics: vals,
				Firing: firingByStream[t.Name+"\x00"+variant],
				Up:     boolOf(vals, "variant_up"),
				Live:   boolOf(vals, "stream_live"),
			}
			// DASH has no per-representation manifest to be up or down; if the
			// target is reachable and the representation has segments, it is
			// as up as the format allows it to be.
			if stream.Up == nil && vals["segment_count"] > 0 {
				up := true
				stream.Up = &up
			}
			if s.Frames != nil {
				if fr, ok := s.Frames.Get(inspect.FrameKey(t.Name, variant)); ok {
					if len(fr.JPEG) > 0 {
						stream.Thumb = "api/frame?target=" + url.QueryEscape(t.Name) +
							"&variant=" + url.QueryEscape(variant) +
							"&t=" + strconv.FormatInt(fr.At.UnixNano(), 10)
					}
					if fr.HasAudio {
						stream.Audio = &AudioLevel{
							PeakDBFS: fr.PeakDBFS, MeanDBFS: fr.MeanDBFS, Silent: fr.Silent(),
						}
					}
				}
			}
			tv.Streams = append(tv.Streams, stream)
		}
		sort.Slice(tv.Streams, func(i, j int) bool { return tv.Streams[i].Name < tv.Streams[j].Name })

		// A target's liveness is its streams': they all come from one manifest.
		for _, stream := range tv.Streams {
			if stream.Live != nil {
				tv.Live = stream.Live
				break
			}
		}

		st.Summary.Targets++
		if tv.Up != nil && *tv.Up {
			st.Summary.TargetsUp++
		}
		st.Summary.Streams += len(tv.Streams)
		for _, stream := range tv.Streams {
			if stream.Up != nil && *stream.Up {
				st.Summary.StreamsUp++
			}
		}
		st.Targets = append(st.Targets, tv)
	}
	sort.Slice(st.Targets, func(i, j int) bool { return st.Targets[i].Name < st.Targets[j].Name })

	if s.Recorder != nil {
		if recent := s.Recorder.Recent(); recent != nil {
			st.Recent = recent
		}
	}
	return st
}

// format renders the manifest format for display, resolving the empty
// "detect it from the body" case to what was actually detected.
func format(t config.Target) string {
	if f := strings.ToLower(t.Type); f != "" {
		return f
	}
	return "auto"
}

// boolOf reads a 0/1 gauge, distinguishing "not observed yet" from "false".
func boolOf(v Values, name string) *bool {
	raw, ok := v[name]
	if !ok {
		return nil
	}
	b := raw != 0
	return &b
}
