package ui

import (
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// benchEnginesOnly builds a snapshot with providers and stamped histories and
// no agent feed, which is what a run watching engines alone sees.
func benchEnginesOnly() core.Snapshot {
	now := time.Unix(1789581724, 0)
	out := make([]float64, core.HistoryLen)
	in := make([]float64, core.HistoryLen)
	for i := range out {
		out[i] = 20 + float64(i%37)*3.5
		in[i] = 400 + float64(i%53)*11
	}
	provs := make([]core.ProviderSnapshot, 3)
	for i := range provs {
		provs[i] = core.ProviderSnapshot{
			Label: "engine", OK: true,
			OutHist: out, InHist: in,
			OutStamps: stamps(now, len(out), time.Second), InStamps: stamps(now, len(in), time.Second),
		}
	}
	return core.Snapshot{At: now, Providers: provs}
}

// benchWithAgents is benchEnginesOnly over a feed that does fill the dense
// agent grid, so the series appends it and the reservation has to cover it.
func benchWithAgents() core.Snapshot {
	s := benchEnginesOnly()
	now := time.Unix(1789581724, 0)
	s.Agents = make([]core.AgentEvent, core.AgentHistoryLen)
	for i := range s.Agents {
		s.Agents[i] = core.AgentEvent{
			At:    now.Add(time.Duration(i) * time.Second),
			Agent: "claude", OutputTokens: int64(20 + i%97), PromptTokens: int64(100 + i),
		}
	}
	return s
}

// BenchmarkTimedSeriesEnginesOnly measures the engines-only series build: a
// run watching engines with no agent feed never fills the dense grid, so the
// series reserves room for it without appending to it.
func BenchmarkTimedSeriesEnginesOnly(b *testing.B) {
	s := benchEnginesOnly()
	b.ReportAllocs()
	for b.Loop() {
		if len(timedSeries(s, true, time.Second)) == 0 {
			b.Fatal("empty series")
		}
	}
}

// BenchmarkTimedSeriesWithAgents is the same series over a feed that does
// fill the grid, so the reservation still has to cover it.
func BenchmarkTimedSeriesWithAgents(b *testing.B) {
	s := benchWithAgents()
	b.ReportAllocs()
	for b.Loop() {
		tv := timedSeries(s, true, time.Second)
		if len(tv) == 0 {
			b.Fatal("empty series")
		}
		// The engines' own samples plus the dense grid appended for the feed.
		if want := 3*core.HistoryLen + core.HistoryLen; len(tv) != want {
			b.Fatalf("series len = %d, want %d", len(tv), want)
		}
	}
}
