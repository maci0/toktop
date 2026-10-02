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

// TestTimedSeriesReservesOnlyWhenTheGridFills pins the reservation rule
// timedSeriesEnd sizes its slice against: room for the dense agent grid is
// held only when the feed holds an event this direction counts that did not
// go through an engine, because that is the only condition under which the
// grid's columns are appended. A run watching engines alone reserves no grid,
// so its series slice is exactly the engine samples.
func TestTimedSeriesReservesOnlyWhenTheGridFills(t *testing.T) {
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

	// Engines alone: neither direction reserves a grid.
	tv, e := timedSeriesEnd(s, true, time.Second)
	if e != end {
		t.Errorf("end = %v, want the newest engine stamp %v", e, end)
	}
	if got, want := cap(tv), len(prov.OutHist); got != want {
		t.Errorf("engines-only series cap = %d, want %d: the agent grid was reserved for a run that never fills it", got, want)
	}
	if got, want := len(tv), len(prov.OutHist); got != want {
		t.Errorf("engines-only series len = %d, want %d", got, want)
	}

	// A feed with unattributed tokens fills the grid, and both directions
	// reserve for it.
	withAgents := s
	withAgents.Agents = agentEvents(end, 2)
	for _, out := range []bool{true, false} {
		tv, e := timedSeriesEnd(withAgents, out, time.Second)
		if e != end {
			t.Errorf("out=%v: end = %v, want %v", out, e, end)
		}
		if got, want := len(tv), len(prov.OutHist)+core.HistoryLen; got != want {
			t.Errorf("out=%v: series len = %d, want %d", out, got, want)
		}
		if cap(tv) < len(tv) {
			t.Errorf("out=%v: series cap %d is below its length %d", out, cap(tv), len(tv))
		}
	}

	// Every event through an engine: no reservation, in either direction.
	viaEngine := s
	viaEngine.Agents = agentEvents(end, 2)
	for i := range viaEngine.Agents {
		viaEngine.Agents[i].ViaEngine = "ollama"
	}
	for _, out := range []bool{true, false} {
		if tv, _ := timedSeriesEnd(viaEngine, out, time.Second); cap(tv) != len(prov.OutHist) {
			t.Errorf("out=%v: engine-attributed feed reserved cap %d, want %d", out, cap(tv), len(prov.OutHist))
		}
	}
}

// TestHasAgentTokensAgreesWithDenseHist pins the reservation helper to the
// filter it stands in for: hasAgentTokens reports true exactly when
// agentDenseHist returns a grid with a nonzero column.
func TestHasAgentTokensAgreesWithDenseHist(t *testing.T) {
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

	for name, feed := range feeds {
		for _, out := range []bool{true, false} {
			grid := agentDenseHist(feed, out, end, core.HistoryLen, time.Second)
			nonzero := false
			for _, v := range grid {
				if v > 0 {
					nonzero = true
				}
			}
			if got := hasAgentTokens(feed, out, end, core.HistoryLen, time.Second); got != nonzero {
				t.Errorf("%s out=%v: hasAgentTokens = %v, grid nonzero = %v", name, out, got, nonzero)
			}
		}
	}
}