package mosaic

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeRunner stands in for a Grabber so the pool can be tested without ffmpeg.
type fakeRunner struct {
	id, url string
	log     *runLog
}

type runLog struct {
	mu      sync.Mutex
	started []string
	ended   []string
	live    map[string]bool
}

func newRunLog() *runLog { return &runLog{live: map[string]bool{}} }

func (l *runLog) snapshotLive() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []string{}
	for k, v := range l.live {
		if v {
			out = append(out, k)
		}
	}
	return out
}

func (r *fakeRunner) Run(ctx context.Context) {
	key := r.id + "@" + r.url
	r.log.mu.Lock()
	r.log.started = append(r.log.started, key)
	r.log.live[key] = true
	r.log.mu.Unlock()

	<-ctx.Done()

	r.log.mu.Lock()
	r.log.ended = append(r.log.ended, key)
	r.log.live[key] = false
	r.log.mu.Unlock()
}

func newTestPool() (*Pool, *runLog) {
	log := newRunLog()
	return NewPool(func(id string, s Stream) Runner {
		return &fakeRunner{id: id, url: s.URL, log: log}
	}), log
}

// live builds the map Sync takes, for the common case where only the URLs
// matter to the test.
func live(pairs map[string]string) map[string]Stream {
	out := make(map[string]Stream, len(pairs))
	for id, url := range pairs {
		out[id] = Stream{URL: url, Live: true}
	}
	return out
}

func waitLive(t *testing.T, log *runLog, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(log.snapshotLive()) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected %d live grabbers, have %v", want, log.snapshotLive())
}

func TestPoolStartsOneGrabberPerTarget(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Stop()

	started, stopped := pool.Sync(ctx, live(map[string]string{"a": "u1", "b": "u2"}))
	if len(started) != 2 || len(stopped) != 0 {
		t.Fatalf("started %v stopped %v", started, stopped)
	}
	waitLive(t, log, 2)
	if got := pool.Running(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("running = %v", got)
	}
}

// Polling the prober every two seconds must not restart ffmpeg every two
// seconds: an unchanged target list is not a change.
func TestPoolLeavesUnchangedTargetsAlone(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Stop()

	want := live(map[string]string{"a": "u1", "b": "u2"})
	pool.Sync(ctx, want)
	waitLive(t, log, 2)

	started, stopped := pool.Sync(ctx, want)
	if len(started) != 0 || len(stopped) != 0 {
		t.Fatalf("a repeat sync churned grabbers: started %v stopped %v", started, stopped)
	}
	log.mu.Lock()
	n := len(log.started)
	log.mu.Unlock()
	if n != 2 {
		t.Errorf("%d grabbers were started for two targets", n)
	}
}

func TestPoolStopsGrabbersForRemovedTargets(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Stop()

	pool.Sync(ctx, live(map[string]string{"a": "u1", "b": "u2"}))
	waitLive(t, log, 2)

	_, stopped := pool.Sync(ctx, live(map[string]string{"a": "u1"}))
	if len(stopped) != 1 || stopped[0] != "b" {
		t.Fatalf("stopped = %v, want [b]", stopped)
	}
	waitLive(t, log, 1)
	if got := pool.Running(); len(got) != 1 || got[0] != "a" {
		t.Errorf("running = %v, want [a]", got)
	}
}

// Repointing a target at a different stream must restart its grabber, or the
// wall keeps showing the old picture under the new name.
func TestPoolRestartsAGrabberWhenTheURLChanges(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Stop()

	pool.Sync(ctx, live(map[string]string{"a": "u1"}))
	waitLive(t, log, 1)

	started, stopped := pool.Sync(ctx, live(map[string]string{"a": "u2"}))
	if len(started) != 1 || len(stopped) != 1 {
		t.Fatalf("started %v stopped %v, want the grabber replaced", started, stopped)
	}
	// Waiting on the count alone would pass while the old grabber is still
	// winding down and the new one has not started.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		log.mu.Lock()
		ok := log.live["a@u2"] && !log.live["a@u1"]
		log.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("the grabber is not following the new URL: live %v", log.snapshotLive())
}

// Shutdown must not leave ffmpeg processes behind, which means waiting for
// each grabber to actually exit rather than only cancelling it.
func TestPoolStopWaitsForGrabbersToExit(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Sync(ctx, live(map[string]string{"a": "u1", "b": "u2", "c": "u3"}))
	waitLive(t, log, 3)

	pool.Stop()

	if got := log.snapshotLive(); len(got) != 0 {
		t.Errorf("Stop returned with %v still running", got)
	}
	if got := pool.Running(); len(got) != 0 {
		t.Errorf("pool still lists %v", got)
	}
}

// Cancelling the context the pool was synced with stops everything, so a
// shutdown path that only cancels still cleans up.
func TestCancellingTheContextStopsGrabbers(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())

	pool.Sync(ctx, live(map[string]string{"a": "u1"}))
	waitLive(t, log, 1)

	cancel()
	waitLive(t, log, 0)
}

// A live event that ends becomes VOD, and the two are read with different
// ffmpeg flags, so the grabber has to be replaced even though the URL has not
// changed.
func TestPoolRestartsWhenLivenessChanges(t *testing.T) {
	pool, log := newTestPool()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer pool.Stop()

	pool.Sync(ctx, map[string]Stream{"a": {URL: "u1", Live: true}})
	waitLive(t, log, 1)

	started, stopped := pool.Sync(ctx, map[string]Stream{"a": {URL: "u1", Live: false}})
	if len(started) != 1 || len(stopped) != 1 {
		t.Fatalf("started %v stopped %v, want the grabber replaced", started, stopped)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		log.mu.Lock()
		n := len(log.started)
		log.mu.Unlock()
		if n == 2 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("the grabber was not replaced when the target stopped being live")
}
