package inspect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubFFprobe writes an executable that answers like ffprobe, so the plumbing
// -- argument construction, JSON parsing, error reporting, timeouts -- is
// tested on every machine rather than only where ffmpeg happens to be
// installed.
func stubFFprobe(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is not portable to windows")
	}
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodOutput = `cat <<'JSON'
{"streams":[
 {"index":0,"codec_name":"h264","codec_type":"video","profile":"High","width":1280,"height":720,"sample_aspect_ratio":"1:1"},
 {"index":1,"codec_name":"aac","codec_type":"audio","channels":2,"sample_rate":"48000"}
]}
JSON`

func TestProbeParsesFFprobeOutput(t *testing.T) {
	i, err := New(stubFFprobe(t, goodOutput))
	if err != nil {
		t.Fatal(err)
	}
	if !i.Available() {
		t.Fatal("inspector should be available")
	}
	m, err := i.Probe(context.Background(), "/tmp/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 {
		t.Fatalf("got %d streams, want 2", len(m.Streams))
	}
	v := m.Video()
	if v == nil || v.CodecName != "h264" || v.Width != 1280 {
		t.Errorf("video stream = %+v", v)
	}
	if a := m.Audio(); a == nil || a.CodecName != "aac" || a.Channels != 2 {
		t.Errorf("audio stream = %+v", a)
	}
	if got := m.Kinds(); len(got) != 2 {
		t.Errorf("Kinds() = %v", got)
	}
}

// ffprobe reports failure on stderr and a non-zero exit. The message has to
// reach the finding, or "could not read the media" tells an operator nothing.
func TestProbeSurfacesFFprobeError(t *testing.T) {
	i, _ := New(stubFFprobe(t, `echo "moov atom not found" >&2; exit 1`))
	_, err := i.Probe(context.Background(), "/tmp/broken.mp4")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "moov atom not found") {
		t.Errorf("error should carry ffprobe's message, got %q", err)
	}
}

func TestProbeRejectsUnparsableOutput(t *testing.T) {
	i, _ := New(stubFFprobe(t, `echo "not json"`))
	if _, err := i.Probe(context.Background(), "x"); err == nil {
		t.Error("expected an error on unreadable output")
	}
}

// A hung ffprobe must not hold a probe cycle open.
func TestProbeRespectsTheContext(t *testing.T) {
	i, _ := New(stubFFprobe(t, "sleep 10"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := i.Probe(ctx, "x"); err == nil {
		t.Error("expected the timeout to surface as an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("probe took %v; the context was not honoured", elapsed)
	}
}

// The dependency is optional, and that has to be true of the zero value too:
// a prober built without an inspector must not panic on the nil path.
func TestDisabledInspectorIsInertNotFatal(t *testing.T) {
	for name, i := range map[string]*Inspector{
		"zero value": {},
		"empty path": mustNew(t, ""),
	} {
		if i.Available() {
			t.Errorf("%s: should not be available", name)
		}
		if _, err := i.Probe(context.Background(), "x"); err == nil {
			t.Errorf("%s: probing a disabled inspector should error, not run", name)
		}
	}
}

func TestAutoReportsWhyItIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on it
	i, err := New("auto")
	if err == nil {
		t.Fatal("expected an explanation when ffprobe is not on $PATH")
	}
	if i.Available() {
		t.Error("inspector should be disabled when the binary is missing")
	}
	if !strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("the reason should name the binary, got %q", err)
	}
}

func TestAutoFindsABinaryOnPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte("#!/bin/sh\n"+goodOutput+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	i, err := New("auto")
	if err != nil {
		t.Fatal(err)
	}
	if !i.Available() {
		t.Fatal("auto should have found the binary")
	}
}

func TestExplicitMissingPathIsReported(t *testing.T) {
	i, err := New("/nonexistent/ffprobe")
	if err == nil {
		t.Error("expected an error for a path that does not exist")
	}
	if i.Available() {
		t.Error("inspector should be disabled")
	}
}

func mustNew(t *testing.T, path string) *Inspector {
	t.Helper()
	i, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
