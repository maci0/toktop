package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// maxSummaryAgents is how many agents fit on the panel-title summary line
// before the rest collapse into a count.
const maxSummaryAgents = 3

// Frame widths below which each AGENTS title hint is dropped, because adding
// it would stretch the title past the pane. The local hint names the session
// logs and is the longer of the two, so it needs the wider frame.
const (
	localHintMinW  = 78
	ingestHintMinW = 56
)

// agentLiveWindow is how recently an agent must have reported for its recency
// cell to read "live" rather than an idle span. Below it the idle string would
// render as "idle 0s", which reads as stalled rather than current.
const agentLiveWindow = 3 * time.Second

// agentIdle is how long ago an agent last reported, and how that recency
// should read: recencyLive inside agentLiveWindow, recencyIdle past it, and
// recencyUnknown when the frame cannot say.
//
// A zero on either side is unknown: a snapshot with no At, which --once can
// receive from a caller that did not stamp one, measures every agent against
// a span of billions of years, negative, and every threshold comparison passes,
// so the whole feed would read live for a frame that knows nothing about the
// present. A last stamped in the future is a sender whose clock runs ahead, not
// an agent that just reported, so it is idle at zero rather than live.
func agentIdle(now, last time.Time) (time.Duration, recency) {
	if now.IsZero() || last.IsZero() {
		return 0, recencyUnknown
	}
	if d := now.Sub(last); d < 0 {
		return 0, recencyIdle
	} else if d < agentLiveWindow {
		return d, recencyLive
	}
	return now.Sub(last), recencyIdle
}

// How an agent's recency cell reads. The three states are distinct because
// "recent" and "not recent" are claims about a timeline, and having no timeline
// is a third thing that must not be reported as either.
type recency uint8

const (
	recencyUnknown recency = iota
	recencyLive
	recencyIdle
)

// agentCountLabel spells an agent count, so the header, the compact strip and
// the linear report cannot disagree about "1 agent" against "1 agents".
func agentCountLabel(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

// agentSummary renders the rates as one line, for a panel title.
func agentSummary(rates []core.AgentRate) string {
	if len(rates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rates))
	for i, r := range rates {
		if i == maxSummaryAgents {
			parts = append(parts, dim(fmt.Sprintf("+%d more", len(rates)-maxSummaryAgents)))
			break
		}
		// Agent names arrive from the ingest endpoint and agent definitions
		// on disk; they pass the terminal sanitizer like every other
		// untrusted field (feedLine does the same at render time).
		name := core.SingleLine(r.Agent)
		cell := styleValue.Render(name)
		switch {
		case r.ViaEngine != "":
			cell += " " + dim("via "+shorten(core.SingleLine(r.ViaEngine), 16))
		case r.TokPS > 0:
			cell += " " + styleOK.Render(fmtRate(r.TokPS)) + dim(" tok/s")
		default:
			// Measured tokens, but not yet a rate: say so rather than showing 0.
			cell += " " + dim(fmtCount(r.Tokens)+" tok")
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, dim("  ·  "))
}

// Column floors for the agent table: the narrowest a column is worth keeping so
// a short row still reads as a table. The columns grow to whatever the rows
// actually hold, so the recency cell at the right is not the one a narrow pane
// pays for: fixed widths spent 22 cells on a rate that is 11 wide, and the row
// was cut before the "● live" the reader is scanning for.
const (
	agentNameMin   = 10
	agentRateMin   = 12
	agentTokensMin = 8
)

// agentRows lays out one row per agent: name, rate, tokens, recency. Cells
// are padded by visible cells (padTo/padStart), never %-Ns width verbs:
// styled cells carry ANSI bytes whose rune counts would skew the columns.
func agentRows(rates []core.AgentRate, now time.Time) []string {
	names := make([]string, len(rates))
	rateCells := make([]string, len(rates))
	tokCells := make([]string, len(rates))
	sinceCells := make([]string, len(rates))
	nameW, rateW, tokW := agentNameMin, agentRateMin, agentTokensMin
	for i, r := range rates {
		names[i] = core.SingleLine(r.Agent)
		// Engine-routed tokens are already counted by the engine, so the row
		// names the engine where the rate would go and does not also print it
		// again beside the recency cell. agentSummary, agentMiniLine, feedLine
		// and the plain report all name it once; this row used to print a vague
		// "via engine" here and the address over there.
		rate := dim("no rate yet")
		switch {
		case r.ViaEngine != "":
			rate = dim("via " + shorten(core.SingleLine(r.ViaEngine), 18))
		case r.TokPS > 0:
			rate = styleValue.Foreground(heatColor(clamp01(r.TokPS / 60))).
				Render("▲ " + fmtRate(r.TokPS) + " tok/s")
		}
		tok := dim("▲" + fmtCount(r.Tokens))
		if r.Prompt > 0 {
			tok += dim(" ▼" + fmtCount(r.Prompt))
		}
		if r.Thinking > 0 {
			tok += dim(" " + fmtCount(r.Thinking) + " think")
		}
		var since string
		switch d, how := agentIdle(now, r.Last); how {
		case recencyLive:
			since = styleOK.Render("● live")
		case recencyIdle:
			since = dim("idle " + fmtDur(d))
		}
		rateCells[i], tokCells[i], sinceCells[i] = rate, tok, since
		nameW = max(nameW, lipgloss.Width(names[i]))
		rateW = max(rateW, lipgloss.Width(rate))
		tokW = max(tokW, lipgloss.Width(tok))
	}
	out := make([]string, 0, len(rates))
	for i := range rates {
		out = append(out, "  "+padTo(styleValue.Render(names[i]), nameW)+
			"  "+padTo(rateCells[i], rateW)+"  "+padStart(tokCells[i], tokW)+"  "+sinceCells[i])
	}
	return out
}

// agentMiniLine is the compact-strip counterpart of one agentRows cell.
func agentMiniLine(r core.AgentRate) string {
	name := core.SingleLine(r.Agent)
	line := styleValue.Render(name) + " "
	switch {
	case r.TokPS > 0:
		line += fmtRate(r.TokPS) + " tok/s"
	default:
		line += dim(fmtCount(r.Tokens) + " tok")
	}
	if r.ViaEngine != "" {
		line += " " + dim("via "+shorten(core.SingleLine(r.ViaEngine), 16))
	}
	return line
}

// renderAgentsOnly is the agents view: a machine with agents but no engines
// gets it automatically, and `a` reaches it with engines present. The same
// dashboard chrome (header, throughput, host strip) with the agents
// themselves in place of the backend panels. It is a real dashboard, not a
// placeholder, because for someone driving claude or codex all day this is
// the whole picture.
func (m Model) renderAgentsOnly() string {
	w := m.w - 4
	now := m.snapNow()
	rates := m.agentRates()
	_, midIn, feedIn := m.sectionHeights()

	rows := agentRows(rates, now)
	if len(rows) == 0 {
		rows = append(rows, dim("  waiting for an agent to report tokens…"))
	}

	// Optional parts join only while they fit: an over-wide title stretches
	// the whole frame past the pane (same rule as feedTitle).
	title := "AGENTS"
	add := func(part string) {
		if lipgloss.Width(title)+lipgloss.Width(part) <= w {
			title += part
		}
	}
	// Where the tokens come from, and only where that is true: the same view
	// is fed by the local session-log watch under --agents and by harness
	// events POSTed to the ingest endpoint without it, and the session-log
	// claim was wrong for the second. Each hint names the frame width below
	// which it would push the title past the pane, since the hint is longer
	// than the other by design.
	if m.cfg.Agents {
		if hint := dim("  local, read from their own session logs"); m.w >= localHintMinW {
			add(hint)
		}
	} else if m.cfg.IngestAddr != "" {
		if hint := dim("  from the ingest endpoint"); m.w >= ingestHintMinW {
			add(hint)
		}
	}
	// This view has no PROBES panel, so the title is where a probe lands: the
	// badge alone left p with no outcome anywhere on screen.
	if r := m.probeReadout(); r != "" {
		add("  " + r)
	}
	if len(rows) > midIn && midIn > 0 {
		add("  " + dim(fmt.Sprintf("+%d more", len(rows)-midIn)))
	}

	feed := feedLines(m.snap.Agents, feedIn, w)
	if len(feed) == 0 {
		feed = append(feed, m.feedEmptyLines(w)...)
		if m.feedDown != "" {
			// The line above says why; this is the way out, and the agents
			// view has no setup card to carry it. It names no subsystem: the
			// remedy differs by cause, and the message above names the cause.
			feed = append(feed, dim("fix the cause named above, then q and restart toktop"))
		}
	}

	body := lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		"",
		m.renderCharts(),
		m.renderSystem(),
		panel(title, strings.Join(rows, "\n"), w, midIn),
		panel(m.feedTitle(w, 0, 0, nil), strings.Join(feed, "\n"), w, feedIn),
	)
	return composeFrame(body, m.renderFooter(), m.w, m.h)
}

// agentDenseHist buckets unattributed agent tokens onto a uniform cadence
// grid ending at `end`: each column is tok/s for that interval. Events marked
// ViaEngine are skipped; the engine's own history already carries them.
func agentDenseHist(events []core.AgentEvent, out bool, end time.Time, n int, cadence time.Duration) []float64 {
	if n <= 0 || cadence <= 0 || end.IsZero() {
		return nil
	}
	grid := make([]float64, n)
	start := end.Add(-time.Duration(n-1) * cadence)
	sec := cadence.Seconds()
	for _, ev := range events {
		if ev.ViaEngine != "" {
			continue
		}
		tok := ev.OutputTokens
		if !out {
			tok = ev.PromptTokens
		}
		if tok <= 0 {
			continue
		}
		idx := nearestCadenceIndex(ev.At.Sub(start), cadence)
		if idx < 0 || idx >= n {
			continue
		}
		grid[idx] += float64(tok) / sec
	}
	return grid
}

// nearestCadenceIndex is the cadence-spaced slot nearest to offset d from the
// series origin. Go divides durations toward zero, so a negative remainder
// would otherwise land on the next slot up: an event 0.6s before the window
// start at 1s cadence would sit in column 0 instead of being out of range.
func nearestCadenceIndex(d, cadence time.Duration) int {
	if cadence <= 0 {
		return 0
	}
	half := cadence / 2
	if d > math.MaxInt64-half {
		return math.MaxInt
	}
	shifted := d + half
	q := shifted / cadence
	if shifted%cadence < 0 {
		q--
	}
	// q is a Duration, so the guard above already kept it inside the int range
	// a 64-bit int holds. Every release platform is 64-bit (see PLATFORMS in
	// the Makefile); a 32-bit build would need its own clamp here.
	return int(q)
}

// cadenceSpan is the range of cadence-spaced columns a sample at offset d from
// the series origin reaches: the first and the last column whose centre lies
// within half a cadence of it. Centres are a full cadence apart and the reach
// is half one either side, so the range spans at most two columns, and a
// sample exactly on a boundary is counted by both of them, as a scan of every
// column would have counted it.
//
// An offset no column can reach yields an empty range (first > last).
func cadenceSpan(d, cadence time.Duration) (first, last int) {
	if cadence <= 0 {
		return 0, -1
	}
	half := cadence / 2
	if d > math.MaxInt64-half || d < math.MinInt64+half {
		return math.MaxInt, math.MinInt
	}
	return int(ceilDivDuration(d-half, cadence)), int(floorDivDuration(d+half, cadence))
}

// ceilDivDuration divides rounding toward positive infinity. Go's integer
// division rounds toward zero, which is the floor of a positive quotient and
// the ceiling of a negative one, so only the positive side needs a nudge.
func ceilDivDuration(a, b time.Duration) time.Duration {
	q := a / b
	if a%b > 0 {
		q++
	}
	return q
}

// floorDivDuration divides rounding toward negative infinity. Go's integer
// division rounds toward zero, which would land a negative offset on the
// column above the one it belongs to.
func floorDivDuration(a, b time.Duration) time.Duration {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

func agentHistEnd(events []core.AgentEvent) time.Time {
	var end time.Time
	for _, ev := range events {
		if ev.ViaEngine != "" {
			continue
		}
		if ev.At.After(end) {
			end = ev.At
		}
	}
	return end
}
