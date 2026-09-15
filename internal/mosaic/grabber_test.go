package mosaic

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// jpegFrame builds a byte sequence shaped like a JPEG: SOI, a payload, EOI.
func jpegFrame(payload ...byte) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, payload...)
	return append(out, 0xFF, 0xD9)
}

func collect(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var got [][]byte
	err := scanMJPEG(bytes.NewReader(stream), func(f []byte) {
		got = append(got, append([]byte(nil), f...))
	})
	if err != io.EOF {
		t.Fatalf("scan ended with %v, want EOF", err)
	}
	return got
}

func TestScanSplitsFramesOnMarkers(t *testing.T) {
	a, b := jpegFrame(1, 2, 3), jpegFrame(9)
	got := collect(t, append(append([]byte{}, a...), b...))

	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Errorf("frames = %v, %v", got[0], got[1])
	}
}

// ffmpeg's first bytes are not always a frame boundary, and a restarted
// grabber can join mid-stream. Leading junk must be discarded, not prepended.
func TestScanSkipsBytesBeforeTheFirstFrame(t *testing.T) {
	frame := jpegFrame(7, 7)
	stream := append([]byte("garbage\x00\x01"), frame...)

	got := collect(t, stream)
	if len(got) != 1 || !bytes.Equal(got[0], frame) {
		t.Fatalf("got %v, want one clean frame", got)
	}
}

// A frame still arriving when the stream ends is not a frame. Emitting it
// would put a half-decoded picture on the wall.
func TestScanDropsATruncatedTrailingFrame(t *testing.T) {
	stream := append(jpegFrame(1), 0xFF, 0xD8, 0x41, 0x42)

	if got := collect(t, stream); len(got) != 1 {
		t.Fatalf("got %d frames, want only the complete one", len(got))
	}
}

// Within JPEG entropy data a real 0xFF is byte-stuffed with 0x00, so FFD9 only
// ever means end-of-image -- but FF00 appears constantly and must not end one.
func TestScanIgnoresStuffedBytesInsideAFrame(t *testing.T) {
	frame := jpegFrame(0xFF, 0x00, 0x42, 0xFF, 0x00)

	got := collect(t, frame)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0], frame) {
		t.Errorf("frame = % x, want the whole thing", got[0])
	}
}

// Two EOI markers in a row, or an SOI immediately after an EOI, must not make
// the scanner lose its place.
func TestScanHandlesFramesBackToBack(t *testing.T) {
	var stream []byte
	for i := 0; i < 5; i++ {
		stream = append(stream, jpegFrame(byte(i))...)
	}
	if got := collect(t, stream); len(got) != 5 {
		t.Fatalf("got %d frames, want 5", len(got))
	}
}

// The grabber's command line is the contract with ffmpeg: a typo in it means
// no wall, and it is the one part with no test coverage from behaviour.
func TestGrabberArgumentsCarryTheDefaults(t *testing.T) {
	g := &Grabber{URL: "https://cdn/live.m3u8"}
	args := strings.Join(g.args(), " ")

	for _, want := range []string{
		"-i https://cdn/live.m3u8", // the stream
		"fps=1",                    // one frame a second by default
		"scale=320:-2",             // default width, aspect preserved
		"-q:v 7",                   // default quality
		"-f mjpeg",                 // the format the scanner expects
		"-an",                      // no audio: nothing here decodes it
		"pipe:1",                   // frames come back on stdout
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
}

func TestGrabberArgumentsHonourOverrides(t *testing.T) {
	g := &Grabber{URL: "u", FPS: "1/2", Width: 640, Quality: 2}
	args := strings.Join(g.args(), " ")

	for _, want := range []string{"fps=1/2", "scale=640:-2", "-q:v 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
}
