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
	// BlackSeconds and FreezeSeconds are the longest run of each in the
	// segment this frame came from.
	BlackSeconds  float64
	FreezeSeconds float64
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
	cmd.WaitDelay = waitDelay
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
	meanRe   = regexp.MustCompile(`mean_volume:\s*(-?[0-9.]+) dB`)
	peakRe   = regexp.MustCompile(`max_volume:\s*(-?[0-9.]+) dB`)
	blackRe  = regexp.MustCompile(`black_duration:\s*([0-9.]+)`)
	freezeRe = regexp.MustCompile(`freeze_duration:\s*([0-9.]+)`)
	durRe    = regexp.MustCompile(`Duration:\s*(\d+):(\d+):([0-9.]+)`)
)

// Content is what a whole segment turned out to look and sound like, as
// opposed to the single frame a thumbnail shows.
type Content struct {
	MeanDBFS float64
	PeakDBFS float64
	HasAudio bool
	// BlackSeconds and FreezeSeconds are the longest run of each found in the
	// segment, zero when there was none.
	BlackSeconds  float64
	FreezeSeconds float64
	HasVideo      bool
	// Seconds is the segment's own length, which the thresholds are relative
	// to. An absolute one cannot work: a stream with 1.9s segments could never
	// report two seconds of anything, however dead it was.
	Seconds float64
}

// freezeWindow is the filter's minimum run length, subtracted when working out
// what fraction of a segment was affected. freezedetect cannot report a run
// shorter than this, so a completely frozen segment reports its length minus
// this much -- 1.4s out of 1.92s on a real stream -- and measuring against the
// raw length would make "entirely frozen" look like 73%.
const freezeWindow = 0.5

// Fraction is how much of the measurable part of the segment a run covered,
// 0 to 1.
func (c *Content) Fraction(runSeconds float64) float64 {
	usable := c.Seconds - freezeWindow
	if usable <= 0 || runSeconds <= 0 {
		return 0
	}
	if f := runSeconds / usable; f < 1 {
		return f
	}
	return 1
}

// Analyse measures the whole segment in one decode: audio level, black
// video and frozen video.
//
// One pass rather than three. Each of these is a filter over the same decoded
// frames, and decoding a segment three times to ask three questions about it
// would triple the cost of the most expensive check in the tool.
//
// The thresholds passed to the filters are minimum durations, not the
// reporting thresholds: ffmpeg is asked to notice short runs so the caller can
// see how long they were and decide, rather than having the decision made
// inside the filter.
func (i *Inspector) Analyse(ctx context.Context, path string, hasVideo, hasAudio bool) (*Content, error) {
	if !i.CanCapture() {
		return nil, fmt.Errorf("frame capture is not enabled")
	}
	args := []string{"-v", "info", "-i", path}
	if hasVideo {
		// pix_th is how dark a pixel must be to count as black. The default
		// 0.10 is near-black, which keeps a dim scene from reading as a fault.
		args = append(args, "-vf", "blackdetect=d=0.1:pix_th=0.10,freezedetect=n=-60dB:d=0.5")
	} else {
		args = append(args, "-vn")
	}
	if hasAudio {
		args = append(args, "-af", "volumedetect")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-sn", "-dn", "-f", "null", "-")

	cmd := exec.CommandContext(ctx, i.ffmpeg, args...)
	cmd.WaitDelay = waitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()

	text := stderr.String()
	c := &Content{HasVideo: hasVideo}
	if hasAudio {
		m, p := meanRe.FindStringSubmatch(text), peakRe.FindStringSubmatch(text)
		if m != nil && p != nil {
			mean, err1 := strconv.ParseFloat(m[1], 64)
			peak, err2 := strconv.ParseFloat(p[1], 64)
			if err1 == nil && err2 == nil {
				c.MeanDBFS, c.PeakDBFS, c.HasAudio = mean, peak, true
			}
		}
	}
	if hasVideo {
		c.BlackSeconds = longest(blackRe, text)
		c.FreezeSeconds = longest(freezeRe, text)
	}
	if m := durRe.FindStringSubmatch(text); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mi, _ := strconv.ParseFloat(m[2], 64)
		s, _ := strconv.ParseFloat(m[3], 64)
		c.Seconds = h*3600 + mi*60 + s
	}
	if !c.HasAudio && !hasVideo {
		return nil, fmt.Errorf("nothing measurable in the segment")
	}
	return c, nil
}

// longest returns the longest run the filter reported. A segment can contain
// several, and what matters is the worst one.
func longest(re *regexp.Regexp, text string) float64 {
	var max float64
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > max {
			max = v
		}
	}
	return max
}

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
	cmd.WaitDelay = waitDelay
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
