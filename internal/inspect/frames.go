package inspect

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame is the most recent visual and audible evidence for one stream.
//
// It exists because there is a question no check answers: not "does the
// manifest describe a stream" or "do the segments decode", but "is there
// actually a picture, and is there any sound". An operator answers that in a
// glance and no assertion substitutes for it.
type Frame struct {
	JPEG []byte
	At   time.Time
	// MeanDBFS and PeakDBFS are the segment's audio levels. Digital silence
	// reads around -91 dBFS and normal programme material around -20.
	MeanDBFS float64
	PeakDBFS float64
	HasAudio bool
}

// Silent reports whether the audio is at or below the floor of audibility.
// Anything quieter than -60 dBFS is not quiet programme material, it is
// nothing.
func (f Frame) Silent() bool { return f.HasAudio && f.PeakDBFS <= -60 }

// Frames holds the latest frame per stream. It is deliberately a fixed-size
// cache of the present rather than a history: this is a live view, and keeping
// old frames would turn a monitoring tool into a video recorder.
type Frames struct {
	mu  sync.RWMutex
	max int
	m   map[string]Frame
}

// FrameKey identifies one stream's frame.
func FrameKey(target, variant string) string { return target + "\x00" + variant }

func NewFrames(max int) *Frames {
	if max <= 0 {
		max = 200
	}
	return &Frames{max: max, m: make(map[string]Frame)}
}

func (f *Frames) Put(key string, fr Frame) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// A stream count beyond the cap means a configuration nobody intended;
	// stop growing rather than hold an unbounded amount of image data.
	if _, replacing := f.m[key]; !replacing && len(f.m) >= f.max {
		return
	}
	f.m[key] = fr
}

func (f *Frames) Get(key string) (Frame, bool) {
	if f == nil {
		return Frame{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	fr, ok := f.m[key]
	return fr, ok
}

// --- ffmpeg ---

// SetFFmpeg enables frame capture. Like ffprobe it is optional: without it the
// inspector still compares codecs and resolutions, and simply produces no
// pictures.
func (i *Inspector) SetFFmpeg(path string) error {
	switch path {
	case "":
		return nil
	case "auto":
		found, err := exec.LookPath("ffmpeg")
		if err != nil {
			return fmt.Errorf("ffmpeg not found on $PATH")
		}
		i.ffmpeg = found
	default:
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("ffmpeg %q: %w", path, err)
		}
		i.ffmpeg = path
	}
	return nil
}

// CanCapture reports whether frame capture is available.
func (i *Inspector) CanCapture() bool { return i != nil && i.ffmpeg != "" }

// FFmpegPath is the resolved binary, for a startup log line.
func (i *Inspector) FFmpegPath() string { return i.ffmpeg }

// Thumbnail decodes one frame and returns it as a small JPEG.
//
// Small on purpose: this is a glanceable indicator sitting in a table row, not
// a viewer. A 320px wide frame is enough to see that the picture is there,
// that it is not black, and roughly what it is, and costs a few kilobytes per
// stream per poll.
func (i *Inspector) Thumbnail(ctx context.Context, path string, width int) ([]byte, error) {
	if !i.CanCapture() {
		return nil, fmt.Errorf("frame capture is not enabled")
	}
	if width <= 0 {
		width = 320
	}
	cmd := exec.CommandContext(ctx, i.ffmpeg,
		"-v", "error",
		"-i", path,
		"-frames:v", "1",
		"-vf", "scale="+strconv.Itoa(width)+":-2",
		"-f", "mjpeg", "-",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s", firstLine(msg))
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no frame")
	}
	return out, nil
}

var (
	meanRe = regexp.MustCompile(`mean_volume:\s*(-?[0-9.]+) dB`)
	peakRe = regexp.MustCompile(`max_volume:\s*(-?[0-9.]+) dB`)
)

// AudioLevel measures the segment's mean and peak level in dBFS.
//
// It is a measurement rather than a verdict. Whether a given level is a fault
// depends on the content -- a drama has quiet passages a news channel does not
// -- so the number is reported and the judgement left to whoever knows the
// programme.
func (i *Inspector) AudioLevel(ctx context.Context, path string) (mean, peak float64, err error) {
	if !i.CanCapture() {
		return 0, 0, fmt.Errorf("frame capture is not enabled")
	}
	cmd := exec.CommandContext(ctx, i.ffmpeg,
		"-v", "info",
		"-i", path,
		"-af", "volumedetect",
		"-vn", "-sn", "-dn",
		"-f", "null", "-",
	)
	// volumedetect reports on stderr, which is also where errors go.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()

	text := stderr.String()
	m := meanRe.FindStringSubmatch(text)
	p := peakRe.FindStringSubmatch(text)
	if m == nil || p == nil {
		return 0, 0, fmt.Errorf("no level reported")
	}
	mean, err = strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, 0, err
	}
	peak, err = strconv.ParseFloat(p[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return mean, peak, nil
}
