// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/demo"
	"github.com/maci0/toktop/internal/ui"
)

// --origin configures the demo timeline and nothing else, so only a demo run
// reads it. A malformed value must abort a demo run (the replay is pinned to
// it) and leave a non-demo run alone: a stale --origin in a shell alias or a
// wrapper otherwise failed every real run over an input that run had no use
// for, and the run then went on to say the flag had no effect anyway.
func TestResolveOriginGatesOnDemo(t *testing.T) {
	const bad = "not-an-instant"
	for _, tt := range []struct {
		name  string
		demo  bool
		val   string
		isErr bool
		want  time.Time
	}{
		{name: "demo accepts a valid instant", demo: true, val: "2026-01-02T03:04:05Z",
			want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "demo rejects a malformed instant", demo: true, val: bad, isErr: true},
		{name: "demo accepts an empty value", demo: true, val: "", want: time.Time{}},
		{name: "non-demo ignores a malformed value", demo: false, val: bad, want: time.Time{}},
		{name: "non-demo ignores a valid value", demo: false, val: "2026-01-02T03:04:05Z", want: time.Time{}},
		{name: "non-demo ignores a date-shaped value", demo: false, val: "20260102", want: time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOrigin(tt.demo, tt.val)
			if tt.isErr {
				if err == nil {
					t.Fatalf("resolveOrigin(%v, %q) = %v, want an error", tt.demo, tt.val, got)
				}
				if !strings.Contains(err.Error(), "--origin") {
					t.Errorf("error does not name the flag: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveOrigin(%v, %q): %v", tt.demo, tt.val, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("resolveOrigin(%v, %q) = %v, want %v", tt.demo, tt.val, got, tt.want)
			}
		})
	}
}

// The gate and warnIgnoredFlags have to agree on when --origin is in force: a
// run the warnings call one mode and the parser another either rejects an
// unused value or accepts one it never reads.
func TestResolveOriginAgreesWithWarnIgnoredFlags(t *testing.T) {
	f := &cliFlags{origin: "not-an-instant"}
	if _, err := resolveOrigin(f.demo, f.origin); err != nil {
		t.Fatalf("a run warnIgnoredFlags would call non-demo rejects --origin: %v", err)
	}
	f.demo = true
	if _, err := resolveOrigin(f.demo, f.origin); err == nil {
		t.Fatal("a demo run accepts a malformed --origin")
	}
}

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

// The origin a report prints is the one a replay is fed back, so it has to
// carry the instant whole. --origin accepts a fractional RFC 3339 instant and
// the timeline is laid out from it: an origin printed to whole seconds named
// a different instant, and every stamp in the replayed run moved by the
// fraction that was dropped.
func TestDemoOriginRoundTripsSubSecondPrecision(t *testing.T) {
	const originArg = "2026-01-02T03:04:05.5Z"
	render := func(originArg string) (string, string) {
		origin, err := parseOrigin(originArg)
		if err != nil {
			t.Fatalf("parseOrigin(%q): %v", originArg, err)
		}
		s := demo.NewSource(10*time.Millisecond, 7)
		s.SetOrigin(origin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan core.Snapshot, 1)
		go s.Run(ctx, ch)
		cfg := ui.Config{Version: "test", Demo: true, DemoSeed: s.Seed(), DemoOrigin: origin, PollEvery: 10 * time.Millisecond}
		out, err := ui.JSONFrame(cfg, <-ch)
		if err != nil {
			t.Fatalf("JSONFrame: %v", err)
		}
		var rep struct {
			DemoOrigin string `json:"demo_origin"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		return out, rep.DemoOrigin
	}
	capture, reported := render(originArg)
	if reported != originArg {
		t.Fatalf("report names origin %q, want %q: feeding it back replays a different run", reported, originArg)
	}
	replay, _ := render(reported)
	if capture != replay {
		t.Fatalf("a run replayed from its own reported origin did not reproduce:\nfirst:\n%s\nsecond:\n%s", capture, replay)
	}
}

// --probe must not put the demo run back on the wall clock. Auto-probe on a
// simulated source is armed on the source and fired by its frames, so a run
// carrying a seed, an origin and a probe cadence still renders the same
// bytes, waves and their stamps included.
func TestDemoAutoProbeReplaysByteForByte(t *testing.T) {
	const frames = 40
	render := func() string {
		s := demo.NewSource(50*time.Millisecond, 7)
		origin, err := parseOrigin("2026-01-02T03:04:05Z")
		if err != nil {
			t.Fatalf("parseOrigin: %v", err)
		}
		s.SetOrigin(origin)
		s.ProbeEvery(time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan core.Snapshot, frames)
		go s.Run(ctx, ch)
		cfg := ui.Config{Version: "test", Demo: true, DemoSeed: s.Seed(), DemoOrigin: origin, PollEvery: 50 * time.Millisecond}
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
		t.Fatalf("two auto-probed runs of one seed and origin diverged:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
	// The cadence has to have produced something, or the replay above would
	// pass on a run that never probed. 40 frames at 50ms is 2s of simulated
	// time, so a 1s cadence runs two waves over the five simulated backends.
	if n := strings.Count(a, `"ttft_ms"`); n < 10 {
		t.Fatalf("auto-probe produced %d probe samples, want two waves of five", n)
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

// Seed 0 is a working seed. omitempty on a plain integer dropped exactly that
// one, so a replay of the report could not tell it from a run that named none.
func TestJSONKeepsDemoSeedZero(t *testing.T) {
	out, err := ui.JSONFrame(ui.Config{Version: "test", Demo: true, DemoSeed: 0}, core.Snapshot{})
	if err != nil {
		t.Fatalf("JSONFrame: %v", err)
	}
	if !strings.Contains(out, `"demo_seed": 0`) {
		t.Fatalf("demo seed 0 was omitted:\n%s", out)
	}
	plain, err := ui.JSONFrame(ui.Config{Version: "test", DemoSeed: 0}, core.Snapshot{})
	if err != nil {
		t.Fatalf("JSONFrame: %v", err)
	}
	if strings.Contains(plain, "demo_seed") {
		t.Fatalf("a non-demo run reported a seed:\n%s", plain)
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

// A bare date is not a Unix second. parseOrigin has to refuse it, because the
// alternative is a run pinned to a 1970 instant that looks like a replay of a
// capture nobody took, with the difference showing only in the timestamps the
// operator pinned the origin to hold still.
func TestParseOriginRefusesAMistypedDate(t *testing.T) {
	for _, s := range []string{"20260928", "2026-09-28", "20260928120000", "0.5"} {
		if at, err := parseOrigin(s); err == nil {
			t.Errorf("parseOrigin(%q) = %v, want a rejection", s, at)
		}
	}
	// The width that a Unix second does have is still accepted, on both sides
	// of the epoch.
	for _, s := range []string{"1700000000", "-1000"} {
		if _, err := parseOrigin(s); err != nil {
			t.Errorf("parseOrigin(%q) = %v, want an instant", s, err)
		}
	}
}
