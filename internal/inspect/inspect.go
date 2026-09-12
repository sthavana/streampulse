// Package inspect looks inside the media, by shelling out to ffprobe.
//
// Everything else StreamPulse does validates the plumbing: manifests parse,
// segments fetch, edges advance, keys exist. None of it opens a segment. A
// stream can pass every one of those checks while shipping the wrong codec,
// the wrong resolution, or no audio at all -- faults that break playback or
// waste bandwidth and are invisible from the manifest.
//
// This is the one place the project depends on something outside the standard
// library, and it is deliberately optional: with no ffprobe on the host the
// inspector reports itself unavailable and every other check runs unchanged.
// The default image ships without it.
package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Inspector runs ffprobe. The zero value is a disabled inspector, which is
// what makes the dependency optional: callers ask Available and skip.
type Inspector struct {
	ffprobe string
	ffmpeg  string
}

// New resolves the ffprobe binary. path may be:
//
//	""      disabled
//	"auto"  look on $PATH
//	other   an explicit path
//
// The error explains why inspection is off, for a startup log line. It is not
// fatal: a missing binary disables these checks and nothing else.
func New(path string) (*Inspector, error) {
	switch path {
	case "":
		return &Inspector{}, nil
	case "auto":
		found, err := exec.LookPath("ffprobe")
		if err != nil {
			return &Inspector{}, fmt.Errorf("ffprobe not found on $PATH")
		}
		return &Inspector{ffprobe: found}, nil
	default:
		if _, err := exec.LookPath(path); err != nil {
			return &Inspector{}, fmt.Errorf("ffprobe %q: %w", path, err)
		}
		return &Inspector{ffprobe: path}, nil
	}
}

// Available reports whether inspection can run at all.
func (i *Inspector) Available() bool { return i != nil && i.ffprobe != "" }

// Path is the resolved binary, for a startup log line.
func (i *Inspector) Path() string { return i.ffprobe }

// Media is what ffprobe found in a resource.
type Media struct {
	Streams []Stream `json:"streams"`
}

// Stream is one elementary stream.
type Stream struct {
	Index      int    `json:"index"`
	CodecName  string `json:"codec_name"`
	CodecType  string `json:"codec_type"` // video, audio, subtitle, data
	Profile    string `json:"profile"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SampleAR   string `json:"sample_aspect_ratio"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
}

// Probe reads the stream layout of a local file.
//
// It takes a file rather than a URL deliberately. Handing ffprobe a URL makes
// it fetch the resource itself -- with its own HTTP stack, outside the
// prober's client, headers and timeouts -- and it reads as much as it likes:
// pointed at Apple's fMP4 example, whose EXT-X-MAP slices a few kilobytes out
// of a 150MB file, it downloaded the whole thing and took 24 seconds. The
// caller fetches exactly the bytes a player would and passes the path.
//
// For fMP4 that is the initialisation segment, which carries the whole track
// description in its moov box. A media segment on its own describes nothing,
// so probing one would report every healthy fMP4 stream as broken.
func (i *Inspector) Probe(ctx context.Context, path string) (*Media, error) {
	if !i.Available() {
		return nil, fmt.Errorf("inspection is not enabled")
	}
	// Bound how far ffprobe reads before it decides what the streams are.
	// The track description is at the front of anything we hand it, and the
	// default limits let it chew through megabytes of a transport stream
	// looking for something it has already found.
	args := []string{
		"-v", "error",
		"-analyzeduration", "2000000", "-probesize", "2000000",
		"-print_format", "json", "-show_streams",
		"-i", path,
	}

	cmd := exec.CommandContext(ctx, i.ffprobe, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s", firstLine(msg))
		}
		return nil, err
	}
	var m Media
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	return &m, nil
}

// Video returns the first video stream, or nil.
func (m *Media) Video() *Stream { return m.first("video") }

// Audio returns the first audio stream, or nil.
func (m *Media) Audio() *Stream { return m.first("audio") }

func (m *Media) first(kind string) *Stream {
	for idx := range m.Streams {
		if m.Streams[idx].CodecType == kind {
			return &m.Streams[idx]
		}
	}
	return nil
}

// Kinds lists the stream types present, for a finding message.
func (m *Media) Kinds() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.Streams {
		if !seen[s.CodecType] {
			seen[s.CodecType] = true
			out = append(out, s.CodecType)
		}
	}
	return out
}

// DisplaySize is the resolution as a viewer sees it, which is the coded size
// scaled by the pixel aspect ratio. Anamorphic content codes 1920x1080 as
// 1440x1080 with a 4:3 pixel; comparing the coded size against a manifest that
// correctly says 1920x1080 would report every such stream as wrong.
func (s *Stream) DisplaySize() (int, int) {
	num, den, ok := parseRatio(s.SampleAR)
	if !ok || num == den || num <= 0 || den <= 0 {
		return s.Width, s.Height
	}
	return (s.Width*num + den/2) / den, s.Height
}

func parseRatio(v string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(v), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	num, err1 := strconv.Atoi(parts[0])
	den, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || den == 0 {
		return 0, 0, false
	}
	return num, den, true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
