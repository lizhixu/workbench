// Package version carries the build version shared by the control plane and the
// agent. Both binaries come from this module, so the agent reports the version
// it was built with and the control plane advertises the version it ships; the
// difference between the two is what marks an agent as outdated.
package version

import "runtime/debug"

// Version is overridden at build time via the linker:
//
//	go build -ldflags "-X watchman/internal/version.Version=v1.2.3" ./server/cmd/watchman
//	go build -ldflags "-X watchman/internal/version.Version=v1.2.3" ./agent/cmd/watchman-agent
//
// See the Makefile targets build-server / build-agent / build-all.
var Version = "0.1.0-dev"

// Commit is the git commit SHA the binary was built from (short form).
// Injected at build time; empty for plain `go build` without ldflags.
var Commit = ""

// BuildTime is the UTC timestamp the binary was built at (RFC3339).
// Injected at build time; empty for plain `go build` without ldflags.
var BuildTime = ""

// devVersion is the placeholder used when no version was injected.
const devVersion = "0.1.0-dev"

// Get returns the build version. It prefers the linker-injected value and
// falls back to the module version recorded by the Go toolchain (which is set
// for `go install` from a tagged commit), so a binary is never anonymous in a
// way that silently breaks agent upgrade comparisons.
func Get() string {
	if Version != "" && Version != devVersion {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return Version
}
