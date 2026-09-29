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

// KillGroup has nothing to signal here, for the reason GroupKill has: with no
// process group of the command's own there is no group to address, so the
// deferred call every caller makes after it is a no-op rather than a missing
// one. The tree a hung Windows wrapper leaves behind is the gap the comment
// above names.
func KillGroup(*exec.Cmd) {}
