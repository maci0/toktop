// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/demo"
	"github.com/maci0/toktop/internal/ui"
)

func TestParseOrigin(t *testing.T) {
	for _, tt := range []struct {
		name  string
		in    string
		want  time.Time
		isErr bool
	}{
		{name: "rfc3339", in: "2026-01-02T03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "rfc3339 offset", in: "2026-01-02T03:04:05+02:00", want: time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC)},
		{name: "unix seconds", in: "1767322245", want: time.Unix(1767322245, 0).UTC()},
		{name: "negative unix seconds", in: "-1", want: time.Unix(-1, 0).UTC()},
		{name: "surrounding space", in: "  2026-01-02T03:04:05Z ", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "empty pins nothing", in: "", want: time.Time{}},
		{name: "blank pins nothing", in: "   ", want: time.Time{}},
		{name: "date alone", in: "2026-01-02", isErr: true},
		{name: "not an instant", in: "nope", isErr: true},
		{name: "trailing junk", in: "1767322245Z", isErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOrigin(tt.in)
			if tt.isErr {
				if err == nil {
					t.Fatalf("parseOrigin(%q) = %v, want an error", tt.in, got)
				}
				if !strings.Contains(err.Error(), "--origin") {
					t.Errorf("error does not name the flag: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOrigin(%q): %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("parseOrigin(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// The seed decides every simulated value; the origin decides the instant they
// are stamped with. Two runs carrying both must render the same bytes, or a
// captured run cannot be replayed and diffed against the one that failed. The
// frames come off a real demo Source and a real JSON report, so a clock read
// left on any path between the flag and the frame shows up here.
func TestDemoRunReplaysByteForByte(t *testing.T) {
	const frames = 5
	render := func() string {
		s := demo.NewSource(10*time.Millisecond, 7)
		origin, err := parseOrigin("2026-01-02T03:04:05Z")
		if err != nil {
			t.Fatalf("parseOrigin: %v", err)
		}
		s.SetOrigin(origin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan core.Snapshot, frames)
		go s.Run(ctx, ch)
		cfg := ui.Config{Version: "test", Demo: true, DemoSeed: s.Seed(), DemoOrigin: origin, PollEvery: 10 * time.Millisecond}
		var b strings.Builder
		for i := 0; i < frames; i++ {
			out, err := ui.JSONFrame(cfg, <-ch)
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			b.WriteString(out)
		}
		return b.String()
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("two runs of one seed and origin diverged:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
	if !strings.Contains(a, `"demo_origin": "2026-01-02T03:04:05Z"`) {
		t.Error("the report does not carry the pinned origin, so a replay of it cannot name the input it needs")
	}
	if !strings.Contains(a, `"demo_seed": 7`) {
		t.Error("the report does not carry the seed, so a replay of it cannot name the input it needs")
	}
}

// An unpinned run has no origin to report: the field is omitted rather than
// rendered as a zero time, which would read as a pinned 0001-01-01.
func TestJSONOmitsUnpinnedDemoOrigin(t *testing.T) {
	out, err := ui.JSONFrame(ui.Config{Version: "test", Demo: true, DemoSeed: 7}, core.Snapshot{})
	if err != nil {
		t.Fatalf("JSONFrame: %v", err)
	}
	if strings.Contains(out, "demo_origin") {
		t.Fatalf("unpinned run reported an origin:\n%s", out)
	}
}

// A different seed must still change the run, or the replay above would pass
// on a source that draws nothing.
func TestDemoSeedChangesTheRun(t *testing.T) {
	render := func(seed int64) string {
		s := demo.NewSource(time.Second, seed)
		origin, err := parseOrigin("2026-01-02T03:04:05Z")
		if err != nil {
			t.Fatalf("parseOrigin: %v", err)
		}
		s.SetOrigin(origin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan core.Snapshot, 1)
		go s.Run(ctx, ch)
		out, err := ui.JSONFrame(ui.Config{Version: "test", Demo: true, DemoSeed: seed, DemoOrigin: origin}, <-ch)
		if err != nil {
			t.Fatalf("JSONFrame: %v", err)
		}
		return out
	}
	if render(7) == render(8) {
		t.Fatal("seeds 7 and 8 rendered the same report")
	}
}
