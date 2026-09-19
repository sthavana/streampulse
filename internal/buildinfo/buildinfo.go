// Package buildinfo answers the question "what exactly is running here".
//
// It matters more for this tool than for most. A prober is the thing you
// consult when everything else is on fire, and the first question about a
// surprising finding is whether the binary producing it is the one you think
// it is. A monitoring tool that cannot say what version it is asks its
// operator to take that on trust.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// version is set at link time for a release build:
//
//	go build -ldflags "-X streampulse/internal/buildinfo.version=v0.3.1"
//
// Left as "dev" for an ordinary local build, which is the honest answer: a
// binary built from a working tree is not a release and should not claim to be.
var version = "dev"

// Version returns the release version, or "dev" plus the revision it was built
// from when there is no release tag.
//
// The revision comes from the VCS stamp the Go toolchain embeds automatically
// since 1.18, so a build from a clean checkout identifies itself without any
// build-system cooperation at all.
func Version() string {
	if version != "dev" {
		return version
	}
	if rev, dirty := revision(); rev != "" {
		if dirty {
			return "dev-" + rev + "-dirty"
		}
		return "dev-" + rev
	}
	return version
}

// String is what -version prints and what each binary logs at startup: the
// version, the toolchain, and the platform, which between them account for
// nearly every "it behaves differently on that host" question.
func String(program string) string {
	return program + " " + Version() + " (" + runtime.Version() + " " +
		runtime.GOOS + "/" + runtime.GOARCH + ")"
}

func revision() (rev string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			// Short form: nobody reads forty hex characters, and the first
			// twelve identify a commit in any repository this size.
			rev = s.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
		case "vcs.modified":
			dirty = strings.EqualFold(s.Value, "true")
		}
	}
	return rev, dirty
}
