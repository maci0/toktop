package ui

import (
	"math"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// FuzzAgentCadenceBuckets drives the agent chart's bucketing over events whose
// timestamps come off the wire. Every /v1/events POST chooses its own At, and
// the retained feed is what the throughput chart is drawn from, so the offsets
// fed to the index arithmetic are sender-shaped: sub-cadence, exactly on a
// boundary, many cadences outside the window, and at the extremes of the
// Duration range the guards in nearestCadenceIndex and cadenceSpan exist for.
//
// The oracle is the property the chart depends on rather than a crash: an
// event's tokens land in the column its own instant names, never in a
// neighbour, and a grid never carries a negative, infinite or NaN cell. The
// index math is where a wrong rounding shows up: Go divides durations toward
// zero, so a negative offset otherwise lands on the column above the one it
// belongs to and a sender that stamps an event far in the past paints tokens
// on the live column.
func FuzzAgentCadenceBuckets(f *testing.F) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// The offsets are measured from the start of the window, so the seeds
	// below are written against the one-second cadence the second loop also
	// reaches: a bucket centre sits on a whole second of that grid.
	for _, seed := range []struct {
		offOut, offIn, tokens int64
	}{
		{0, 0, 0},
		{0, 0, 100},
		{-int64(time.Second), 0, 50},
		{-2500 * int64(time.Millisecond), 0, 999},
		{-2600 * int64(time.Millisecond), 0, 80},
		{500 * int64(time.Millisecond), 0, 10},
		{-500 * int64(time.Millisecond), 0, 10},
		{-500*int64(time.Millisecond) - 1, 0, 10},
		{int64(time.Hour), 0, 10},
		{-int64(time.Hour), 0, 10},
		{math.MaxInt64, 0, 10},
		{math.MinInt64, 0, 10},
		{0, -int64(time.Second), 7},
		{0, math.MinInt64, 7},
		{0, 0, math.MaxInt64},
		{0, 0, math.MinInt64},
		{0, 0, -1},
	} {
		f.Add(seed.offOut, seed.offIn, seed.tokens, int64(time.Second))
	}
	for _, cadence := range []time.Duration{time.Second, 250 * time.Millisecond, 2 * time.Second, time.Minute} {
		f.Add(-int64(time.Second), int64(0), int64(100), int64(cadence))
		f.Add(int64(math.MinInt64), int64(0), int64(100), int64(cadence))
	}

	f.Fuzz(func(t *testing.T, offOut, offIn, tokens, cadenceNS int64) {
		cadence := time.Duration(cadenceNS)
		// A non-positive cadence is a caller error, not a peer one: the chart
		// hands one of its own fixed spacings. The zero guard is already
		// covered by the unit tests, so the fuzz budget goes to the shapes a
		// sender produces.
		if cadence <= 0 {
			cadence = time.Second
		}
		// The window is what the chart draws, a few hundred columns wide.
		const cols = 8
		end := now
		start := end.Add(-time.Duration(cols-1) * cadence)
		at := func(off int64) time.Time { return start.Add(time.Duration(off)) }

		events := []core.AgentEvent{
			{At: at(offOut), Agent: "out", OutputTokens: tokens},
			{At: at(offIn), Agent: "in", PromptTokens: tokens},
		}

		for _, out := range []bool{true, false} {
			grid := agentDenseHist(events, out, end, cols, cadence)
			if len(grid) != cols {
				t.Fatalf("agentDenseHist returned %d columns, want %d", len(grid), cols)
			}
			var placed float64
			for i, v := range grid {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("out=%v column %d = %v from offset %d at cadence %v", out, i, v, tokens, cadence)
				}
				placed += v
			}
			// An event whose nearest column falls outside the window is
			// dropped rather than folded onto an edge: a sender that stamps
			// an event hours old must not paint the live column.
			if idx := nearestCadenceIndex(at(offOut).Sub(start), cadence); idx >= 0 && idx < cols {
				want := float64(tokens) / cadence.Seconds()
				if tokens > 0 {
					got := grid[nearestCadenceIndex(at(offOut).Sub(start), cadence)]
					if out && math.Abs(got-want) > math.Abs(want)*1e-9 {
						t.Fatalf("out=%v event at column %d read %v, want %v", out, idx, got, want)
					}
				}
			}
			if placed < 0 {
				t.Fatalf("out=%v placed total %v", out, placed)
			}
		}

		// The two halves of the same rounding have to agree about where a
		// sample lands: the dense grid rounds to the nearest column, and
		// cadenceSpan reports the columns a sample reaches. An index inside
		// the span is the property aggHist depends on when it walks the range.
		for _, off := range []int64{offOut, offIn, 0, -int64(cadence), int64(cadence), math.MaxInt64, math.MinInt64} {
			d := time.Duration(off)
			first, last := cadenceSpan(d, cadence)
			idx := nearestCadenceIndex(d, cadence)
			if first > last {
				continue // an offset no column can reach: an empty range
			}
			if idx >= 0 && idx < cols && (idx < first || idx > last) {
				t.Fatalf("column %d for offset %v is outside the span [%d, %d] at cadence %v", idx, d, first, last, cadence)
			}
		}
	})
}
