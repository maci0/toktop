// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// pipeGrace bounds Output's wait on the command's stdout after the process
// is killed by the context deadline. A grandchild inheriting that pipe
// (nested shells, lsof helpers) would otherwise hold the read end open and
// pin Discover / MatchingEndpoints, which run on a timer for the dashboard
// lifetime.
const pipeGrace = 500 * time.Millisecond

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = pipeGrace
	// Each call gets its own process group, so the deadline kill reaches a
	// wrapper's whole tree instead of orphaning its children to init. These
	// run on a dashboard timer, so survivors would accumulate one per call
	// for the life of the process. The Cancel also fires when pipeGrace
	// elapses because a surviving grandchild still holds stdout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	return cmd.Output()
}
