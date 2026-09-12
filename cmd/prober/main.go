// Command prober is the StreamPulse synthetic HLS health prober.
//
// It reads a JSON config of targets, polls each on its own schedule, runs the
// health checks, streams findings as JSON (and optionally to Slack), and serves
// Prometheus metrics on /metrics.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	// Embed the timezone database so maintenance windows resolve their
	// location on hosts without system tzdata, such as a scratch container.
	_ "time/tzdata"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/inspect"
	"streampulse/internal/metrics"
	"streampulse/internal/probe"
	"streampulse/internal/supervisor"
	"streampulse/internal/web"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	reg := metrics.New()
	if cfg.Vantage != "" {
		reg.SetConstantLabels(map[string]string{"vantage": cfg.Vantage})
		log.Printf("observing as vantage %q", cfg.Vantage)
	}

	// The recorder keeps the last few hundred notifications in memory for the
	// web UI. It sits in the chain rather than replacing anything: stdout stays
	// the durable record.
	recorder := alert.NewRecorder(200)
	var notifier alert.Notifier = alert.Multi(alert.NewJSONNotifier(), recorder)
	if cfg.SlackWebhook != "" {
		notifier = alert.Multi(notifier, alert.NewSlackNotifier(cfg.SlackWebhook))
	}

	// Every finding goes through the tracker, so a fault re-observed on each
	// poll is announced once when it opens and once when it clears.
	tracker := alert.NewTracker(alert.TrackerConfig{
		For:          cfg.Alerting.For(),
		ResolveAfter: cfg.Alerting.ResolveAfter(),
		RepeatEvery:  cfg.Alerting.Repeat(),
	}, notifier, reg)

	if cfg.Alerting.StateFile != "" {
		tracker.SetStateFile(cfg.Alerting.StateFile)
		// A state file that cannot be read is logged and stepped over. Refusing
		// to start because the scratch file is corrupt would turn a cosmetic
		// problem into an outage of the monitoring itself.
		switch n, err := tracker.LoadState(); {
		case err != nil:
			log.Printf("incident state: %v; starting with none", err)
		case n > 0:
			log.Printf("incident state: restored %d open incident(s) from %s", n, cfg.Alerting.StateFile)
		default:
			log.Printf("incident state: %s (none to restore)", cfg.Alerting.StateFile)
		}
	}

	schedule, err := cfg.Schedule()
	if err != nil {
		log.Fatalf("maintenance: %v", err)
	}
	tracker.SetSchedule(schedule)
	for _, w := range schedule {
		log.Printf("maintenance window configured: %q targets=%v checks=%v", w.Name, scopeOrAll(w.Targets), scopeOrAll(w.Checks))
	}

	pr := probe.New(reg, tracker)

	// The one optional external dependency. A missing ffprobe disables the
	// media checks and nothing else, so it is logged rather than fatal.
	inspector, err := inspect.New(cfg.Inspection.FFprobe)
	if err != nil {
		log.Printf("media inspection disabled: %v", err)
	} else if inspector.Available() {
		log.Printf("media inspection using %s", inspector.Path())
	}
	if err := inspector.SetFFmpeg(cfg.Inspection.FFmpeg); err != nil {
		log.Printf("frame capture disabled: %v", err)
	} else if inspector.CanCapture() {
		log.Printf("frame capture using %s", inspector.FFmpegPath())
	}
	pr.SetInspector(inspector, cfg.Inspection.Timeout())
	pr.SetVantage(cfg.Vantage)
	pr.SetContentThresholds(cfg.Inspection.BlackFraction, cfg.Inspection.FreezeFraction)

	// Before the web handler, which reads the live target set from it.
	sup := supervisor.New(pr)

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", web.Handler(web.Sources{
		Registry: reg, Tracker: tracker, Recorder: recorder, Frames: pr.Frames(),
		Vantage: cfg.Vantage,
		Targets: sup.Targets,
		Started: time.Now(),
	}))
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	go func() {
		log.Printf("web UI + metrics on %s (/, /metrics, /healthz)", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server error: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		runSweeper(ctx, tracker, cfg.Alerting.Sweep())
	}()
	sup.Sync(ctx, cfg.Targets)

	// Re-read the config so a stream can be added, removed or retimed without
	// restarting. A restart is not free for a monitoring tool: it drops every
	// in-flight probe and re-derives the cross-poll state that freeze and
	// rollback detection depend on.
	if every := cfg.Reload(); every > 0 {
		log.Printf("watching %s for changes every %s", *cfgPath, every)
		wg.Add(1)
		go func() {
			defer wg.Done()
			config.Watch(ctx, *cfgPath, every, func(next *config.Config, err error) {
				if err != nil {
					// A saved typo must not take monitoring down. The previous
					// config stays in force and the operator gets told why.
					log.Printf("config reload failed, keeping the running one: %v", err)
					return
				}
				if ch := sup.Sync(ctx, next.Targets); !ch.Empty() {
					log.Printf("config reloaded: %s", ch)
				}
				schedule, err := next.Schedule()
				if err != nil {
					log.Printf("config reload: maintenance windows unchanged: %v", err)
					return
				}
				tracker.SetSchedule(schedule)
			})
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	cancel()

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = srv.Shutdown(shutdownCtx)
	sup.Stop()
	wg.Wait()

	// Last thing, after the probers have stopped, so the snapshot reflects the
	// final state rather than one taken mid-cycle.
	if cfg.Alerting.StateFile != "" {
		if err := tracker.SaveState(); err != nil {
			log.Printf("incident state: could not save: %v", err)
		}
	}
}

// scopeOrAll renders an empty scope as the wildcard it actually is.
func scopeOrAll(s []string) string {
	if len(s) == 0 {
		return "<all>"
	}
	return strings.Join(s, ",")
}

// runSweeper drives incident expiry: it is the only path that emits a resolved
// notification, so it has to keep running even when every target is healthy.
func runSweeper(ctx context.Context, tr *alert.Tracker, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tr.Sweep()
		}
	}
}
