// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "github.com/maci0/toktop/internal/core"

// Ticker is the timing source [Watcher.Run] waits on: C is the channel the
// loop selects on, Stop releases whatever paces it.
type Ticker = core.Ticker

// Pacer produces the [Ticker] a [Watcher.Run] loop runs on. [WallPacer] is
// wall-clock time and [NewVirtualPacer] is a simulated timeline a driver
// steps; [Watcher.SetPacer] takes one.
//
// It names the same type as the internal definition rather than declaring a
// second interface, so a value satisfies both and no type has to be
// reimplemented to reach the published surface.
type Pacer = core.Pacer

// WallPacer paces [Watcher.Run] on wall-clock time, which is what a watcher
// runs on unless [Watcher.SetPacer] is given another.
var WallPacer Pacer = core.WallPacer

// VirtualPacer paces [Watcher.Run] on a simulated timeline: the tickers it
// hands out fire when Fire is called and never on their own, so a replayed
// run's schedule is the step sequence rather than wall-clock time. Pair it
// with a clock injected through [Watcher.SetNow], and the readings and the
// frames stamped with them both come from the driver.
type VirtualPacer = core.VirtualPacer

// NewVirtualPacer returns a [VirtualPacer] whose tickers stay silent until
// the driver fires them.
func NewVirtualPacer() *VirtualPacer { return core.NewVirtualPacer() }
