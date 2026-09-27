package main

import (
	"runtime/debug"
	"strings"
)

// The release stamp baked in at build time.

var version = ""

func init() {
	moduleVersion := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = bi.Main.Version
	}
	version = resolveVersion(version, moduleVersion)
}

// resolveVersion prefers any ldflags stamp, then the module version recorded
// at compile time. "(devel)" is a local tree, not a release. "dev" is a real
// stamp (`make build`); it must not be replaced with a VCS pseudo-version.
func resolveVersion(stamped, moduleVersion string) string {
	stamped = strings.TrimPrefix(stamped, "v")
	if stamped != "" {
		return stamped
	}
	moduleVersion = strings.TrimPrefix(moduleVersion, "v")
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return moduleVersion
	}
	return "dev"
}

// cliFlags is the top-level FlagSet. Registration is independent of
