package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, path string, targets string) {
	t.Helper()
	body := `{"metrics_addr":":9090","targets":[` + targets + `]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// awaitChange writes a config repeatedly, varying it each time, until the
// watcher reports one. Writing once races the watcher's first poll: it takes
// its baseline hash when its goroutine starts, and a write that lands first
// becomes the baseline and is never seen as a change. Varying the content
// means whatever baseline it took, the next write differs from it.
func awaitChange[T any](t *testing.T, write func(int), got <-chan T) T {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		write(i)
		select {
		case v := <-got:
			return v
		case <-time.After(25 * time.Millisecond):
		}
	}
	var zero T
	t.Fatal("the watcher never reported a change")
	return zero
}

func TestReloadInterval(t *testing.T) {
	cases := map[int]time.Duration{
		0:  DefaultReload,   // unset
		5:  5 * time.Second, // configured
		-1: 0,               // turned off
	}
	for in, want := range cases {
		if got := (&Config{ReloadSeconds: in}).Reload(); got != want {
			t.Errorf("ReloadSeconds %d -> %v, want %v", in, got, want)
		}
	}
}

func TestWatchReportsAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan *Config, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(c *Config, err error) {
		if err == nil {
			got <- c
		}
	})

	c := awaitChange(t, func(i int) {
		writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"},{"name":"b","url":"http://b/`+itoa(i)+`.m3u8"}`)
	}, got)
	if len(c.Targets) != 2 {
		t.Errorf("reloaded config has %d targets, want 2", len(c.Targets))
	}
}

func itoa(i int) string { return string(rune('a' + i%26)) }

// An unchanged file must not produce a reload, or the supervisor churns and
// the log fills with nothing.
func TestWatchIsQuietWhenNothingChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 8)
	go Watch(ctx, path, 5*time.Millisecond, func(*Config, error) { calls <- struct{}{} })

	// Touch the file without changing its contents: the hash, not the
	// timestamp, is what decides.
	time.Sleep(60 * time.Millisecond)
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	time.Sleep(60 * time.Millisecond)

	if n := len(calls); n != 0 {
		t.Errorf("%d reloads with no content change", n)
	}
}

// Monitoring must not stop because someone saved a typo. The error is
// reported so the caller can log it and keep running on what it has.
func TestWatchReportsBadConfigWithoutStopping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 4)
	good := make(chan *Config, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(c *Config, err error) {
		if err != nil {
			errs <- err
		} else {
			good <- c
		}
	})

	awaitChange(t, func(i int) {
		if err := os.WriteFile(path, []byte("{not json "+itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}, errs)

	// And the watcher is still going: fixing it is picked up.
	c := awaitChange(t, func(i int) {
		writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"},{"name":"b","url":"http://b/`+itoa(i)+`.m3u8"}`)
	}, good)
	if len(c.Targets) != 2 {
		t.Errorf("recovered config has %d targets", len(c.Targets))
	}
}

// A validation failure -- not a syntax error -- must be caught too, since
// that is what a duplicated name or a bad maintenance window looks like.
func TestWatchReportsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(_ *Config, err error) {
		if err != nil {
			errs <- err
		}
	})

	err := awaitChange(t, func(i int) {
		writeConfig(t, path, `{"name":"a","url":"http://a/`+itoa(i)+`.m3u8"},{"name":"a","url":"http://b/x.m3u8"}`)
	}, errs)
	if err == nil {
		t.Fatal("a duplicate target name should have been reported")
	}
}

// A file that briefly vanishes is what an editor renaming a temp file into
// place looks like. It is not a change and not an error.
func TestWatchToleratesTheFileBrieflyMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan error, 8)
	go Watch(ctx, path, 5*time.Millisecond, func(_ *Config, err error) { calls <- err })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if n := len(calls); n != 0 {
		t.Errorf("a missing file produced %d callbacks", n)
	}

	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"},{"name":"b","url":"http://b/x.m3u8"}`)
	select {
	case err := <-calls:
		if err != nil {
			t.Errorf("the replacement was reported as an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the replacement file was never picked up")
	}
}

func TestWatchStopsWithTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, path, 5*time.Millisecond, func(*Config, error) {}); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return when its context was cancelled")
	}
}

func TestWatchDisabled(t *testing.T) {
	done := make(chan struct{})
	go func() { Watch(context.Background(), "irrelevant", 0, func(*Config, error) {}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a zero interval should return immediately, not poll")
	}
}

// A writer that truncates and then writes -- a shell redirect, or a
// bind-mounted file edited where it lies -- leaves a window in which the file
// is half a document. A poll landing in it used to report a syntax error
// nobody could reproduce.
func TestWatchIgnoresAHalfWrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan error, 16)
	// Slow enough that the timeline below is unambiguous: one poll sees the
	// truncated file and the next sees the finished one, which is exactly
	// what a write in progress looks like.
	go Watch(ctx, path, 50*time.Millisecond, func(_ *Config, err error) { calls <- err })

	whole := `{"metrics_addr":":9090","targets":[{"name":"a","url":"http://a/x.m3u8"},` +
		`{"name":"b","url":"http://b/x.m3u8"}]}`
	if err := os.WriteFile(path, []byte(whole[:40]), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := os.WriteFile(path, []byte(whole), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-calls:
		if err != nil {
			t.Fatalf("a write in progress was reported as an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the completed file was never picked up")
	}
}

// The settle rule must not swallow a real one. A config that stays broken is
// still reported, one tick later than it used to be.
func TestWatchStillReportsAConfigThatStaysBroken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, `{"name":"a","url":"http://a/x.m3u8"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan error, 16)
	go Watch(ctx, path, 5*time.Millisecond, func(_ *Config, err error) { calls <- err })
	// Let the watcher take its baseline first: a write that lands before it
	// does becomes the baseline and is never seen as a change.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(path, []byte(`{"targets": oops`), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-calls:
		if err == nil {
			t.Fatal("a broken config was accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a config that stayed broken was never reported")
	}
}
