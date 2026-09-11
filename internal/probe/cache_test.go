package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streampulse/internal/config"
	"streampulse/internal/metrics"
)

func TestReadCache(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantAge time.Duration
		hasAge  bool
		wantHit string
	}{
		{"nothing", nil, 0, false, ""},
		{"age only", map[string]string{"Age": "38"}, 38 * time.Second, true, ""},
		{"cloudfront", map[string]string{"Age": "0", "X-Cache": "Hit from cloudfront"}, 0, true, "HIT"},
		{"cloudfront miss", map[string]string{"X-Cache": "Miss from cloudfront"}, 0, false, "MISS"},
		{"akamai", map[string]string{"X-Cache": "TCP_HIT from a1-2-3.akamai.net"}, 0, false, "HIT"},
		{"akamai refresh miss", map[string]string{"X-Cache": "TCP_REFRESH_MISS from a1.akamai.net"}, 0, false, "MISS"},
		{"cloudflare", map[string]string{"CF-Cache-Status": "HIT"}, 0, false, "HIT"},
		// A CDN chain lists shield first, edge last; the edge is what served us.
		{"fastly chain", map[string]string{"X-Cache": "HIT, MISS"}, 0, false, "MISS"},
		// A node name containing "hit" must not turn a miss into a hit.
		{"miss from a node named hit", map[string]string{"X-Cache": "MISS from edge-hit01.cdn.example.net"}, 0, false, "MISS"},
		// Unified Streaming spells the header X-Cached, and nginx's UPDATING
		// means it is serving past its TTL while it revalidates -- both
		// observed on their live demo channel.
		{"unified streaming hit", map[string]string{"X-Cached": "HIT"}, 0, false, "HIT"},
		{"nginx updating", map[string]string{"X-Cached": "UPDATING"}, 0, false, "STALE"},
		{"nginx stale", map[string]string{"X-Cache-Status": "STALE"}, 0, false, "STALE"},
		{"nginx expired went to origin", map[string]string{"X-Cache-Status": "EXPIRED"}, 0, false, "MISS"},
		{"garbage age", map[string]string{"Age": "soon"}, 0, false, ""},
		{"negative age", map[string]string{"Age": "-5"}, 0, false, ""},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		got := readCache(h)
		if got.HasAge != c.hasAge || got.Age != c.wantAge {
			t.Errorf("%s: age = %v/%v, want %v/%v", c.name, got.Age, got.HasAge, c.wantAge, c.hasAge)
		}
		if got.Hit != c.wantHit {
			t.Errorf("%s: hit = %q, want %q", c.name, got.Hit, c.wantHit)
		}
	}
}

// The whole point of the feature: the same frozen manifest attributed to a
// different layer depending on how old the response was.
func TestCacheExplainAttributesTheLayer(t *testing.T) {
	frozen := 40 * time.Second

	edge := cacheInfo{Age: 45 * time.Second, HasAge: true, Hit: "HIT"}.explain(frozen)
	if !strings.Contains(edge, "check the origin") {
		t.Errorf("a response older than the freeze should point at the edge, got %q", edge)
	}

	origin := cacheInfo{Age: 2 * time.Second, HasAge: true, Hit: "MISS"}.explain(frozen)
	if !strings.Contains(origin, "origin is serving it") {
		t.Errorf("a response younger than the freeze should point at the origin, got %q", origin)
	}

	// An edge admitting it is past its TTL settles the question by itself.
	stale := cacheInfo{Hit: "STALE"}.explain(frozen)
	if !strings.Contains(stale, "serving expired content") {
		t.Errorf("STALE should be attributed to the edge, got %q", stale)
	}

	// Nothing to go on must stay silent rather than guess a layer.
	if got := (cacheInfo{}).explain(frozen); got != "" {
		t.Errorf("a response with no cache headers should say nothing, got %q", got)
	}
}

// A frozen playlist behind a stale edge and the same playlist fresh from the
// origin must produce findings that read differently.
func TestStallFindingNamesTheLayer(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:4.000,\nseg100.ts\n"

	probeFrozen := func(age string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if age != "" {
				w.Header().Set("Age", age)
				w.Header().Set("X-Cache", "Hit from cloudfront")
			}
			_, _ = w.Write([]byte(media))
		}))
		defer srv.Close()

		pr, clk := newTestProber(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		cap := &capture{}
		pr.notifier = cap
		tgt := config.Target{Name: "t", URL: srv.URL + "/media.m3u8"}

		pr.ProbeTarget(context.Background(), tgt)
		clk.advance(30 * time.Second) // past the 12s stall threshold
		pr.ProbeTarget(context.Background(), tgt)

		for _, f := range cap.findings {
			if f.Check == "playlist_stalled" {
				return f.Message
			}
		}
		return ""
	}

	stale := probeFrozen("120")
	if !strings.Contains(stale, "Age 120.0s") || !strings.Contains(stale, "check the origin") {
		t.Errorf("a 120s-old cached response should be attributed to the edge, got %q", stale)
	}
	fresh := probeFrozen("0")
	if !strings.Contains(fresh, "origin is serving it") {
		t.Errorf("a fresh response should be attributed to the origin, got %q", fresh)
	}
	none := probeFrozen("")
	if none == "" {
		t.Fatal("the stall itself should still be reported without cache headers")
	}
	if strings.Contains(none, "cache:") {
		t.Errorf("no cache headers should mean no cache claim, got %q", none)
	}
}

func TestCacheMetricsExported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Age", "17")
		w.Header().Set("X-Cache", "Hit from cloudfront")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()

	reg := metrics.New()
	pr := New(reg, &capture{})
	pr.ProbeTarget(context.Background(), config.Target{Name: "t", URL: srv.URL + "/media.m3u8"})

	body := scrape(t, reg)
	for _, want := range []string{
		`streampulse_manifest_age_seconds{target="t"} 17`,
		`streampulse_manifest_cache_hit{target="t"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// Probe traffic has to be identifiable in an origin's access log, and the
// no-cache option has to actually reach the wire.
func TestRequestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()

	pr := New(metrics.New(), &capture{})
	pr.ProbeTarget(context.Background(), config.Target{Name: "t", URL: srv.URL + "/media.m3u8"})
	if ua := got.Get("User-Agent"); !strings.HasPrefix(ua, "StreamPulse/") {
		t.Errorf("User-Agent = %q, want the prober to identify itself", ua)
	}
	if cc := got.Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want none by default -- viewers get the cached edge too", cc)
	}

	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: srv.URL + "/media.m3u8", NoCache: true,
		Headers: map[string]string{"X-Probe-Token": "abc"},
	})
	if cc := got.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if v := got.Get("X-Probe-Token"); v != "abc" {
		t.Errorf("configured header not sent, got %q", v)
	}
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// Liveness is exported because a cache-age alert cannot be written without
// it: a VOD manifest is *supposed* to sit in a CDN for hours, and a rule that
// cannot tell it from a live one pages about healthy streams.
func TestStreamLivenessIsExported(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"hls live", "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n", "1"},
		{"hls complete", "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n", "0"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(c.body))
		}))
		reg := metrics.New()
		pr := New(reg, &capture{})
		pr.ProbeTarget(context.Background(), config.Target{Name: "t", URL: srv.URL + "/media.m3u8"})
		srv.Close()

		want := `streampulse_stream_live{target="t",variant="direct"} ` + c.want
		if body := scrape(t, reg); !strings.Contains(body, want) {
			t.Errorf("%s: metrics missing %q", c.name, want)
		}
	}
}

// pdt_stale asks the same question a freeze does -- packager, or a stale copy
// of its manifest -- so it gets the same answer. This came from a real
// incident: a four-minute-behind live edge during a network blip, with no way
// to tell whether the packager had fallen behind or the edge had served an old
// manifest, when the Age header had already been read and thrown away.
func TestPDTStaleNamesTheLayer(t *testing.T) {
	// PDT four minutes behind the clock, on a 4s-target playlist.
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pl := livePlaylist(100, 4, 2, 4.0, &anchor)
	now := anchor.Add(8*time.Second + 260*time.Second)

	// A 300s-old manifest against a 260s lag: the cache covers all of it.
	stale := edgeStalenessCheck(now, target, "v", pl,
		cacheInfo{Age: 300 * time.Second, HasAge: true, Hit: "HIT"})
	if len(stale) != 1 {
		t.Fatalf("expected pdt_stale, got %v", checks(stale))
	}
	if !strings.Contains(stale[0].Message, "accounts for all of it") {
		t.Errorf("a 300s-old manifest covers a 260s lag, got %q", stale[0].Message)
	}

	// The case a threshold got wrong: nearly all cache, a little packager.
	mostly := edgeStalenessCheck(now, target, "v", pl,
		cacheInfo{Age: 254 * time.Second, HasAge: true, Hit: "HIT"})
	if !strings.Contains(mostly[0].Message, "the packager was 6.0s behind") {
		t.Errorf("the lag should be split, not thresholded, got %q", mostly[0].Message)
	}

	fresh := edgeStalenessCheck(now, target, "v", pl,
		cacheInfo{Age: 0, HasAge: true, Hit: "MISS"})
	if !strings.Contains(fresh[0].Message, "all of it is the packager") {
		t.Errorf("a fresh manifest points at the origin, got %q", fresh[0].Message)
	}

	// No cache headers, no claim.
	quiet := edgeStalenessCheck(now, target, "v", pl, cacheInfo{})
	if strings.Contains(quiet[0].Message, "cache") {
		t.Errorf("nothing said should mean nothing claimed, got %q", quiet[0].Message)
	}
}

// A segment that 404s because we are acting on a minutes-old cached manifest
// -- naming segments that have since aged out of the window -- is a different
// fault from one the origin never produced.
func TestSegmentAvailabilityCarriesTheManifestAge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Age", "300")
			w.Header().Set("X-Cache", "Hit from cloudfront")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\ngone.ts\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cap := &capture{}
	pr := New(metrics.New(), cap)
	pr.ProbeTarget(context.Background(), config.Target{
		Name: "t", URL: srv.URL + "/media.m3u8", SegmentSample: 1,
	})

	var msg string
	for _, f := range cap.findings {
		if f.Check == "segment_availability" {
			msg = f.Message
		}
	}
	if msg == "" {
		t.Fatalf("expected segment_availability, got %+v", cap.findings)
	}
	if !strings.Contains(msg, "Age 300.0s") || !strings.Contains(msg, "HIT") {
		t.Errorf("finding should carry the manifest's cache state, got %q", msg)
	}
}
