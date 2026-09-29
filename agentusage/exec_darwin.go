// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"context"
	"os/exec"
	"time"

	"github.com/maci0/toktop/internal/core"
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
	core.GroupKill(cmd)
	return cmd.Output()
}
