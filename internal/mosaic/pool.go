package mosaic

import (
	"context"
	"sort"
	"sync"
)

// Stream is everything the wall needs in order to grab one target: where it
// is, and whether it is live. Both come from the prober, and both matter --
// see Grabber.args for what being live changes about the ffmpeg command.
type Stream struct {
	URL  string
	Live bool
}

// Pool keeps one Grabber running per target and follows the prober's target
// list as it changes.
//
// The same shape as the prober's own supervisor, for the same reason: a target
// added to the config should appear on the wall without a restart, and one
// removed should stop costing an ffmpeg process. Grabbers are compared by URL
// as well as by name, so repointing a target at a different stream restarts
// its grabber rather than leaving the old picture up.
type Pool struct {
	// New builds a grabber for one target. Injected so tests can run a pool
	// without ffmpeg.
	New func(id string, s Stream) Runner

	mu      sync.Mutex
	running map[string]*entry
}

// Runner is the part of a Grabber the pool needs, so that a test can supply
// something cheaper.
type Runner interface {
	Run(ctx context.Context)
}

type entry struct {
	stream Stream
	cancel context.CancelFunc
	done   chan struct{}
}

func NewPool(new func(id string, s Stream) Runner) *Pool {
	return &Pool{New: new, running: map[string]*entry{}}
}

// Sync makes the running grabbers match want (target id -> stream) and reports
// what it started and stopped.
func (p *Pool) Sync(ctx context.Context, want map[string]Stream) (started, stopped []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, e := range p.running {
		stream, keep := want[id]
		if keep && stream == e.stream {
			continue
		}
		// Gone, or pointed somewhere else. Either way this grabber is wrong.
		e.cancel()
		delete(p.running, id)
		stopped = append(stopped, id)
	}
	for id, stream := range want {
		if _, ok := p.running[id]; ok {
			continue
		}
		gctx, cancel := context.WithCancel(ctx)
		e := &entry{stream: stream, cancel: cancel, done: make(chan struct{})}
		p.running[id] = e
		runner := p.New(id, stream)
		go func() {
			defer close(e.done)
			runner.Run(gctx)
		}()
		started = append(started, id)
	}
	sort.Strings(started)
	sort.Strings(stopped)
	return started, stopped
}

// Running lists the target ids with a live grabber.
func (p *Pool) Running() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.running))
	for id := range p.running {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Stop cancels every grabber and waits for them to exit, so a shutdown does
// not leave ffmpeg processes behind.
func (p *Pool) Stop() {
	p.mu.Lock()
	entries := make([]*entry, 0, len(p.running))
	for id, e := range p.running {
		e.cancel()
		entries = append(entries, e)
		delete(p.running, id)
	}
	p.mu.Unlock()

	for _, e := range entries {
		<-e.done
	}
}
