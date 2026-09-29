// Package version reports the build identity of ClinePassProxy.
//
// Version and Commit are injected by GitHub Actions through -ldflags.
package version

import "runtime/debug"

// Name is the product name reported by /api/version and the UI.
const Name = "ClinePassProxy"

// Version is the release version, overridden at build time.
var Version = "0.0.0-dev"

// Commit is the source revision, overridden at build time.
var Commit = ""

// BuildTime is the build timestamp, overridden at build time.
var BuildTime = ""

// ResolvedCommit returns the injected commit, falling back to VCS metadata
// that the Go toolchain embeds for local builds.
func ResolvedCommit() string {
	if Commit != "" {
		return Commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

// String returns a compact human-readable build identity.
func String() string {
	return Name + " " + Version + " (" + ResolvedCommit() + ")"
}
