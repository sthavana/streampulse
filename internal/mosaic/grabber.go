package mosaic

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Grabber pulls one target with a single long-running ffmpeg process, decimated
// to a low frame rate, and pushes each JPEG thumbnail into the Store. One
// Grabber per target, mirroring the prober's per-target-goroutine model.
//
// ffmpeg is exec'd, not linked — the same shell-out stance as ffprobe, so no
// FFmpeg license reaches the Go code. A dead stream just makes ffmpeg exit;
// Run respawns it with capped backoff, which is itself the "no signal" signal.
type Grabber struct {
	FFmpegPath string // "ffmpeg" or an absolute path
	TargetID   string
	URL        string // media playlist / MPD / TS URL, from the prober
	// Live changes how the input is read. See args.
	Live    bool
	FPS     string // frames per second as an ffmpeg expr: "1", "1/2", "2"
	Width   int    // thumbnail width; height keeps aspect (default 320)
	Quality int    // ffmpeg -q:v, 2 (best) .. 31 (worst); default 7
	Store   *Store
	Logf    func(format string, args ...any) // optional
}

func (g *Grabber) log(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

// Run blocks until ctx is cancelled, keeping a grabber alive across stream
// failures with exponential backoff (1s..15s), reset after any successful frame.
func (g *Grabber) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		frames, err := g.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if frames > 0 {
			backoff = time.Second
		}
		if err == nil {
			g.log("mosaic: grabber %s reached the end of its input after %d frames; restarting in %s",
				g.TargetID, frames, backoff)
		} else {
			g.log("mosaic: grabber %s failed after %d frames: %v; retry in %s",
				g.TargetID, frames, err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (g *Grabber) args() []string {
	fps := g.FPS
	if fps == "" {
		fps = "1"
	}
	w := g.Width
	if w == 0 {
		w = 320
	}
	q := g.Quality
	if q == 0 {
		q = 7
	}
	args := []string{"-loglevel", "error", "-nostdin", "-fflags", "nobuffer"}
	if !g.Live {
		// A finite input -- a VOD playlist, or a file -- is otherwise read as
		// fast as it can be downloaded. Apple's ten-minute sample raced by in
		// seventy seconds at eight and a half times speed, hit the end, exited,
		// and was restarted from the beginning by the backoff path: a tile that
		// fast-forwarded and then jumped back every minute, re-downloading the
		// whole clip each time. -re paces it at its own frame rate and
		// -stream_loop keeps it going, so a VOD tile shows the clip at real
		// speed, forever.
		//
		// Not applied to live input, where it is at best redundant -- the
		// stream is already paced by segment availability -- and at worst a
		// slow drift behind the live edge.
		args = append(args, "-re", "-stream_loop", "-1")
	}
	return append(args,
		"-i", g.URL,
		"-an",
		"-vf", fmt.Sprintf("fps=%s,scale=%d:-2:flags=fast_bilinear", fps, w),
		"-f", "mjpeg", "-q:v", fmt.Sprintf("%d", q),
		"pipe:1",
	)
}

func (g *Grabber) runOnce(ctx context.Context) (int, error) {
	bin := g.FFmpegPath
	if bin == "" {
		bin = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, bin, g.args()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	frames := 0
	scanErr := scanMJPEG(stdout, func(jpeg []byte) {
		g.Store.PushFrame(g.TargetID, jpeg)
		frames++
	})
	waitErr := cmd.Wait()
	if scanErr != nil && scanErr != io.EOF {
		return frames, scanErr
	}
	return frames, waitErr
}

// scanMJPEG splits a raw MJPEG byte stream into individual JPEGs on SOI (FFD8)
// and EOI (FFD9) markers. Safe because within JPEG entropy data a real 0xFF is
// byte-stuffed with 0x00, so a bare FFD9 only ever marks end-of-image.
func scanMJPEG(r io.Reader, emit func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	var buf []byte
	inFrame := false
	var prev byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if !inFrame {
			if prev == 0xFF && b == 0xD8 {
				inFrame = true
				buf = append(buf[:0], 0xFF, 0xD8)
			}
			prev = b
			continue
		}
		buf = append(buf, b)
		if prev == 0xFF && b == 0xD9 {
			frame := make([]byte, len(buf))
			copy(frame, buf)
			emit(frame)
			inFrame, prev = false, 0
			continue
		}
		prev = b
	}
}
