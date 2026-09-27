// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"context"
	"time"
)

// Tick runs one background loop until ctx is done, calling warm once up front
// and refresh on every tick. It returns a channel closed once the loop has
// returned, so a caller that must not outlive it can join.
//
// The first call is not deferred to the first tick. A manual probe should put
// a request on the wire when the dashboard comes up, not one interval later,
// and the host-vitals poller warms its cache before the first frame reads it.
// Where the two passes are the same function it is passed twice: the process
// table has no cache to warm, and a second helper for that case would be the
// same loop written again.
func Tick(ctx context.Context, every time.Duration, warm, refresh func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		warm()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				refresh()
			}
		}
	}()
	return done
}
