package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// agentSummary renders the rates as one line, for a panel title.
func agentSummary(rates []core.AgentRate) string {
	if len(rates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rates))
	for i, r := range rates {
		if i == 3 {
			parts = append(parts, dim(fmt.Sprintf("+%d more", len(rates)-3)))
			break
		}
		// Agent names arrive from the ingest endpoint and agent definitions
		// on disk; they pass the terminal sanitizer like every other
		// untrusted field (feedLine does the same at render time).
		name := core.SanitizeText(r.Agent)
		cell := styleValue.Render(name)
		switch {
		case r.ViaEngine != "":
			cell += " " + dim("via "+shorten(core.SanitizeText(r.ViaEngine), 16))
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

// agentRows lays out one row per agent: name, rate, tokens, recency. Cells
// are padded by visible cells (padTo/padStart), never %-Ns width verbs:
// styled cells carry ANSI bytes whose rune counts would skew the columns.
func agentRows(rates []core.AgentRate, now time.Time) []string {
	names := make([]string, len(rates))
	nameW := 10
	for i, r := range rates {
		names[i] = core.SanitizeText(r.Agent)
		nameW = max(nameW, lipgloss.Width(names[i]))
	}
	out := make([]string, 0, len(rates))
	for i, r := range rates {
		rate := dim("no rate yet")
		if r.TokPS > 0 {
			rate = styleValue.Foreground(heatColor(clamp01(r.TokPS / 60))).
				Render("▲ " + fmtRate(r.TokPS) + " tok/s")
		} else if r.ViaEngine != "" {
			rate = dim("via engine")
		}
		tok := dim("▲" + fmtCount(r.Tokens))
		if r.Prompt > 0 {
			tok += dim(" ▼" + fmtCount(r.Prompt))
		}
		if r.Thinking > 0 {
			tok += dim(" " + fmtCount(r.Thinking) + " think")
		}
		since := dim("idle " + fmtDur(now.Sub(r.Last).Truncate(time.Second)))
		if now.Sub(r.Last) < 3*time.Second {
			since = styleOK.Render("● live")
		}
		if r.ViaEngine != "" {
			since = dim("via "+shorten(core.SanitizeText(r.ViaEngine), 18)) + "  " + since
		}
		out = append(out, "  "+padTo(styleValue.Render(names[i]), nameW)+
			"  "+padTo(rate, 22)+"  "+padStart(tok, 18)+"  "+since)
	}
	return out
}

// agentMiniLine is the compact-strip counterpart of one agentRows cell.
func agentMiniLine(r core.AgentRate) string {
	name := core.SanitizeText(r.Agent)
	line := styleValue.Render(name) + " "
	switch {
	case r.TokPS > 0:
		line += fmtRate(r.TokPS) + " tok/s"
	default:
		line += dim(fmtCount(r.Tokens) + " tok")
	}
	if r.ViaEngine != "" {
		line += " " + dim("via "+shorten(core.SanitizeText(r.ViaEngine), 16))
	}
	return line
}

// renderAgentsOnly is the view for a machine with agents but no engines: the
// same dashboard chrome (header, throughput, host strip) with the agents
// themselves in place of the backend panels. It is a real dashboard, not a
// placeholder, because for someone driving claude or codex all day this is
// the whole picture.
func (m Model) renderAgentsOnly() string {
	w := m.w - 4
	now := m.snapNow()
	rates := core.AgentRates(m.snap.Agents, now)
	_, midIn, feedIn := m.sectionHeights()

	rows := agentRows(rates, now)
	if len(rows) == 0 {
		rows = append(rows, dim("  waiting for an agent to report tokens…"))
	}

	title := "AGENTS"
	if hint := dim("  local, read from their own session logs"); m.w >= 78 {
		title += hint
	}

	feed := feedLines(m.snap.Agents, feedIn, w)
	if len(feed) == 0 {
		feed = append(feed, m.feedEmptyLines(w)...)
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
	shifted := d + cadence/2
	q := shifted / cadence
	if shifted%cadence < 0 {
		q--
	}
	return int(q)
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
