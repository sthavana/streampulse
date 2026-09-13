package config

import (
	"context"
	"crypto/sha256"
	"os"
	"time"
)

// DefaultReload is how often the config file is checked for changes.
const DefaultReload = 10 * time.Second

// Reload returns the poll interval: the configured one, the default when
// unset, or zero when it has been turned off with a negative value.
func (c *Config) Reload() time.Duration {
	switch {
	case c.ReloadSeconds < 0:
		return 0
	case c.ReloadSeconds == 0:
		return DefaultReload
	default:
		return time.Duration(c.ReloadSeconds) * time.Second
	}
}

// Watch polls path and calls onChange whenever its contents change.
//
// Polling rather than filesystem notifications, for two reasons. It needs no
// dependency, and it is indifferent to how the file was written -- an editor
// that renames a temp file over the original, a ConfigMap remounted by
// Kubernetes as a new symlink, and a plain in-place write all look the same
// to a hash of the contents, and all look different to a naive watch on the
// inode.
//
// A change that fails to load is reported with a nil config and an error
// rather than swallowed. Monitoring must not stop because someone saved a
// typo, so the caller logs it and keeps running on what it already had.
//
// One exception to that, and it is the difference between a useful error and
// noise: a write that is still in progress. Not every editor renames a temp
// file into place -- a shell redirect, or a bind-mounted file edited where it
// lies, truncates and then writes -- and a poll landing in that window reads
// half a document and calls it a syntax error. So a config that fails to load
// is given one more tick to settle, and only reported if it is still broken
// when the file has stopped changing. A genuine typo is reported one poll
// later than it used to be, which nobody will notice, and a half-written file
// is not reported at all, which was showing up as an error nobody could
// reproduce.
func Watch(ctx context.Context, path string, every time.Duration, onChange func(*Config, error)) {
	if every <= 0 {
		return
	}
	last, _ := hash(path)
	// pending holds the hash of a version that failed to load, waiting to see
	// whether the next tick still reads the same bytes.
	var pending string

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sum, err := hash(path)
			if err != nil {
				// The file being briefly absent is what a rename-into-place
				// looks like from here. The next tick will see the new one.
				continue
			}
			if sum == last {
				continue
			}
			cfg, loadErr := Load(path)
			if loadErr != nil && sum != pending {
				// First sighting of bytes that do not load. Do not accept
				// them as the current version, so that a writer still working
				// is picked up on a later tick either way.
				pending = sum
				continue
			}
			last, pending = sum, ""
			onChange(cfg, loadErr)
		}
	}
}

func hash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return string(sum[:]), nil
}
