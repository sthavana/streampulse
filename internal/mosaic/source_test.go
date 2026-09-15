package mosaic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// stateServer serves whatever /api/state body it currently holds.
type stateServer struct {
	mu    sync.Mutex
	body  string
	code  int
	hits  int
	srv   *httptest.Server
	block chan struct{}
}

func newStateServer(body string) *stateServer {
	s := &stateServer{body: body, code: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		body, code, block := s.body, s.code, s.block
		s.hits++
		s.mu.Unlock()
		if block != nil {
			<-block
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	return s
}

func (s *stateServer) set(body string) {
	s.mu.Lock()
	s.body = body
	s.mu.Unlock()
}

func (s *stateServer) fail(code int) {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
}

func (s *stateServer) url() string { return s.srv.URL }
func (s *stateServer) close()      { s.srv.Close() }

// poll runs one fetch-and-apply cycle, which is what each tick of Run does.
func poll(t *testing.T, src *Source) map[string]Stream {
	t.Helper()
	state, err := src.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return state.apply(src.Store)
}

const twoTargets = `{
  "targets": [
    {"name":"news","url":"https://cdn/news.m3u8","up":true,
     "metrics":{"manifest_age_seconds":4,"playlist_window_seconds":600},
     "streams":[{"metrics":{"segment_ttfb_seconds":0.08}},
                {"metrics":{"segment_ttfb_seconds":0.19}}]},
    {"name":"sport","url":"https://cdn/sport.mpd","up":true,
     "metrics":{},"streams":[]}
  ],
  "incidents": []
}`

// The wall needs no config of its own: the prober's state document already
// carries the target list and where each one lives.
func TestTargetsAndURLsComeFromTheProber(t *testing.T) {
	srv := newStateServer(twoTargets)
	defer srv.close()
	store := New()
	src := &Source{ProberURL: srv.url(), Store: store}

	streams := poll(t, src)
	if streams["news"].URL != "https://cdn/news.m3u8" || streams["sport"].URL != "https://cdn/sport.mpd" {
		t.Fatalf("streams = %v", streams)
	}
	got := store.Targets()
	if len(got) != 2 || got[0].ID != "news" || got[1].ID != "sport" {
		t.Fatalf("tiles = %+v, want the prober's order", got)
	}
}

// Worst incident per target wins the tile: a target with a critical and a
// warning open is critical, whichever arrived last.
func TestWorstIncidentWinsTheTile(t *testing.T) {
	srv := newStateServer(`{
	  "targets": [{"name":"news","url":"u","up":true,"metrics":{},"streams":[]}],
	  "incidents": [
	    {"severity":"critical","target":"news","message":"edge frozen 40s"},
	    {"severity":"warning","target":"news","message":"cache stale"},
	    {"severity":"info","target":"news","message":"2 periods"}
	  ]}`)
	defer srv.close()
	store := New()
	poll(t, &Source{ProberURL: srv.url(), Store: store})

	st := store.Snapshot()[0]
	if st.SeverityStr != "critical" {
		t.Errorf("severity = %s, want critical", st.SeverityStr)
	}
	if st.Summary != "edge frozen 40s" {
		t.Errorf("summary = %q, want the critical one", st.Summary)
	}
}

// for_seconds can hold a finding back, and a black tile with a green border is
// the single worst thing a multiviewer can show.
func TestAnUnreachableTargetIsCriticalWithoutAnIncident(t *testing.T) {
	srv := newStateServer(`{
	  "targets": [{"name":"news","url":"u","up":false,"metrics":{},"streams":[]}],
	  "incidents": []}`)
	defer srv.close()
	store := New()
	poll(t, &Source{ProberURL: srv.url(), Store: store})

	st := store.Snapshot()[0]
	if st.SeverityStr != "critical" {
		t.Errorf("severity = %s, want critical for a target the prober cannot reach", st.SeverityStr)
	}
	if st.Summary == "" {
		t.Error("an unreachable tile says nothing about why")
	}
}

// A target the prober has not managed to probe yet is not a target it has
// probed and found down.
func TestUnprobedIsNotCritical(t *testing.T) {
	srv := newStateServer(`{
	  "targets": [{"name":"news","url":"u","metrics":{},"streams":[]}],
	  "incidents": []}`)
	defer srv.close()
	store := New()
	poll(t, &Source{ProberURL: srv.url(), Store: store})

	if st := store.Snapshot()[0]; st.SeverityStr != "ok" {
		t.Errorf("severity = %s, want ok for a target with no verdict yet", st.SeverityStr)
	}
}

// One row of text per tile, so per-variant numbers roll up: the worst TTFB of
// the ladder is the one worth seeing from across a room.
func TestVariantMetricsRollUpToTheWorst(t *testing.T) {
	srv := newStateServer(twoTargets)
	defer srv.close()
	store := New()
	poll(t, &Source{ProberURL: srv.url(), Store: store})

	st := store.Snapshot()[0]
	if st.TTFB != 0.19 {
		t.Errorf("ttfb = %v, want the worst variant's 0.19", st.TTFB)
	}
	if st.EdgeAge != 4 {
		t.Errorf("edge age = %v, want 4", st.EdgeAge)
	}
}

// The prober re-reads its config while running, so targets appear and vanish
// without a restart and the wall has to follow.
func TestTheWallFollowsTheProbersTargetList(t *testing.T) {
	srv := newStateServer(twoTargets)
	defer srv.close()
	store := New()
	src := &Source{ProberURL: srv.url(), Store: store}
	poll(t, src)

	srv.set(`{"targets":[{"name":"sport","url":"https://cdn/sport.mpd","up":true,"metrics":{},"streams":[]},
	                     {"name":"kids","url":"https://cdn/kids.m3u8","up":true,"metrics":{},"streams":[]}],
	          "incidents":[]}`)
	streams := poll(t, src)

	if _, gone := streams["news"]; gone {
		t.Error("a removed target is still being grabbed")
	}
	got := store.Targets()
	if len(got) != 2 || got[0].ID != "sport" || got[1].ID != "kids" {
		t.Fatalf("tiles = %+v, want sport then kids", got)
	}
}

// A tile that survives a reload keeps its picture: rebuilding it would restart
// the MJPEG stream and blink the wall.
func TestSurvivingTilesKeepTheirFrames(t *testing.T) {
	srv := newStateServer(twoTargets)
	defer srv.close()
	store := New()
	src := &Source{ProberURL: srv.url(), Store: store}
	poll(t, src)
	store.PushFrame("sport", []byte("\xff\xd8frame\xff\xd9"))

	srv.set(`{"targets":[{"name":"sport","url":"https://cdn/sport.mpd","up":true,"metrics":{},"streams":[]}],
	          "incidents":[]}`)
	poll(t, src)

	ch, cancel := store.SubscribeFrames("sport")
	defer cancel()
	select {
	case got := <-ch:
		if string(got) != "\xff\xd8frame\xff\xd9" {
			t.Errorf("frame = %q", got)
		}
	default:
		t.Error("the surviving tile lost its picture across a target-list change")
	}
}

// A fault that clears must clear on the wall. Health is replaced wholesale
// every poll rather than accumulated, so this needs no expiry rule.
func TestHealthClearsWhenTheIncidentDoes(t *testing.T) {
	srv := newStateServer(`{
	  "targets":[{"name":"news","url":"u","up":true,"metrics":{},"streams":[]}],
	  "incidents":[{"severity":"critical","target":"news","message":"edge frozen"}]}`)
	defer srv.close()
	store := New()
	src := &Source{ProberURL: srv.url(), Store: store}
	poll(t, src)
	if store.Snapshot()[0].SeverityStr != "critical" {
		t.Fatal("setup: the tile did not go critical")
	}

	srv.set(`{"targets":[{"name":"news","url":"u","up":true,"metrics":{},"streams":[]}],"incidents":[]}`)
	poll(t, src)

	st := store.Snapshot()[0]
	if st.SeverityStr != "ok" {
		t.Errorf("severity = %s, want ok once the incident cleared", st.SeverityStr)
	}
	if st.Summary != "" {
		t.Errorf("summary = %q, want it gone with the incident", st.Summary)
	}
}

// A wall that blanks itself when its data source hiccups is worse than one
// that says how old its answer is.
func TestAnUnreachableProberLeavesTheLastHealthStanding(t *testing.T) {
	srv := newStateServer(`{
	  "targets":[{"name":"news","url":"u","up":true,"metrics":{},"streams":[]}],
	  "incidents":[{"severity":"warning","target":"news","message":"cache stale"}]}`)
	defer srv.close()
	store := New()
	src := &Source{ProberURL: srv.url(), Store: store, Every: time.Millisecond}
	poll(t, src)

	// Driven through Run rather than by marking the store by hand: the point
	// is that the poll loop reacts to an unreachable prober at all.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)
	srv.fail(http.StatusBadGateway)
	waitFor(t, func() bool { return store.Snapshot()[0].Stale },
		"the poll loop did not mark the wall stale when the prober failed")

	st := store.Snapshot()[0]
	if !st.Stale {
		t.Error("the tile is not marked stale")
	}
	if st.SeverityStr != "warning" || st.Summary != "cache stale" {
		t.Errorf("tile lost its last known health: %+v", st)
	}
}

// A body that is not prober state -- a proxy's error page served with a 200 --
// must be an error, not an empty wall.
func TestNonStateBodyIsAnError(t *testing.T) {
	srv := newStateServer(`<html>gateway timeout</html>`)
	defer srv.close()
	src := &Source{ProberURL: srv.url(), Store: New()}

	if _, err := src.fetch(context.Background()); err == nil {
		t.Fatal("an HTML error page parsed as prober state")
	}
}

// The grabbers are only disturbed when the set of targets or their URLs
// actually changes, not on every poll.
func TestOnTargetsFiresOnlyOnChange(t *testing.T) {
	srv := newStateServer(twoTargets)
	defer srv.close()

	var mu sync.Mutex
	calls := 0
	src := &Source{
		ProberURL: srv.url(), Store: New(), Every: 5 * time.Millisecond,
		OnTargets: func(map[string]Stream) { mu.Lock(); calls++; mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go src.Run(ctx)

	// Several polls of an unchanging prober.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	afterSteady := calls
	mu.Unlock()
	if afterSteady != 1 {
		cancel()
		t.Fatalf("OnTargets called %d times for an unchanging target list, want 1", afterSteady)
	}

	srv.set(`{"targets":[{"name":"news","url":"https://cdn/NEW.m3u8","up":true,"metrics":{},"streams":[]}],
	          "incidents":[]}`)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 },
		"OnTargets did not fire when a target URL changed")
	cancel()
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// A target the prober stops mentioning must stop being described. Leaving its
// last health standing would show a tile that is no longer being probed as
// though someone were still watching it.
func TestHealthOfAVanishedTargetDoesNotLinger(t *testing.T) {
	store := New()
	store.Sync([]TargetInfo{{ID: "news", Name: "news"}, {ID: "sport", Name: "sport"}})
	store.SetHealth(map[string]Health{
		"news":  {Severity: SevCritical, Summary: "edge frozen"},
		"sport": {Severity: SevOK},
	})

	// The next poll knows nothing about news -- it was removed from the
	// prober's config, but its tile is still on the wall until the list
	// refreshes.
	store.SetHealth(map[string]Health{"sport": {Severity: SevOK}})

	for _, st := range store.Snapshot() {
		if st.TargetID != "news" {
			continue
		}
		if st.SeverityStr != "ok" || st.Summary != "" {
			t.Errorf("a target the prober no longer reports kept its health: %+v", st)
		}
	}
}

// Whether a target is live is the prober's answer, and the wall needs it to
// decide how to read the stream.
func TestLivenessReachesTheGrabber(t *testing.T) {
	srv := newStateServer(`{
	  "targets":[
	    {"name":"vod","url":"https://cdn/vod.m3u8","up":true,"live":false,"metrics":{},"streams":[]},
	    {"name":"live","url":"https://cdn/live.m3u8","up":true,"live":true,"metrics":{},"streams":[]},
	    {"name":"unknown","url":"https://cdn/x.m3u8","up":true,"metrics":{},"streams":[]}
	  ],"incidents":[]}`)
	defer srv.close()

	got := poll(t, &Source{ProberURL: srv.url(), Store: New()})
	if got["vod"].Live {
		t.Error("a VOD target was passed to the grabber as live")
	}
	if !got["live"].Live {
		t.Error("a live target was passed to the grabber as VOD")
	}
	// Pacing a live stream is harmless; not pacing a VOD one is the bug.
	if !got["unknown"].Live {
		t.Error("a target with no verdict was treated as VOD, the riskier guess")
	}
}

// A target that flips between live and VOD -- a live event that ends -- must
// restart its grabber, because the two are read with different ffmpeg flags.
func TestALivenessChangeRestartsTheGrabber(t *testing.T) {
	srv := newStateServer(`{"targets":[{"name":"ev","url":"u","up":true,"live":true,"metrics":{},"streams":[]}],
	                        "incidents":[]}`)
	defer srv.close()

	var mu sync.Mutex
	var seen []Stream
	src := &Source{
		ProberURL: srv.url(), Store: New(), Every: 5 * time.Millisecond,
		OnTargets: func(s map[string]Stream) { mu.Lock(); seen = append(seen, s["ev"]); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 1 }, "no initial target set")

	srv.set(`{"targets":[{"name":"ev","url":"u","up":true,"live":false,"metrics":{},"streams":[]}],
	          "incidents":[]}`)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 2 && !seen[1].Live
	}, "the event ending did not reach the grabber")
}
