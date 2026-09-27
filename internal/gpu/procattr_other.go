//go:build !unix

package gpu

import "os/exec"

// groupKill is a no-op where there is no process group to signal. Windows
// terminates the child tree through the console it inherits rather than a
// signal, so a deadline kill already reaches what the tool spawned.
func groupKill(*exec.Cmd) {}
