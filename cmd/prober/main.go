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
	"streampulse/internal/metrics"
	"streampulse/internal/probe"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	reg := metrics.New()

	var notifier alert.Notifier = alert.NewJSONNotifier()
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

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	go func() {
		log.Printf("metrics + health server on %s (/metrics, /healthz)", cfg.MetricsAddr)
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
	for _, t := range cfg.Targets {
		wg.Add(1)
		go func(t config.Target) {
			defer wg.Done()
			runTarget(ctx, pr, t)
		}(t)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	cancel()

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = srv.Shutdown(shutdownCtx)
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

func runTarget(ctx context.Context, pr *probe.Prober, t config.Target) {
	log.Printf("probing %q every %s: %s", t.Name, t.Interval(), t.URL)
	probeOnce(ctx, pr, t) // run immediately, then on the ticker
	ticker := time.NewTicker(t.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeOnce(ctx, pr, t)
		}
	}
}

func probeOnce(ctx context.Context, pr *probe.Prober, t config.Target) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr.ProbeTarget(c, t)
}
