package supervisor

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"streampulse/internal/config"
)

// counter records which targets were probed and how often.
type counter struct {
	mu sync.Mutex
	n  map[string]int
	// block, when set for a target, holds its probe open until released and
	// deliberately ignores cancellation -- standing in for work already
	// committed, a syscall or an HTTP body being read, which does not stop
	// the instant a context is cancelled.
	block map[string]chan struct{}
}

func newCounter() *counter {
	return &counter{n: map[string]int{}, block: map[string]chan struct{}{}}
}

func (c *counter) ProbeTarget(ctx context.Context, t config.Target) {
	c.mu.Lock()
	c.n[t.Name]++
	held := c.block[t.Name]
	c.mu.Unlock()
	if held != nil {
		<-held
	}
}

func (c *counter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

func (c *counter) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.n))
	for k := range c.n {
		out = append(out, k)
	}
	return out
}

func target(name string, interval int) config.Target {
	return config.Target{Name: name, URL: "http://origin/" + name + ".m3u8", IntervalSeconds: interval}
}

// eventually waits for a condition rather than sleeping a guessed interval,
// which is how these tests stay quick and stop being flaky on a loaded CI box.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSyncStartsTargets(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	ch := s.Sync(ctx, []config.Target{target("a", 60), target("b", 60)})
	if len(ch.Added) != 2 {
		t.Fatalf("added = %v, want two", ch.Added)
	}
	// Each target probes once immediately rather than waiting out its first
	// tick, or adding a stream on a 60s interval would tell you nothing for
	// a minute.
	eventually(t, "both targets to probe", func() bool { return c.count("a") > 0 && c.count("b") > 0 })
	if got := s.Running(); len(got) != 2 {
		t.Errorf("running = %v", got)
	}
}

// The point of the whole package: a stream added to the config starts probing
// without the process restarting.
func TestSyncAddsWithoutDisturbingTheRest(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Sync(ctx, []config.Target{target("a", 60)})
	eventually(t, "a to probe", func() bool { return c.count("a") > 0 })
	before := c.count("a")

	ch := s.Sync(ctx, []config.Target{target("a", 60), target("b", 60)})
	if len(ch.Added) != 1 || ch.Added[0] != "b" {
		t.Fatalf("added = %v, want just b", ch.Added)
	}
	eventually(t, "b to probe", func() bool { return c.count("b") > 0 })

	// The untouched target must not have been restarted: a restart would
	// re-probe immediately and, in the real prober, throw away the cross-poll
	// state that freeze and rollback detection are built on.
	if after := c.count("a"); after != before {
		t.Errorf("an unchanged target was restarted: probed %d times, was %d", after, before)
	}
}

func TestSyncRemovesTargets(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Sync(ctx, []config.Target{target("a", 1), target("b", 1)})
	eventually(t, "both to probe", func() bool { return c.count("a") > 0 && c.count("b") > 0 })

	ch := s.Sync(ctx, []config.Target{target("a", 1)})
	if len(ch.Removed) != 1 || ch.Removed[0] != "b" {
		t.Fatalf("removed = %v", ch.Removed)
	}
	if got := s.Running(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("running = %v, want just a", got)
	}

	// Sync returns only once the goroutine is finished, so nothing probes
	// after it says the target is gone.
	stopped := c.count("b")
	time.Sleep(50 * time.Millisecond)
	if c.count("b") != stopped {
		t.Errorf("a removed target kept probing")
	}
}

// A changed target is stopped and started, because its interval, URL and
// check configuration are all read once when the goroutine starts.
func TestChangedTargetIsRestarted(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Sync(ctx, []config.Target{target("a", 60)})
	eventually(t, "a to probe", func() bool { return c.count("a") == 1 })

	changed := target("a", 60)
	changed.SegmentSample = 3
	ch := s.Sync(ctx, []config.Target{changed})
	if len(ch.Changed) != 1 || ch.Changed[0] != "a" {
		t.Fatalf("changed = %v", ch.Changed)
	}
	if len(ch.Added) != 0 {
		t.Errorf("a restart was also counted as an addition: %v", ch.Added)
	}
	eventually(t, "a to probe again after its restart", func() bool { return c.count("a") == 2 })
}

// Fields that are maps and slices are why the comparison is a deep one: a
// shallow == would not compile, and comparing only the scalars would miss a
// changed header or rendition filter.
func TestChangeDetectionSeesMapsAndSlices(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	first := target("a", 60)
	first.Headers = map[string]string{"X-Token": "one"}
	s.Sync(ctx, []config.Target{first})
	eventually(t, "a to probe", func() bool { return c.count("a") == 1 })

	second := target("a", 60)
	second.Headers = map[string]string{"X-Token": "two"}
	if ch := s.Sync(ctx, []config.Target{second}); len(ch.Changed) != 1 {
		t.Errorf("a changed header went unnoticed: %+v", ch)
	}

	// And an identical one is still a no-op.
	third := target("a", 60)
	third.Headers = map[string]string{"X-Token": "two"}
	if ch := s.Sync(ctx, []config.Target{third}); !ch.Empty() {
		t.Errorf("an identical target was treated as a change: %+v", ch)
	}
}

func TestSyncWithNoChangesDoesNothing(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	ts := []config.Target{target("a", 60), target("b", 60)}
	s.Sync(ctx, ts)
	eventually(t, "both to probe", func() bool { return c.count("a") > 0 && c.count("b") > 0 })

	if ch := s.Sync(ctx, ts); !ch.Empty() {
		t.Errorf("an unchanged config produced %+v", ch)
	}
}

// Stop cancels and then waits for the goroutines to unwind, rather than
// returning while they are still running. Cancellation is what makes that
// quick; the waiting is what makes it correct, and a probe that is slow to
// notice must still be waited for or the process exits mid-write.
func TestStopWaitsForGoroutinesToFinish(t *testing.T) {
	c := newCounter()
	held := make(chan struct{})
	c.block["slow"] = held

	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Sync(ctx, []config.Target{target("slow", 60)})
	eventually(t, "the slow probe to start", func() bool { return c.count("slow") > 0 })

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()

	select {
	case <-done:
		t.Fatal("Stop returned while a probe was still running")
	case <-time.After(80 * time.Millisecond):
	}

	close(held) // let the probe finish
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the probe finished")
	}
	if got := s.Running(); len(got) != 0 {
		t.Errorf("running after Stop = %v", got)
	}
}

// Cancelling the parent context stops everything, which is what shutdown does.
func TestParentCancellationStopsTargets(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())

	s.Sync(ctx, []config.Target{target("a", 1)})
	eventually(t, "a to probe", func() bool { return c.count("a") > 0 })

	cancel()
	s.Stop()
	stopped := c.count("a")
	time.Sleep(50 * time.Millisecond)
	if c.count("a") != stopped {
		t.Errorf("a target kept probing after the parent context was cancelled")
	}
}

func TestSyncToEmptyStopsEverything(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Sync(ctx, []config.Target{target("a", 60), target("b", 60)})
	eventually(t, "both to probe", func() bool { return len(c.names()) == 2 })

	ch := s.Sync(ctx, nil)
	if len(ch.Removed) != 2 {
		t.Errorf("removed = %v, want both", ch.Removed)
	}
	if got := s.Running(); len(got) != 0 {
		t.Errorf("running = %v, want none", got)
	}
}

func TestTargetsReflectsTheRunningSet(t *testing.T) {
	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Sync(ctx, []config.Target{target("b", 60), target("a", 60)})
	got := s.Targets()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("targets = %+v, want a then b", got)
	}
	if got[0].URL == "" {
		t.Error("the whole target should come back, not just the name")
	}
}

// Every target is a goroutine, and a Sync that churns them must not leave any
// behind: on a config someone edits all day, a leak per edit adds up.
func TestNoGoroutinesLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	c := newCounter()
	s := New(c)
	ctx, cancel := context.WithCancel(context.Background())

	for i := 0; i < 20; i++ {
		s.Sync(ctx, []config.Target{target("a", 60), target("b", 60), target("c", 60)})
		s.Sync(ctx, []config.Target{target("a", 60)})
		s.Sync(ctx, nil)
	}
	s.Sync(ctx, []config.Target{target("a", 60), target("b", 60)})
	s.Stop()
	cancel()

	eventually(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before+2 })
}
