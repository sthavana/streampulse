package mosaic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Source keeps the wall in step with a prober by polling its /api/state.
//
// That endpoint is the whole integration. It already carries every target's
// name, URL, reachability and open incidents, which is everything a tile
// needs, so the wall needs no config file of its own and no access to the
// prober's internals: point it at an address and it discovers what to show.
// When the prober's target list changes -- someone edits its config, which it
// re-reads while running -- the wall follows within one poll.
type Source struct {
	// ProberURL is the base address of a prober, e.g. http://prober:9090.
	ProberURL string
	Every     time.Duration
	Store     *Store
	Client    *http.Client
	// OnTargets is called with name -> stream URL whenever the set changes,
	// for the caller to start and stop grabbers. Never called concurrently.
	OnTargets func(map[string]string)
	Logf      func(format string, args ...any)
}

func (s *Source) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Source) every() time.Duration {
	if s.Every <= 0 {
		return 2 * time.Second
	}
	return s.Every
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Run polls until ctx is cancelled. It returns no error: a prober that cannot
// be reached is a condition the wall displays, not one it dies of.
func (s *Source) Run(ctx context.Context) {
	t := time.NewTicker(s.every())
	defer t.Stop()
	var last map[string]string
	failing := false

	poll := func() {
		state, err := s.fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.Store.MarkStale()
			// Logged once per outage rather than once per poll: at a two
			// second interval the second form fills a day of logs with one
			// sentence.
			if !failing {
				s.log("mosaic: prober unreachable, tiles show their last known health: %v", err)
				failing = true
			}
			return
		}
		if failing {
			s.log("mosaic: prober reachable again")
			failing = false
		}
		urls := state.apply(s.Store)
		if s.OnTargets != nil && !sameURLs(last, urls) {
			s.OnTargets(urls)
			last = urls
		}
	}

	poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			poll()
		}
	}
}

func (s *Source) fetch(ctx context.Context) (*proberState, error) {
	url := strings.TrimRight(s.ProberURL, "/") + "/api/state"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	// Bounded: this is a document about a handful of targets, and a wall that
	// reads an unbounded body because something else is misconfigured has
	// stopped being a monitoring tool.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var st proberState
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("%s did not return prober state: %w", url, err)
	}
	return &st, nil
}

// proberState is the part of the prober's /api/state the wall reads. It is
// deliberately a subset: fields the wall does not use are not named here, so
// the prober can add to its API without this failing to parse.
type proberState struct {
	Targets []struct {
		Name    string             `json:"name"`
		URL     string             `json:"url"`
		Up      *bool              `json:"up"`
		Metrics map[string]float64 `json:"metrics"`
		Streams []struct {
			Metrics map[string]float64 `json:"metrics"`
		} `json:"streams"`
	} `json:"targets"`
	Incidents []struct {
		Severity string `json:"severity"`
		Target   string `json:"target"`
		Message  string `json:"message"`
	} `json:"incidents"`
}

// apply pushes the state into the store and returns the target URLs to grab.
func (s *proberState) apply(store *Store) map[string]string {
	infos := make([]TargetInfo, 0, len(s.Targets))
	urls := make(map[string]string, len(s.Targets))
	for _, t := range s.Targets {
		infos = append(infos, TargetInfo{ID: t.Name, Name: t.Name})
		urls[t.Name] = t.URL
	}
	store.Sync(infos)

	// Worst incident per target wins the tile. A target with a critical and a
	// warning open is critical; showing the most recent instead would let a
	// trailing info finding paint over an outage.
	worst := map[string]Severity{}
	summary := map[string]string{}
	for _, inc := range s.Incidents {
		sev := ParseSeverity(inc.Severity)
		if prev, seen := worst[inc.Target]; seen && prev >= sev {
			continue
		}
		worst[inc.Target] = sev
		summary[inc.Target] = inc.Message
	}

	health := make(map[string]Health, len(s.Targets))
	for _, t := range s.Targets {
		h := Health{
			Severity: worst[t.Name],
			Summary:  summary[t.Name],
			Up:       t.Up,
			EdgeAge:  t.Metrics["manifest_age_seconds"],
			Window:   t.Metrics["playlist_window_seconds"],
		}
		// A target the prober has probed and found unreachable is critical on
		// the wall whether or not an incident has opened yet: for_seconds can
		// hold a finding back, and a black tile with a green border is the
		// single worst thing a multiviewer can show.
		if t.Up != nil && !*t.Up && h.Severity < SevCritical {
			h.Severity = SevCritical
			if h.Summary == "" {
				h.Summary = "unreachable"
			}
		}
		// Per-variant numbers roll up to the tile by taking the worst, since
		// a wall has one row of text per target, not one per rendition.
		for _, v := range t.Streams {
			if ttfb := v.Metrics["segment_ttfb_seconds"]; ttfb > h.TTFB {
				h.TTFB = ttfb
			}
			if h.EdgeAge == 0 {
				h.EdgeAge = v.Metrics["manifest_age_seconds"]
			}
			if w := v.Metrics["playlist_window_seconds"]; h.Window == 0 || (w > 0 && w < h.Window) {
				h.Window = w
			}
		}
		health[t.Name] = h
	}
	store.SetHealth(health)
	return urls
}

func sameURLs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
