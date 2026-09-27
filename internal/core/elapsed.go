// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import "time"

// Age is how much later than then now is, floored at zero.
//
// Every expiry window in this program ages an entry by subtracting the stamp
// it recorded from a later clock read, and that clock is a wall clock on a
// real run. A backward step (an NTP correction, a laptop resuming from sleep,
// a restored VM snapshot) makes the subtraction negative, and a negative age
// satisfies every "younger than the window" test: the entry then never
// expires. A probe wave gate stops spacing its waves, a transcript listing
// stops re-walking, a remote sample stays on screen after the host stopped
// answering, each until real time catches back up to the value it lost.
//
// A window asks how long ago, never whether the clock moved, so the floor is
// the whole of it. Code that needs the direction (a sender claiming an
// instant ahead of arrival, which [core.Snapshot] stamps rather than ages)
// subtracts the other way round and keeps the raw result.
func Age(now, then time.Time) time.Duration {
	return max(now.Sub(then), 0)
}
