// Package supervisor runs one probing goroutine per target and can change the
// set while the process keeps running.
//
// It exists so that adding a stream does not mean restarting the prober.
// A restart is not free for a monitoring tool: it drops every in-flight probe,
// re-derives the cross-poll state that freeze and rollback detection depend
// on, and — before incident state was persisted — re-announced every open
// fault. None of that should be the price of watching one more channel.
package supervisor

import (
	"context"
	"log"
	"reflect"
	"sort"
	"sync"
	"time"

	"streampulse/internal/config"
)

// Prober is the part of the prober this needs, declared here so the package
// can be tested without one.
type Prober interface {
	ProbeTarget(ctx context.Context, t config.Target)
}

// probeTimeout bounds one cycle, so a hung origin cannot stall a target's
// ticker indefinitely.
const probeTimeout = 30 * time.Second

type Supervisor struct {
	prober Prober

	mu      sync.Mutex
	running map[string]*runner
}

type runner struct {
	target config.Target
	cancel context.CancelFunc
	done   chan struct{}
}

func New(p Prober) *Supervisor {
	return &Supervisor{prober: p, running: map[string]*runner{}}
}

// Changes is what one Sync did, for a log line.
type Changes struct {
	Added   []string
	Removed []string
	Changed []string
}

func (c Changes) Empty() bool {
	return len(c.Added) == 0 && len(c.Removed) == 0 && len(c.Changed) == 0
}

func (c Changes) String() string {
	parts := make([]string, 0, 3)
	for label, names := range map[string][]string{
		"added": c.Added, "removed": c.Removed, "changed": c.Changed,
	} {
		if len(names) > 0 {
			parts = append(parts, label+" "+join(names))
		}
	}
	sort.Strings(parts)
	return join(parts)
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// Sync makes the running set match targets: starting what is new, stopping
// what has gone, and restarting what has changed.
//
// A target is identified by its name, which is why the config refuses
// duplicates: two targets sharing one would also share their metric series,
// and here one would silently replace the other.
func (s *Supervisor) Sync(ctx context.Context, targets []config.Target) Changes {
	var ch Changes

	wanted := make(map[string]config.Target, len(targets))
	for _, t := range targets {
		wanted[t.Name] = t
	}

	s.mu.Lock()
	// Stop what is gone or has changed. A changed target is stopped and
	// started rather than mutated: its ticker interval, its URL and its check
	// configuration are all read once when the goroutine starts, and patching
	// a running one would leave it half old and half new.
	var stopping []*runner
	for name, r := range s.running {
		want, still := wanted[name]
		switch {
		case !still:
			ch.Removed = append(ch.Removed, name)
		case !reflect.DeepEqual(want, r.target):
			ch.Changed = append(ch.Changed, name)
		default:
			continue
		}
		r.cancel()
		stopping = append(stopping, r)
		delete(s.running, name)
	}
	s.mu.Unlock()

	// Waited on outside the lock: a probe in flight can take up to the probe
	// timeout to notice its context, and holding the lock for that would block
	// every other caller including the web UI.
	for _, r := range stopping {
		<-r.done
	}

	s.mu.Lock()
	for name, t := range wanted {
		if _, ok := s.running[name]; ok {
			continue
		}
		if !contains(ch.Changed, name) {
			ch.Added = append(ch.Added, name)
		}
		s.start(ctx, t)
	}
	s.mu.Unlock()

	sort.Strings(ch.Added)
	sort.Strings(ch.Removed)
	sort.Strings(ch.Changed)
	return ch
}

// start launches one target. Called with the lock held.
func (s *Supervisor) start(ctx context.Context, t config.Target) {
	tctx, cancel := context.WithCancel(ctx)
	r := &runner{target: t, cancel: cancel, done: make(chan struct{})}
	s.running[t.Name] = r
	go s.run(tctx, t, r.done)
}

func (s *Supervisor) run(ctx context.Context, t config.Target, done chan struct{}) {
	defer close(done)
	log.Printf("probing %q every %s: %s", t.Name, t.Interval(), t.URL)

	s.probeOnce(ctx, t) // immediately, then on the ticker
	ticker := time.NewTicker(t.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeOnce(ctx, t)
		}
	}
}

func (s *Supervisor) probeOnce(ctx context.Context, t config.Target) {
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	s.prober.ProbeTarget(c, t)
}

// Targets returns the target set as it is right now, sorted by name. The web
// UI reads this rather than the slice main loaded at startup, which goes stale
// the first time the config is reloaded.
func (s *Supervisor) Targets() []config.Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]config.Target, 0, len(s.running))
	for _, r := range s.running {
		out = append(out, r.target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Running lists the targets currently probing, sorted.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for name := range s.running {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Stop halts every target and waits for them to finish.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	stopping := make([]*runner, 0, len(s.running))
	for name, r := range s.running {
		r.cancel()
		stopping = append(stopping, r)
		delete(s.running, name)
	}
	s.mu.Unlock()

	for _, r := range stopping {
		<-r.done
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
