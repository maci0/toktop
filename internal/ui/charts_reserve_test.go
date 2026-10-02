package ui

import (
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// agentEvents builds a feed of n events n seconds apart ending at end, all
// unattributed and all carrying output and prompt tokens.
func agentEvents(end time.Time, n int) []core.AgentEvent {
	evs := make([]core.AgentEvent, n)
	for i := range evs {
		evs[i] = core.AgentEvent{
			At:           end.Add(-time.Duration(n-1-i) * time.Second),
			OutputTokens: 10,
			PromptTokens: 20,
		}
	}
	return evs
}

// reserveSnapshot is a one-engine snapshot whose history is n samples a
// second apart ending at end, which is the shape timedSeries reserves against.
func reserveSnapshot(end time.Time, n int) core.Snapshot {
	prov := core.ProviderSnapshot{
		OutHist:   make([]float64, n),
		OutStamps: stamps(end, n, time.Second),
		InHist:    make([]float64, n),
		InStamps:  stamps(end, n, time.Second),
	}
	for i := range prov.OutHist {
		prov.OutHist[i] = float64(i)
		prov.InHist[i] = float64(i)
	}
	return core.Snapshot{Providers: []core.ProviderSnapshot{prov}}
}

// TestTimedSeriesReservesOnlyWhatItAppends pins the reservation timedSeries
// sizes its slice against: the dense agent grid is appended only when the feed
// holds an event this direction counts that did not go through an engine,
// because that is the only condition under which the grid's columns reach the
// series. A run watching engines alone appends no grid, so a reservation that
// counted one anyway would size every frame's slice for samples it never
// holds.
func TestTimedSeriesReservesOnlyWhatItAppends(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const n = 4
	s := reserveSnapshot(end, n)

	// Engines alone: neither direction appends a grid.
	tv := timedSeries(s, true, time.Second)
	if got, want := len(tv), n; got != want {
		t.Errorf("engines-only series len = %d, want %d: the dense grid was appended for a feed that never fills it", got, want)
	}
	if got, want := cap(tv), n+core.HistoryLen; got != want {
		t.Errorf("engines-only series cap = %d, want %d", got, want)
	}

	// A feed with unattributed tokens fills the grid, and both directions
	// append it.
	withAgents := s
	withAgents.Agents = agentEvents(end, 2)
	for _, out := range []bool{true, false} {
		tv := timedSeries(withAgents, out, time.Second)
		if got, want := len(tv), n+core.HistoryLen; got != want {
			t.Errorf("out=%v: series len = %d, want %d: the filled grid was not appended", out, got, want)
		}
		if cap(tv) < len(tv) {
			t.Errorf("out=%v: series cap %d is below its length %d", out, cap(tv), len(tv))
		}
	}

	// Every event through an engine: no grid is appended, in either
	// direction, so the series is the engine samples and nothing else.
	viaEngine := s
	viaEngine.Agents = agentEvents(end, 2)
	for i := range viaEngine.Agents {
		viaEngine.Agents[i].ViaEngine = "ollama"
	}
	for _, out := range []bool{true, false} {
		if got, want := len(timedSeries(viaEngine, out, time.Second)), n; got != want {
			t.Errorf("out=%v: engine-attributed feed appended %d entries, want %d", out, got, want)
		}
	}
}

// TestSeriesLengthTracksTheGrid pins the same rule from the other side: what
// timedSeries appends is exactly the nonzero dense grid the feed produces for
// that direction. It is the property the reservation above exists to serve,
// stated directly so a change to either side of it is caught here.
func TestSeriesLengthTracksTheGrid(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const n = 4
	feeds := map[string][]core.AgentEvent{
		"empty":                    nil,
		"zero":                     {{At: end}},
		"unattributed":             agentEvents(end, 3),
		"prompt only":              {{At: end, PromptTokens: 5}},
		"old prompt only":          {{At: end.Add(-time.Hour), OutputTokens: 7, PromptTokens: 7}},
		"everything via an engine": agentEvents(end, 3),
	}
	for i := range feeds["everything via an engine"] {
		feeds["everything via an engine"][i].ViaEngine = "ollama"
	}

	for name, feed := range feeds {
		s := reserveSnapshot(end, n)
		s.Agents = feed
		for _, out := range []bool{true, false} {
			grid := agentDenseHist(feed, out, end, core.HistoryLen, time.Second)
			nonzero := false
			for _, v := range grid {
				if v > 0 {
					nonzero = true
				}
			}
			series := timedSeries(s, out, time.Second)
			want := n
			if nonzero {
				want = n + core.HistoryLen
			}
			if len(series) != want {
				t.Errorf("%s out=%v: series len = %d, want %d (grid nonzero = %v)", name, out, len(series), want, nonzero)
			}
			if cap(series) < len(series) {
				t.Errorf("%s out=%v: series cap %d is below its length %d", name, out, cap(series), len(series))
			}
		}
	}
}
