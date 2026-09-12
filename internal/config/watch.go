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
func Watch(ctx context.Context, path string, every time.Duration, onChange func(*Config, error)) {
	if every <= 0 {
		return
	}
	last, _ := hash(path)

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
			last = sum
			onChange(Load(path))
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
