package inspect

import (
	"testing"
	"time"
)

func TestSilenceThreshold(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want bool
	}{
		{"digital silence", Frame{HasAudio: true, PeakDBFS: -91}, true},
		{"at the floor", Frame{HasAudio: true, PeakDBFS: -60}, true},
		{"quiet programme", Frame{HasAudio: true, PeakDBFS: -42}, false},
		{"normal programme", Frame{HasAudio: true, PeakDBFS: -6}, false},
		// No audio track is not silence: there is nothing to be silent.
		{"no audio track", Frame{HasAudio: false, PeakDBFS: 0}, false},
	}
	for _, c := range cases {
		if got := c.f.Silent(); got != c.want {
			t.Errorf("%s: Silent() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFramesStoreKeepsOnlyTheLatest(t *testing.T) {
	f := NewFrames(10)
	key := FrameKey("chan", "video/v0")

	if _, ok := f.Get(key); ok {
		t.Error("an empty store should report nothing")
	}
	f.Put(key, Frame{JPEG: []byte("first"), At: time.Unix(1, 0)})
	f.Put(key, Frame{JPEG: []byte("second"), At: time.Unix(2, 0)})

	got, ok := f.Get(key)
	if !ok || string(got.JPEG) != "second" {
		t.Errorf("got %q, want the latest frame", got.JPEG)
	}
}

// Streams are keyed separately, or every picture would be the last one
// captured.
func TestFramesAreKeyedPerStream(t *testing.T) {
	f := NewFrames(10)
	f.Put(FrameKey("a", "v"), Frame{JPEG: []byte("A")})
	f.Put(FrameKey("b", "v"), Frame{JPEG: []byte("B")})
	f.Put(FrameKey("a", "audio"), Frame{JPEG: []byte("C")})

	for key, want := range map[string]string{
		FrameKey("a", "v"): "A", FrameKey("b", "v"): "B", FrameKey("a", "audio"): "C",
	} {
		got, _ := f.Get(key)
		if string(got.JPEG) != want {
			t.Errorf("%q = %q, want %q", key, got.JPEG, want)
		}
	}
}

// Image data is held in memory, so the store refuses to grow past its cap
// rather than following a misconfiguration into swap.
func TestFramesStoreIsBounded(t *testing.T) {
	f := NewFrames(2)
	f.Put(FrameKey("a", "v"), Frame{JPEG: []byte("A")})
	f.Put(FrameKey("b", "v"), Frame{JPEG: []byte("B")})
	f.Put(FrameKey("c", "v"), Frame{JPEG: []byte("C")})

	if _, ok := f.Get(FrameKey("c", "v")); ok {
		t.Error("the store grew past its cap")
	}
	// Existing entries still refresh: the cap bounds streams, not updates.
	f.Put(FrameKey("a", "v"), Frame{JPEG: []byte("A2")})
	if got, _ := f.Get(FrameKey("a", "v")); string(got.JPEG) != "A2" {
		t.Errorf("a full store stopped refreshing existing frames: %q", got.JPEG)
	}
}

// The zero value and a nil store are inert, so a prober with capture disabled
// needs no special casing.
func TestNilFramesIsSafe(t *testing.T) {
	var f *Frames
	f.Put("k", Frame{JPEG: []byte("x")})
	if _, ok := f.Get("k"); ok {
		t.Error("a nil store should hold nothing")
	}
}

func TestCaptureDisabledWithoutFFmpeg(t *testing.T) {
	i := &Inspector{}
	if i.CanCapture() {
		t.Error("capture should be off without ffmpeg")
	}
	if _, err := i.Thumbnail(nil, "x", 320); err == nil {
		t.Error("thumbnailing without ffmpeg should error, not run")
	}
	if _, _, err := i.AudioLevel(nil, "x"); err == nil {
		t.Error("measuring without ffmpeg should error, not run")
	}
}
