package probe

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
	"streampulse/internal/metrics"
)

func encPlaylist(keyURI string, segs int) *hls.MediaPlaylist {
	k := hls.Key{Method: "AES-128", URI: keyURI}
	pl := &hls.MediaPlaylist{TargetDuration: 4, Keys: []hls.Key{k}}
	for i := 0; i < segs; i++ {
		kc := k
		pl.Segments = append(pl.Segments, hls.Segment{URI: "s.ts", Duration: 4, Key: &kc})
	}
	return pl
}

func runKeyChecks(t *testing.T, target config.Target, pl *hls.MediaPlaylist) []alert.Finding {
	t.Helper()
	pr := New(metrics.New(), &capture{})
	pr.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return pr.keyChecks(context.Background(), target, "http://o/media.m3u8", "v", pl)
}

// --- clear-lead detection ---

// The commercially serious one: the stream plays, nothing 404s, and the
// content is going out unprotected.
func TestUnexpectedClearSegmentsOnEncryptedStream(t *testing.T) {
	pl := encPlaylist("https://k/1", 3)
	pl.Segments = append(pl.Segments, hls.Segment{URI: "clear.ts", Duration: 4}) // no key

	fs := runKeyChecks(t, config.Target{Name: "t", ExpectEncrypted: true}, pl)
	if !hasCheck(fs, "unexpected_clear_segments") {
		t.Fatalf("clear segment on an encrypted stream went undetected: %v", checks(fs))
	}
	for _, f := range fs {
		if f.Check == "unexpected_clear_segments" {
			if f.Severity != alert.Critical {
				t.Errorf("severity = %v, want critical", f.Severity)
			}
			if !strings.Contains(f.Message, "1 of 4") {
				t.Errorf("message should count the clear segments, got %q", f.Message)
			}
		}
	}
}

func TestFullyEncryptedStreamIsSilent(t *testing.T) {
	fs := runKeyChecks(t, config.Target{Name: "t", ExpectEncrypted: true}, encPlaylist("https://k/1", 4))
	if len(fs) != 0 {
		t.Errorf("a correctly encrypted stream produced %v", checks(fs))
	}
}

// Without expect_encrypted, a clear stream is simply a clear stream.
func TestClearStreamNotFlaggedUnlessDeclared(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4, Segments: []hls.Segment{{URI: "a.ts", Duration: 4}}}
	if fs := runKeyChecks(t, config.Target{Name: "t"}, pl); len(fs) != 0 {
		t.Errorf("unencrypted stream flagged without expect_encrypted: %v", checks(fs))
	}
}

func TestEmptyPlaylistDoesNotTriggerClearCheck(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4}
	if fs := runKeyChecks(t, config.Target{Name: "t", ExpectEncrypted: true}, pl); len(fs) != 0 {
		t.Errorf("empty playlist produced %v", checks(fs))
	}
}

// --- structural ---

func TestKeyMissingURI(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4, Keys: []hls.Key{{Method: "AES-128"}}}
	fs := runKeyChecks(t, config.Target{Name: "t"}, pl)
	if !hasCheck(fs, "key_missing_uri") {
		t.Fatalf("expected key_missing_uri, got %v", checks(fs))
	}
}

func TestMethodNoneWithoutURIIsFine(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4, Keys: []hls.Key{{Method: "NONE"}}}
	if fs := runKeyChecks(t, config.Target{Name: "t"}, pl); len(fs) != 0 {
		t.Errorf("METHOD=NONE legitimately has no URI, got %v", checks(fs))
	}
}

func TestInvalidIVFlagged(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4,
		Keys: []hls.Key{{Method: "AES-128", URI: "https://k/1", IV: "0xdeadbeef"}}}
	if !hasCheck(runKeyChecks(t, config.Target{Name: "t"}, pl), "key_invalid_iv") {
		t.Error("expected key_invalid_iv for a short IV")
	}
}

func TestAbsentIVIsValid(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4,
		Keys: []hls.Key{{Method: "AES-128", URI: "https://k/1"}}}
	if hasCheck(runKeyChecks(t, config.Target{Name: "t"}, pl), "key_invalid_iv") {
		t.Error("an absent IV is legal for AES-128")
	}
}

// --- PSSH ---

func psshBox(systemID string, data []byte) []byte {
	sid, _ := hex.DecodeString(strings.ReplaceAll(systemID, "-", ""))
	size := 32 + len(data)
	b := []byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
	b = append(b, 'p', 's', 's', 'h', 0, 0, 0, 0)
	b = append(b, sid...)
	b = append(b, byte(len(data)>>24), byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
	return append(b, data...)
}

func dataURIKey(payload []byte) hls.Key {
	return hls.Key{
		Method:    "SAMPLE-AES",
		KeyFormat: "urn:uuid:" + hls.SystemWidevine,
		URI:       "data:text/plain;base64," + base64.StdEncoding.EncodeToString(payload),
	}
}

func TestValidPSSHIsSilent(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4, Keys: []hls.Key{dataURIKey(psshBox(hls.SystemWidevine, []byte("init")))}}
	if fs := runKeyChecks(t, config.Target{Name: "t"}, pl); len(fs) != 0 {
		t.Errorf("a valid PSSH produced %v", checks(fs))
	}
}

func TestMalformedPSSHFlagged(t *testing.T) {
	bad := psshBox(hls.SystemWidevine, []byte("init"))
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 9, 0 // size field now lies
	pl := &hls.MediaPlaylist{TargetDuration: 4, Keys: []hls.Key{dataURIKey(bad)}}
	if !hasCheck(runKeyChecks(t, config.Target{Name: "t"}, pl), "pssh_malformed") {
		t.Error("expected pssh_malformed for a bad size field")
	}
}

// A data: URI that is not a PSSH box must not be reported as a broken one:
// PlayReady commonly ships a bare PlayReady Object.
func TestNonPSSHDataURIIsNotFlagged(t *testing.T) {
	pl := &hls.MediaPlaylist{TargetDuration: 4,
		Keys: []hls.Key{dataURIKey([]byte("<WRMHEADER><DATA/></WRMHEADER>"))}}
	if fs := runKeyChecks(t, config.Target{Name: "t"}, pl); len(fs) != 0 {
		t.Errorf("a PlayReady Object was misreported: %v", checks(fs))
	}
}

// --- key fetching ---

func keyServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestKeyFetchSuccess(t *testing.T) {
	s := keyServer(t, make([]byte, 16), http.StatusOK)
	fs := runKeyChecks(t, config.Target{Name: "t", FetchKeys: true}, encPlaylist(s.URL+"/key", 2))
	if len(fs) != 0 {
		t.Errorf("a healthy 16-byte key produced %v", checks(fs))
	}
}

func TestKeyFetchFailureIsCritical(t *testing.T) {
	s := keyServer(t, nil, http.StatusForbidden)
	fs := runKeyChecks(t, config.Target{Name: "t", FetchKeys: true}, encPlaylist(s.URL+"/key", 2))
	if !hasCheck(fs, "key_fetch") {
		t.Fatalf("expected key_fetch, got %v", checks(fs))
	}
	for _, f := range fs {
		if f.Check == "key_fetch" && f.Severity != alert.Critical {
			t.Errorf("severity = %v, want critical", f.Severity)
		}
	}
}

// A key URI serving an error page instead of 16 raw bytes is a real and
// easily-missed failure: it returns 200.
func TestWrongSizedKeyFlagged(t *testing.T) {
	s := keyServer(t, []byte("<html>404 not found</html>"), http.StatusOK)
	if !hasCheck(runKeyChecks(t, config.Target{Name: "t", FetchKeys: true}, encPlaylist(s.URL+"/key", 2)), "key_size_invalid") {
		t.Error("expected key_size_invalid when the URI serves the wrong resource")
	}
}

// Key material must never reach a finding: findings go to logs and Slack.
func TestFindingsNeverCarryKeyMaterial(t *testing.T) {
	secret := []byte("SUPERSECRETKEY16")
	s := keyServer(t, secret, http.StatusOK)

	pr := New(metrics.New(), &capture{})
	pr.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	// Force a finding by declaring the stream encrypted and giving it a clear segment.
	pl := encPlaylist(s.URL+"/key", 1)
	pl.Segments = append(pl.Segments, hls.Segment{URI: "clear.ts", Duration: 4})
	fs := pr.keyChecks(context.Background(), config.Target{Name: "t", FetchKeys: true, ExpectEncrypted: true},
		"http://o/media.m3u8", "v", pl)

	if len(fs) == 0 {
		t.Fatal("expected at least one finding to inspect")
	}
	for _, f := range fs {
		if strings.Contains(f.Message, string(secret)) {
			t.Fatalf("key material leaked into a finding: %q", f.Message)
		}
	}
}

func TestNonFetchableKeysAreSkipped(t *testing.T) {
	// skd:// (FairPlay) and data: URIs are not HTTP resources. Attempting to
	// fetch them would report a failure on a perfectly healthy stream.
	for _, uri := range []string{"skd://fps.example/abc", "data:text/plain;base64,AAAA"} {
		pl := encPlaylist(uri, 2)
		pl.Keys[0].KeyFormat = hls.KeyFormatFairPlay
		for i := range pl.Segments {
			pl.Segments[i].Key.KeyFormat = hls.KeyFormatFairPlay
		}
		fs := runKeyChecks(t, config.Target{Name: "t", FetchKeys: true}, pl)
		if hasCheck(fs, "key_fetch") {
			t.Errorf("%s was fetched over HTTP: %v", uri, checks(fs))
		}
	}
}

func TestKeysNotFetchedUnlessEnabled(t *testing.T) {
	s := keyServer(t, nil, http.StatusInternalServerError)
	fs := runKeyChecks(t, config.Target{Name: "t"}, encPlaylist(s.URL+"/key", 2)) // FetchKeys false
	if hasCheck(fs, "key_fetch") {
		t.Error("keys were fetched with fetch_keys disabled")
	}
}

// --- rotation ---

func TestKeyRotationStalls(t *testing.T) {
	pr := New(metrics.New(), &capture{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr.now = func() time.Time { return now }
	target := config.Target{Name: "t", KeyRotationMaxSec: 60}
	pl := encPlaylist("https://k/1", 2)

	pr.keyChecks(context.Background(), target, "u", "v", pl) // baseline

	now = now.Add(90 * time.Second)
	fs := pr.keyChecks(context.Background(), target, "u", "v", pl)
	if !hasCheck(fs, "key_rotation_stalled") {
		t.Fatalf("expected key_rotation_stalled, got %v", checks(fs))
	}
}

func TestKeyRotationWithinIntervalIsSilent(t *testing.T) {
	pr := New(metrics.New(), &capture{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr.now = func() time.Time { return now }
	target := config.Target{Name: "t", KeyRotationMaxSec: 60}

	pr.keyChecks(context.Background(), target, "u", "v", encPlaylist("https://k/1", 2))
	now = now.Add(90 * time.Second)
	// The key rotated, so the clock restarts.
	fs := pr.keyChecks(context.Background(), target, "u", "v", encPlaylist("https://k/2", 2))
	if hasCheck(fs, "key_rotation_stalled") {
		t.Errorf("a rotated key must not be reported as stalled: %v", checks(fs))
	}
}

func TestRotationNotCheckedUnlessConfigured(t *testing.T) {
	pr := New(metrics.New(), &capture{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr.now = func() time.Time { return now }
	target := config.Target{Name: "t"} // no KeyRotationMaxSec
	pl := encPlaylist("https://k/1", 2)

	pr.keyChecks(context.Background(), target, "u", "v", pl)
	now = now.Add(24 * time.Hour)
	if hasCheck(pr.keyChecks(context.Background(), target, "u", "v", pl), "key_rotation_stalled") {
		t.Error("rotation was checked without an expected interval")
	}
}

// Key URIs in real playlists are usually relative to the media playlist, not
// absolute. This is exercised end to end because the resolution happens in the
// prober, not the parser.
func TestRelativeKeyURIIsFetchedAndResolved(t *testing.T) {
	var keyHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/live/key.bin", func(w http.ResponseWriter, _ *http.Request) {
		keyHits++
		_, _ = w.Write(make([]byte, 16))
	})
	s := httptest.NewServer(mux)
	defer s.Close()

	pr := New(metrics.New(), &capture{})
	pr.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	// URI="key.bin" relative to /live/media.m3u8 must resolve to /live/key.bin.
	pl := encPlaylist("key.bin", 2)
	fs := pr.keyChecks(context.Background(), config.Target{Name: "t", FetchKeys: true},
		s.URL+"/live/media.m3u8", "v", pl)

	if keyHits != 1 {
		t.Fatalf("relative key URI fetched %d times, want 1", keyHits)
	}
	if len(fs) != 0 {
		t.Errorf("a healthy relative key produced %v", checks(fs))
	}
}

func TestRelativeKeyURIFailureIsReported(t *testing.T) {
	s := httptest.NewServer(http.NewServeMux()) // nothing registered: 404
	defer s.Close()

	pr := New(metrics.New(), &capture{})
	pr.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	fs := pr.keyChecks(context.Background(), config.Target{Name: "t", FetchKeys: true},
		s.URL+"/live/media.m3u8", "v", encPlaylist("key.bin", 2))

	if !hasCheck(fs, "key_fetch") {
		t.Fatalf("a missing relative key went undetected: %v", checks(fs))
	}
}
