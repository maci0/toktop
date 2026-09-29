// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The drawn frame is the last thing a run produces, and its header clock is
// ticked by a wall-clock timer while every frame under it is stamped on the
// run's own timeline. Two runs of one step sequence have to draw the same
// bytes: a run captured from one machine is only worth keeping if the run that
// failed on another can be diffed against it, and a header reading the
// reader's own wall clock made every capture unreproducible.
//
// The frames and the clock are the ones a demo source hands over, and each
// tick carries the wall instant bubbletea's timer delivers, which is the value
// the model has to refuse to use.
func TestDemoDashboardReplaysByteForByte(t *testing.T) {
	const steps = 6
	origin := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)

	render := func() string {
		sim := origin
		m := New(Config{Version: "t", Demo: true, DemoSeed: 7, DemoOrigin: origin}, nil)
		m.SetNow(func() time.Time { return sim })
		m.w, m.h, m.ready = 110, 36, true

		var b strings.Builder
		for i := range steps {
			sim = origin.Add(time.Duration(i) * time.Second)
			nm, _ := m.Update(snapMsg(core.Snapshot{
				At:        sim,
				Uptime:    time.Duration(i) * time.Minute,
				Providers: []core.ProviderSnapshot{{Label: "ollama", Addr: "127.0.0.1:11434", OK: true, OutTokPS: float64(100 * (i + 1)), InTokPS: 400}},
			}))
			m = nm.(Model)
			nm, _ = m.Update(tickMsg(time.Now()))
			m = nm.(Model)
			out := strip(m.View())
			if want := sim.Format("15:04:05"); !strings.Contains(out, want) {
				t.Fatalf("step %d header does not read %s:\n%s", i, want, out)
			}
			b.WriteString(out)
			b.WriteString("\n")
		}
		return b.String()
	}

	a, b := render(), render()
	if a != b {
		t.Fatalf("two dashboard runs of one step sequence diverged:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
	if strings.Count(a, "03:04:") != steps {
		t.Errorf("expected %d frames on the simulated timeline, got:\n%s", steps, a)
	}
	if !strings.Contains(a, "DEMO seed 7") {
		t.Error("the run under test is not a demo run")
	}
	// The frames have to differ from each other, or the equality above proves
	// nothing about the sequence.
	if got := strings.Count(a, "tok/s out"); got != steps {
		t.Errorf("frames carry no per-step state, so the replay comparison is vacuous: %d\n%s", got, a)
	}
}

// A live dashboard has no simulated timeline to sit on, so the tick's own
// instant is the header. Pinning an origin must not leak into it.
func TestLiveDashboardTicksOnTheWallClock(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 110, 36, true
	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
	}))
	m = nm.(Model)
	wall := time.Date(2026, 8, 25, 12, 0, 0, 0, time.Local)
	nm, _ = m.Update(tickMsg(wall))
	if got := nm.(Model).clock; !got.Equal(wall) {
		t.Errorf("header clock = %v, want the tick instant %v", got, wall)
	}
}
