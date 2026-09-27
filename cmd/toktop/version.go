package main

import (
	"runtime/debug"
	"strings"
)

// version is the release stamp. Empty means unstamped: init fills it from the
// module version Go embeds (`go install @v0.5.0`), then "dev". Release and
// `make build` override it with -ldflags "-X main.version=...", including the
// Makefile's default "dev". The source default must not be a real tag:
// "0.1.0" made `go install @latest` report the first release, so --version
// lied and `toktop update` always thought it was behind.
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
