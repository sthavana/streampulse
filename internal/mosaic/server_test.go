package mosaic

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func servedStore(ids ...string) (*Store, *httptest.Server) {
	store := New()
	infos := make([]TargetInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, TargetInfo{ID: id, Name: id})
	}
	store.Sync(infos)

	mux := http.NewServeMux()
	(&Server{Store: store, Prefix: "/mosaic"}).Register(mux)
	return store, httptest.NewServer(mux)
}

func TestWallIsServedWithItsRoutes(t *testing.T) {
	_, srv := servedStore("news")
	defer srv.Close()

	for _, path := range []string{"/mosaic/", "/mosaic/api/targets"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d", path, resp.StatusCode)
		}
	}
}

func TestTargetsEndpointListsTheWall(t *testing.T) {
	_, srv := servedStore("news", "sport")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mosaic/api/targets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []TargetInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "news" || got[1].ID != "sport" {
		t.Errorf("targets = %+v", got)
	}
}

// A tile for a target that does not exist must 404 rather than hang a
// connection open forever waiting for frames that will never come.
func TestUnknownTileIsNotFound(t *testing.T) {
	_, srv := servedStore("news")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mosaic/tile/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The tile is motion-JPEG in a plain <img>: multipart, with each part a
// complete JPEG. Getting the boundary or the headers wrong shows a broken
// image icon and nothing else.
func TestTileStreamsMultipartJPEG(t *testing.T) {
	store, srv := servedStore("news")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/mosaic/tile/news", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/x-mixed-replace") {
		t.Fatalf("content type = %q", ct)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		store.PushFrame("news", jpegFrame(1, 2, 3))
	}()

	br := bufio.NewReader(resp.Body)
	var sawBoundary, sawType bool
	for i := 0; i < 6; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading part headers: %v", err)
		}
		switch {
		case strings.HasPrefix(line, "--frame"):
			sawBoundary = true
		case strings.HasPrefix(line, "Content-Type: image/jpeg"):
			sawType = true
		}
		if sawBoundary && sawType {
			return
		}
	}
	t.Errorf("no JPEG part arrived (boundary=%v type=%v)", sawBoundary, sawType)
}

// The wall's status feed is server-sent events, and the first message must
// arrive immediately rather than after the first tick -- otherwise a freshly
// opened wall is blank for a second.
func TestEventsSendASnapshotImmediately(t *testing.T) {
	store, srv := servedStore("news")
	defer srv.Close()
	store.SetHealth(map[string]Health{"news": {Severity: SevCritical, Summary: "edge frozen"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/mosaic/events", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading first event: %v", err)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line = %q, want an SSE data frame", line)
	}
	// Immediately, not on the first tick of the one-second ticker: a freshly
	// opened wall showing nothing for a second looks broken.
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("first event took %v, want it sent before the first tick", waited)
	}
	var rows []Status
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rows); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if len(rows) != 1 || rows[0].SeverityStr != "critical" || rows[0].Summary != "edge frozen" {
		t.Errorf("rows = %+v", rows)
	}
}

// A tile that has never received a frame reports -1 rather than 0, because
// zero seconds ago is what a perfectly healthy tile reports.
func TestFrameAgeDistinguishesNeverFromJustNow(t *testing.T) {
	store := New()
	store.Sync([]TargetInfo{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}})
	store.PushFrame("b", jpegFrame(1))

	byID := map[string]Status{}
	for _, s := range store.Snapshot() {
		byID[s.TargetID] = s
	}
	if byID["a"].FrameAge != -1 {
		t.Errorf("never-seen tile reports frame age %v, want -1", byID["a"].FrameAge)
	}
	if byID["b"].FrameAge < 0 {
		t.Errorf("tile with a frame reports %v", byID["b"].FrameAge)
	}
}
