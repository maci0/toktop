package ui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// Agent names arrive from the ingest endpoint (attacker-shaped: any local
// process able to reach the endpoint) and from agent definitions on disk.
// The agents-only view and the panel summary must pass them through the same
// terminal sanitizer feedLine uses, so an escape payload in a name can never
// reach the raw terminal.
func TestAgentSummarySanitizesNames(t *testing.T) {
	rates := []core.AgentRate{
		{Agent: "claude\x1b]52;c;QUJD\x07", TokPS: 12, Last: time.Now()},
		{Agent: "codex\x1b[2J", Tokens: 300, Last: time.Now()},
	}
	out := strip(agentSummary(rates))
	for _, b := range []byte{'\x1b', '\x07'} {
		if strings.ContainsRune(out, rune(b)) {
			t.Errorf("agentSummary output contains control byte %#x:\n%s", b, out)
		}
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, "codex") {
		t.Errorf("agentSummary lost visible names:\n%s", out)
	}
}

func TestRenderAgentsOnlySanitizesNames(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Agents: []core.AgentEvent{{
		At:           time.Now(),
		Agent:        "evil\x1b]0;pwned\x07agent",
		OutputTokens: 42,
	}}}
	m.w, m.h, m.ready = 100, 40, true
	m.clock = time.Now()
	out := strip(m.renderAgentsOnly())
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("agents-only view leaked escape bytes:\n%s", out)
	}
	if !strings.Contains(out, "evil") {
		t.Errorf("agents-only view lost the visible name:\n%s", out)
	}
}

// The rate column must start at the same cell in every row. Padding by rune
// or byte counts (fmt width verbs, len) counts the ANSI bytes of styled
// cells and drifts whenever a name's visible width differs from either.
func TestAgentRowsAlignRateColumn(t *testing.T) {
	now := time.Now()
	ev := func(agent string, at time.Time) core.AgentEvent {
		return core.AgentEvent{At: at, Agent: agent, Kind: "turn", OutputTokens: 10}
	}
	events := []core.AgentEvent{
		ev("a", now.Add(-3*time.Second)), ev("a", now.Add(-2*time.Second)),
		ev("日本語エージェント", now.Add(-3*time.Second)), ev("日本語エージェント", now.Add(-2*time.Second)),
	}
	rows := agentRows(core.AgentRates(events, now), now)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	rateCol := func(row string) int {
		before, _, ok := strings.Cut(row, "▲")
		if !ok {
			t.Fatalf("row lacks a rate cell: %q", strip(row))
		}
		return lipgloss.Width(before)
	}
	if got := rateCol(rows[0]); got != rateCol(rows[1]) {
		t.Errorf("rate columns misaligned: %d vs %d cells\n%s\n%s",
			got, rateCol(rows[1]), strip(rows[0]), strip(rows[1]))
	}
}

// feedLines renders the newest n events oldest-first, so the feed reads like
// a log tail; it must never reorder or drop within the tail.
func TestFeedLinesNewestLast(t *testing.T) {
	base := time.Now()
	var events []core.AgentEvent
	for i := range 6 {
		events = append(events, core.AgentEvent{
			At: base.Add(time.Duration(i) * time.Second), Agent: fmt.Sprint(i), Kind: "turn",
		})
	}
	lines := feedLines(events, 4, 80)
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4", len(lines))
	}
	for i, want := range []string{"2", "3", "4", "5"} {
		if got := strip(lines[i]); !strings.Contains(got, want) {
			t.Errorf("line %d = %q, want oldest-first event %s", i, got, want)
		}
	}
}

// Tokens an agent spends through a monitored engine must not raise the
// header/chart totals: the engine already reports them. Unattributed agents
// (cloud APIs, engines toktop is not watching) still add in.
func TestAggSkipsViaEngineTokens(t *testing.T) {
	now := time.Now()
	ev := func(agent string, dt time.Duration, out, prompt int64, via string) core.AgentEvent {
		return core.AgentEvent{
			At: now.Add(dt), Agent: agent, Kind: "turn",
			OutputTokens: out, PromptTokens: prompt, ViaEngine: via,
		}
	}
	s := core.Snapshot{
		Providers: []core.ProviderSnapshot{{OutTokPS: 100, InTokPS: 20}},
		Agents: []core.AgentEvent{
			ev("claude", -2*time.Second, 50, 80, "127.0.0.1:11434"),
			ev("claude", -time.Second, 50, 80, "127.0.0.1:11434"),
			ev("codex", -2*time.Second, 40, 10, ""),
			ev("codex", -time.Second, 40, 10, ""),
		},
	}
	if got := aggOutAt(s, now); got != 180 {
		t.Errorf("aggOut = %v, want 180 (engine 100 + codex 80, claude skipped)", got)
	}
	if got := aggInAt(s, now); got != 40 {
		t.Errorf("aggIn = %v, want 40 (engine 20 + codex 20, claude skipped)", got)
	}
}

// An agent that switches onto a monitored engine mid-window still
// contributes the tokens it spent before that. Last-ViaEngine-wins would
// drop the unattributed slice from the header the moment the last event
// is labelled via.
func TestAggCountsOwnTokensWhenAgentSwitchesOntoAnEngine(t *testing.T) {
	now := time.Now()
	s := core.Snapshot{
		Agents: []core.AgentEvent{
			{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn",
				OutputTokens: 40, PromptTokens: 10},
			{At: now.Add(-time.Second), Agent: "claude", Kind: "turn",
				OutputTokens: 40, PromptTokens: 10},
			{At: now.Add(-500 * time.Millisecond), Agent: "claude", Kind: "turn",
				OutputTokens: 100, PromptTokens: 80, ViaEngine: "127.0.0.1:11434"},
		},
	}
	if got := aggOutAt(s, now); got != 80 {
		t.Errorf("aggOut = %v, want 80 (own 80 tok/s, via event skipped)", got)
	}
	if got := aggInAt(s, now); got != 20 {
		t.Errorf("aggIn = %v, want 20 (own 20 tok/s, via event skipped)", got)
	}
}

func TestAggAgentsOnlyUsesAgentRates(t *testing.T) {
	now := time.Now()
	s := core.Snapshot{Agents: []core.AgentEvent{
		{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn", OutputTokens: 30, PromptTokens: 90},
		{At: now.Add(-time.Second), Agent: "claude", Kind: "turn", OutputTokens: 30, PromptTokens: 90},
	}}
	if got := aggOutAt(s, now); got != 60 {
		t.Errorf("aggOut = %v, want 60", got)
	}
	if got := aggInAt(s, now); got != 180 {
		t.Errorf("aggIn = %v, want 180", got)
	}
}

func TestAgentRowsShowPromptThinkingAndVia(t *testing.T) {
	now := time.Now()
	rates := []core.AgentRate{
		{Agent: "claude", TokPS: 12, Tokens: 2400, Prompt: 8100, Thinking: 400, Last: now},
		{Agent: "codex", Tokens: 18000, Prompt: 40000, Last: now, ViaEngine: "127.0.0.1:11434"},
	}
	rows := agentRows(rates, now)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	claude := strip(rows[0])
	for _, want := range []string{"claude", "tok/s", "▲2.4k", "▼8.1k", "400 think", "live"} {
		if !strings.Contains(claude, want) {
			t.Errorf("claude row missing %q:\n%s", want, claude)
		}
	}
	codex := strip(rows[1])
	for _, want := range []string{"codex", "via 127.0.0.1:11434", "▲18.0k", "▼40.0k"} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex row missing %q:\n%s", want, codex)
		}
	}
}

func TestAgentRowsSanitizeViaEngine(t *testing.T) {
	now := time.Now()
	rows := agentRows([]core.AgentRate{{
		Agent: "claude", Last: now, ViaEngine: "127.0.0.1:11434\x1b]52;c;QUJD\x07",
	}}, now)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if strings.ContainsAny(rows[0], "\x1b\x07") {
		t.Errorf("via-engine leaked escape bytes:\n%s", rows[0])
	}
	if !strings.Contains(strip(rows[0]), "127.0.0.1:11434") {
		t.Errorf("via-engine lost the address:\n%s", strip(rows[0]))
	}
}

func TestRenderAgentsOnlyShowsDashboardChrome(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		Agents: []core.AgentEvent{
			{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn",
				OutputTokens: 40, PromptTokens: 100},
			{At: now.Add(-time.Second), Agent: "claude", Kind: "turn",
				OutputTokens: 40, PromptTokens: 100},
		},
		Sys: &core.SysSample{MemTotal: 32 << 30, MemUsed: 16 << 30, Load1: 1.5},
	}
	out := strip(StaticFrame(Config{Version: "t"}, snap, 110, 36))
	for _, want := range []string{"1 agent", "tok/s", "THROUGHPUT", "output", "SYS", "AGENTS", "claude", "AGENT FEED"} {
		if !strings.Contains(out, want) {
			t.Errorf("agents-only dashboard missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0/0 engines") {
		t.Errorf("agents-only dashboard still shows the engines-empty header:\n%s", out)
	}
}

// Agents-only used a stripped AGENT FEED that hid ingest death and the POST
// target the full dashboard already shows.
func TestAgentsOnlyFeedMirrorsIngestStatus(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{Agents: []core.AgentEvent{
		{At: now, Agent: "claude", Kind: "turn", OutputTokens: 40},
	}}
	live := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420"}, nil)
	live.snap, live.w, live.h, live.ready, live.clock = snap, 110, 36, true, now
	if out := strip(live.renderAgentsOnly()); !strings.Contains(out, "POST http://127.0.0.1:8420/v1/events") {
		t.Errorf("agents-only live ingest lost the POST target:\n%s", out)
	}

	down := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420"}, nil)
	down.snap, down.w, down.h, down.ready, down.clock = snap, 110, 36, true, now
	down.feedDown = "listener closed"
	out := strip(down.renderAgentsOnly())
	if strings.Contains(out, "POST http://127.0.0.1:8420") {
		t.Errorf("agents-only dead ingest still advertised:\n%s", out)
	}
	if !strings.Contains(out, "ingest down") {
		t.Errorf("agents-only missing ingest-down after feed death:\n%s", out)
	}
}

func TestAgentsOnlyFrameFitsPane(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		Agents: []core.AgentEvent{
			{At: now.Add(-2 * time.Second), Agent: "日本語エージェント", Kind: "turn",
				OutputTokens: 40, PromptTokens: 100, Note: strings.Repeat("x", 80)},
			{At: now.Add(-time.Second), Agent: "日本語エージェント", Kind: "turn",
				OutputTokens: 40, PromptTokens: 100},
		},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			CPUModel: "Test CPU", OsName: "TestOS",
		},
	}
	for _, sz := range [][2]int{{62, 30}, {80, 32}, {110, 36}, {160, 44}} {
		w, h := sz[0], sz[1]
		out := StaticFrame(Config{Version: "t"}, snap, w, h)
		if got := lipgloss.Height(out); got > h {
			t.Errorf("%dx%d: frame is %d lines, overflows pane", w, h, got)
		}
		for i, ln := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(ln); lw > w {
				t.Fatalf("%dx%d: line %d renders %d cells, want <= %d:\n%s",
					w, h, i, lw, w, ln)
			}
		}
	}
}

func TestAgentDenseHistSkipsViaEngine(t *testing.T) {
	end := time.Unix(1_700_000_010, 0)
	events := []core.AgentEvent{
		{At: end.Add(-time.Second), Agent: "claude", OutputTokens: 100, ViaEngine: "127.0.0.1:11434"},
		{At: end.Add(-time.Second), Agent: "codex", OutputTokens: 50},
	}
	got := agentDenseHist(events, true, end, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// middle bucket is end-1s: only the unattributed 50 tokens, as 50 tok/s.
	if got[1] != 50 {
		t.Fatalf("hist = %v, want 50 tok/s from the unattributed event in the middle bucket", got)
	}
	if got[0] != 0 || got[2] != 0 {
		t.Fatalf("hist = %v, want zeros outside the event bucket", got)
	}
}

func TestNearestCadenceIndexFloorsNegatives(t *testing.T) {
	sec := time.Second
	half := 500 * time.Millisecond
	if got := nearestCadenceIndex(-half+time.Millisecond, sec); got != 0 {
		t.Errorf("just inside the first bucket = %d, want 0", got)
	}
	if got := nearestCadenceIndex(-half-time.Millisecond, sec); got != -1 {
		t.Errorf("just outside the first bucket = %d, want -1", got)
	}
	if got := nearestCadenceIndex(-sec-half, sec); got != -1 {
		t.Errorf("-1.5 cadences = %d, want -1 (tie rounds toward later)", got)
	}
	if got := nearestCadenceIndex(half, sec); got != 1 {
		t.Errorf("+0.5 cadences = %d, want 1 (tie rounds toward later)", got)
	}
	if got := nearestCadenceIndex(math.MaxInt64, sec); got != math.MaxInt {
		t.Errorf("max duration = %d, want math.MaxInt", got)
	}
	if got := nearestCadenceIndex(10*sec, 0); got != 0 {
		t.Errorf("zero cadence = %d, want 0", got)
	}
}

func TestAgentDenseHistExcludesEventsBeforeWindow(t *testing.T) {
	end := time.Unix(1_700_000_010, 0)
	// n=3, cadence=1s → start = end-2s. An event 0.6s before start is
	// outside the nearest-bucket window and must not inflate column 0.
	// Toward-zero duration division maps that offset onto index 0.
	events := []core.AgentEvent{
		{At: end.Add(-2*time.Second - 600*time.Millisecond), Agent: "codex", OutputTokens: 999},
		{At: end.Add(-time.Second), Agent: "codex", OutputTokens: 50},
	}
	got := agentDenseHist(events, true, end, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != 0 {
		t.Fatalf("hist = %v, event 0.6s before the window leaked into column 0", got)
	}
	if got[1] != 50 {
		t.Fatalf("hist = %v, want 50 tok/s in the middle bucket", got)
	}
}

func TestAgentDenseHistRoundsHalfCadenceBeforeStartIntoColumnZero(t *testing.T) {
	end := time.Unix(1_700_000_010, 0)
	events := []core.AgentEvent{
		{At: end.Add(-2*time.Second - 400*time.Millisecond), Agent: "codex", OutputTokens: 80},
	}
	got := agentDenseHist(events, true, end, 3, time.Second)
	if got[0] != 80 {
		t.Fatalf("hist = %v, want 80 tok/s in column 0 (0.4s before start is nearer to start)", got)
	}
}

// 'a' swaps which side gets the panel estate: the engines keep the header,
// the shared throughput chart and the host strip, and the agents get the tall
// panel; pressing it again hands the panels back.
func TestAgentsFocusToggleSwapsEstate(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", Kind: core.KindOllama, OK: true,
			OutTokPS: 42, OutT0: now, OutHist: []float64{1, 2, 3}, Models: []core.ModelInfo{{Name: "llama3"}}}},
		Agents: []core.AgentEvent{
			{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn", OutputTokens: 40},
			{At: now.Add(-time.Second), Agent: "claude", Kind: "turn", OutputTokens: 40},
		},
		Sys: &core.SysSample{MemTotal: 32 << 30, MemUsed: 16 << 30, Load1: 1.5},
	}
	m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420", Agents: true}, nil)
	m.snap, m.w, m.h, m.ready, m.clock = snap, 120, 40, true, now

	engines := strip(m.View())
	for _, want := range []string{"ENGINES", "ENGINE STATE", "AGENT FEED", "a agents"} {
		if !strings.Contains(engines, want) {
			t.Errorf("engines focus missing %q:\n%s", want, engines)
		}
	}

	nm, _ := m.Update(keyMsg("a"))
	m = nm.(Model)
	agents := strip(m.View())
	for _, want := range []string{"AGENTS", "THROUGHPUT", "SYS", "claude", "a engines"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents focus missing %q:\n%s", want, agents)
		}
	}
	for _, gone := range []string{"ENGINE STATE", "PROBES"} {
		if strings.Contains(agents, gone) {
			t.Errorf("agents focus still draws the %s panel:\n%s", gone, agents)
		}
	}

	nm, _ = m.Update(keyMsg("a"))
	m = nm.(Model)
	if back := strip(m.View()); !strings.Contains(back, "ENGINE STATE") {
		t.Errorf("second press did not restore the engines panels:\n%s", back)
	}
}

// A pane too small for the dashboard keeps the compact strip, which must not
// lead with missing engines on a run that asked for agents.
func TestMinimalAgentsViewDropsEngineComplaint(t *testing.T) {
	m := New(Config{Version: "t", Agents: true}, nil)
	m.w, m.h, m.ready = 44, 12, true
	out := strip(m.View())
	if !strings.Contains(out, "watching local agents") {
		t.Errorf("minimal --agents view lost the wait hint:\n%s", out)
	}
	if strings.Contains(out, "no inference engines detected") {
		t.Errorf("minimal --agents view still complains about engines:\n%s", out)
	}
}

// The agents focus draws the agents-only layout while engine data is present,
// at the scale the engine dashboard runs at: it must still fit the pane.
func TestAgentsFocusFrameFitsPane(t *testing.T) {
	snap := perfSnap()
	for _, sz := range [][2]int{{62, 30}, {80, 32}, {120, 40}, {200, 50}} {
		w, h := sz[0], sz[1]
		m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420", Agents: true}, nil)
		m.snap, m.w, m.h, m.ready, m.clock = snap, w, h, true, snap.At
		nm, _ := m.Update(keyMsg("a"))
		m = nm.(Model)
		out := m.View()
		if got := lipgloss.Height(out); got > h {
			t.Errorf("%dx%d: agents focus is %d lines, overflows pane", w, h, got)
		}
		for i, ln := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(ln); lw > w {
				t.Fatalf("%dx%d: line %d renders %d cells, want <= %d:\n%s",
					w, h, i, lw, w, ln)
			}
		}
	}
}
