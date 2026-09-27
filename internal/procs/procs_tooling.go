//go:build darwin || windows

package procs

import "time"

// listTimeout bounds OS-tooling process listings (darwin ps, windows
// PowerShell CIM). A hung tool must not stall every snapshot; the linux path
// reads procfs directly and never approaches it.
const listTimeout = 15 * time.Second

// listPipeGrace bounds the post-kill wait on the listing tool's output pipes
// (see Cmd.WaitDelay): a grandchild inheriting stdout must not keep the
// killed tool's Output call blocked forever.
const listPipeGrace = 500 * time.Millisecond
