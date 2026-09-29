//go:build !unix

package core

import "os/exec"

// GroupKill is a no-op where there is no process group to signal. On Windows
// the deadline kill is os/exec's Process.Kill, which terminates the direct
// child only; anything that child spawned survives, reparented. The unix path
// closes the same hole with a group signal and Windows has no equivalent here
// without a Job Object, so a wrapper that hangs leaks its tree once per poll
// for as long as it keeps hanging.
func GroupKill(*exec.Cmd) {}
