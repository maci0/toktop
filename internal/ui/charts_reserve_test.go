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

// TestTimedSeriesReserveAndGrid pins both halves of the reservation rule
// timedSeries works under. The slice is sized against a fixed room for the
// dense agent grid (one entry per column) whatever the feed holds, because
// the grid is HistoryLen wide regardless of how much of it fills; and the
// grid's columns are appended only when agentDenseHist returns a nonzero one,
// which is the condition hasAgentTokens used to test in a second pass.
func TestTimedSeriesReserveAndGrid(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	prov := core.ProviderSnapshot{
		OutHist:   make([]float64, 4),
		OutStamps: make([]time.Time, 4),
		InHist:    make([]float64, 4),
		InStamps:  make([]time.Time, 4),
	}
	for i := range prov.OutHist {
		prov.OutHist[i] = float64(i)
		prov.OutStamps[i] = end.Add(-time.Duration(3-i) * time.Second)
		prov.InHist[i] = float64(i)
		prov.InStamps[i] = prov.OutStamps[i]
	}
	s := core.Snapshot{Providers: []core.ProviderSnapshot{prov}}

	// Engines alone: the grid never fills, so nothing is appended, but the
	// slice still holds the fixed grid reservation.
	tv := timedSeries(s, true, time.Second)
	if e := newestOf(tv); !e.Equal(end) {
		t.Errorf("end = %v, want the newest engine stamp %v", e, end)
	}
	if got, want := cap(tv), len(prov.OutHist)+core.HistoryLen; got != want {
		t.Errorf("engines-only series cap = %d, want %d: the grid reservation is what the slice is sized against", got, want)
	}
	if got, want := len(tv), len(prov.OutHist); got != want {
		t.Errorf("engines-only series len = %d, want %d: an unfilled grid appends nothing", got, want)
	}

	// A feed with unattributed tokens fills the grid, and both directions
	// reserve for it.
	withAgents := s
	withAgents.Agents = agentEvents(end, 2)
	for _, out := range []bool{true, false} {
		tv := timedSeries(withAgents, out, time.Second)
		if e := newestOf(tv); !e.Equal(end) {
			t.Errorf("out=%v: end = %v, want %v", out, e, end)
		}
		if got, want := len(tv), len(prov.OutHist)+core.HistoryLen; got != want {
			t.Errorf("out=%v: series len = %d, want %d", out, got, want)
		}
		if cap(tv) < len(tv) {
			t.Errorf("out=%v: series cap %d is below its length %d", out, cap(tv), len(tv))
		}
	}

	// Every event through an engine: the grid stays empty, so nothing is
	// appended and the series is the engine samples alone.
	viaEngine := s
	viaEngine.Agents = agentEvents(end, 2)
	for i := range viaEngine.Agents {
		viaEngine.Agents[i].ViaEngine = "ollama"
	}
	for _, out := range []bool{true, false} {
		if tv := timedSeries(viaEngine, out, time.Second); len(tv) != len(prov.OutHist) {
			t.Errorf("out=%v: engine-attributed feed appended %d entries, want the %d engine samples",
				out, len(tv), len(prov.OutHist))
		}
	}
}

// TestTimedSeriesAppendsGridExactlyWhenDenseHistIsNonzero pins the condition
// timedSeries gates the grid append on: one entry per column of agentDenseHist
// exactly when that grid holds a nonzero column, and none otherwise. That is
// the rule hasAgentTokens used to answer in a second pass over the feed, and
// a gate that disagreed with the grid would either append zeros the chart
// draws as activity or drop real columns off the left of the series.
func TestTimedSeriesAppendsGridExactlyWhenDenseHistIsNonzero(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	feeds := map[string][]core.AgentEvent{
		"empty":   nil,
		"zero":    {{At: end}},
		"output":  agentEvents(end, 3),
		"viaonly": {{At: end, ViaEngine: "ollama", OutputTokens: 9, PromptTokens: 9}},
	}
	feeds["output via engine"] = agentEvents(end, 3)
	for i := range feeds["output via engine"] {
		feeds["output via engine"][i].ViaEngine = "ollama"
	}
	feeds["prompt only"] = []core.AgentEvent{{At: end, PromptTokens: 5}}
	feeds["old prompt only"] = []core.AgentEvent{{
		At:           end.Add(-time.Hour),
		OutputTokens: 7,
		PromptTokens: 7,
	}}

	prov := core.ProviderSnapshot{
		OutHist:   make([]float64, 4),
		OutStamps: make([]time.Time, 4),
		InHist:    make([]float64, 4),
		InStamps:  make([]time.Time, 4),
	}
	for i := range prov.OutHist {
		prov.OutHist[i] = float64(i)
		prov.OutStamps[i] = end.Add(-time.Duration(3-i) * time.Second)
		prov.InHist[i] = float64(i)
		prov.InStamps[i] = prov.OutStamps[i]
	}

	for name, feed := range feeds {
		for _, out := range []bool{true, false} {
			grid := agentDenseHist(feed, out, end, core.HistoryLen, time.Second)
			nonzero := false
			for _, v := range grid {
				if v > 0 {
					nonzero = true
				}
			}
			s := core.Snapshot{Providers: []core.ProviderSnapshot{prov}, Agents: feed}
			tv := timedSeries(s, out, time.Second)
			got := len(tv) - len(prov.OutHist)
			want := 0
			if nonzero {
				want = len(grid)
			}
			if got != want {
				t.Errorf("%s out=%v: %d grid entries appended, want %d (grid nonzero = %v)",
					name, out, got, want, nonzero)
			}
		}
	}
}
