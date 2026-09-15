// Command mosaic is the StreamPulse multiviewer: a tile wall showing a picture
// per target beside the health the prober already knows.
//
// It is a client of a running prober, not a second copy of one. Point it at a
// prober's address and it reads /api/state for the target list, their URLs and
// their open incidents, then runs one ffmpeg per target to pull a thumbnail a
// second. Nothing here decides what "critical" means.
//
// Separate from the prober on purpose. A multiviewer decodes every channel
// continuously; a prober samples a segment per poll and must stay light enough
// to be trusted when everything else is on fire. Keeping them apart means an
// ffmpeg storm cannot starve the process that pages you, and the prober's
// image stays a 10MB scratch container with no ffmpeg in it at all.
//
//	mosaic -prober http://localhost:9090 -addr :9091
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"streampulse/internal/mosaic"
)

func main() {
	proberURL := flag.String("prober", "http://localhost:9090", "base URL of the StreamPulse prober to follow")
	addr := flag.String("addr", ":9091", "address to serve the wall on")
	prefix := flag.String("prefix", "/mosaic", "path to mount the wall at")
	poll := flag.Duration("poll", 2*time.Second, "how often to re-read the prober's state")
	fps := flag.String("fps", "1", "thumbnail rate as an ffmpeg expression, e.g. 1 or 1/2")
	width := flag.Int("width", 320, "thumbnail width in pixels; height follows the aspect ratio")
	quality := flag.Int("quality", 7, "JPEG quality, 2 (best) to 31 (worst)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "path to the ffmpeg binary")
	flag.Parse()

	store := mosaic.New()

	// One grabber per target, built here so the package never needs to know
	// what the flags are called.
	pool := mosaic.NewPool(func(id, url string) mosaic.Runner {
		return &mosaic.Grabber{
			FFmpegPath: *ffmpegPath,
			TargetID:   id,
			URL:        url,
			FPS:        *fps,
			Width:      *width,
			Quality:    *quality,
			Store:      store,
			Logf:       log.Printf,
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &mosaic.Source{
		ProberURL: *proberURL,
		Every:     *poll,
		Store:     store,
		Logf:      log.Printf,
		OnTargets: func(urls map[string]string) {
			started, stopped := pool.Sync(ctx, urls)
			if len(started) > 0 {
				log.Printf("mosaic: grabbing %v", started)
			}
			if len(stopped) > 0 {
				log.Printf("mosaic: stopped grabbing %v", stopped)
			}
		},
	}
	go src.Run(ctx)

	mux := http.NewServeMux()
	(&mosaic.Server{Store: store, Prefix: *prefix}).Register(mux)
	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// No write timeout: the tiles are MJPEG responses and the status feed
		// is server-sent events, both of which are meant to stay open. Read
		// timeouts still apply, so a stuck client cannot hold a connection
		// without asking for anything.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("multiviewer on http://localhost%s%s, following %s", *addr, *prefix, *proberURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	cancel()

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = srv.Shutdown(shutdownCtx)
	// After the server, so no handler is still subscribing to frames from a
	// grabber we are about to wait on.
	pool.Stop()
}
