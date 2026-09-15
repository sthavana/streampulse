package mosaic

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// Server exposes the multiviewer over HTTP. Register its routes on the same
// http.ServeMux that already serves /metrics — no extra listener, no new deps.
type Server struct {
	Store  *Store
	Prefix string // mount point, default "/mosaic"
}

func (s *Server) prefix() string {
	p := strings.TrimRight(s.Prefix, "/")
	if p == "" {
		p = "/mosaic"
	}
	return p
}

// Register mounts the wall, the per-tile MJPEG streams, the SSE status feed and
// the target list. Requires Go 1.22+ for method+wildcard patterns.
func (s *Server) Register(mux *http.ServeMux) {
	p := s.prefix()
	mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET "+p+"/", s.handleIndex)
	mux.HandleFunc("GET "+p+"/api/targets", s.handleTargets)
	mux.HandleFunc("GET "+p+"/events", s.handleEvents)
	mux.HandleFunc("GET "+p+"/tile/{id}", s.handleTile)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Store.Targets())
}

// handleTile streams one target's thumbnails as motion-JPEG. A browser renders
// this in a plain <img> with zero client-side JS — flat cost regardless of how
// many tiles are on the wall.
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, cancel := s.Store.SubscribeFrames(id)
	if ch == nil {
		http.NotFound(w, r)
		return
	}
	defer cancel()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	// Sent before the first frame, deliberately. A grabber that has not
	// produced anything yet -- ffmpeg is still opening the stream, or the
	// stream is down -- would otherwise leave the request with no response at
	// all, holding one of the browser's few connections to this origin open
	// on a promise. Headers now means the tile is a valid, empty stream that
	// starts showing pictures whenever they begin.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(b)); err != nil {
				return
			}
			if _, err := w.Write(b); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleEvents pushes a full status snapshot once per second over SSE. Tiny
// payload for tens of targets, and it naturally surfaces a dead grabber as a
// growing frame_age the client can grey out.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		b, err := json.Marshal(s.Store.Snapshot())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}
