package ui

// The agent event feed and its title.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

func (m Model) renderFeed() string {
	w := m.w - 4
	_, _, feedIn := m.sectionHeights()
	now := m.snapNow()
	rates := m.agentRates()
	rows := agentRows(rates, now)
	statsN := 0
	if len(rows) > 0 && feedIn > 0 {
		statsN = min(len(rows), max(feedIn/2, 1))
		if statsN >= feedIn && feedIn > 1 {
			statsN = feedIn - 1
		}
	}
	var lines []string
	if statsN > 0 {
		lines = append(lines, rows[:statsN]...)
	}
	rest := feedIn - len(lines)
	lines = append(lines, feedLines(m.snap.Agents, rest, w)...)
	if len(lines) == 0 {
		lines = append(lines, m.feedEmptyLines(w)...)
	}
	content := strings.Join(lines, "\n")
	return panel(m.feedTitle(w, statsN, len(rows), rates), content, w, feedIn)
}

// feedTitle is the AGENT FEED heading. A panel title is not clipped by the
// panel, and JoinVertical pads every other block out to the widest one, so
// an over-wide title here silently stretches the whole frame past the pane.
// Optional parts are therefore added only while they fit, most useful first.
func (m Model) feedTitle(w, statsN, nRows int, rates []core.AgentRate) string {
	title := "AGENT FEED"
	add := func(part string) {
		if lipgloss.Width(title)+lipgloss.Width(part) <= w {
			title += part
		}
	}
	if m.paused {
		add("  " + styleWarn.Render("(paused)"))
	}
	if m.feedDown != "" {
		// Not "ingest down": the feed channel carries whatever took the agent
		// event stream down, and a bad engine address reaches it beside a
		// perfectly healthy ingest endpoint. The badge names the condition,
		// not the subsystem the message itself identifies.
		add("  " + styleBad.Render("✗ feed error"))
	}
	if statsN == 0 {
		if s := agentSummary(rates); s != "" {
			add("  " + s)
		}
	} else if statsN < nRows {
		add("  " + dim(fmt.Sprintf("+%d more", nRows-statsN)))
	}
	if m.feedDown == "" && m.cfg.IngestAddr != "" {
		add(dim("  ← POST http://" + m.cfg.IngestAddr + "/v1/events"))
	}
	return title
}

// feedEmptyLines is the empty AGENT FEED body: which knob fills this panel
// is invisible from a bare "empty", and a dead feed's reason is otherwise
// only on stderr, hidden under the alternate screen.
//
// The reason is rendered as it arrives. Every producer names its own
// subsystem (main sends "ingest stopped: …" and "agent watch: …"), so a
// prefix added here would name the ingest endpoint for a failure that has
// nothing to do with it, and send the operator to restart a dashboard whose
// endpoint is answering.
func (m Model) feedEmptyLines(w int) []string {
	switch {
	case m.feedDown != "":
		reason := clip(shorten(core.SanitizeText(m.feedDown), w), w)
		return []string{styleBad.Render(reason)}
	case m.cfg.Agents:
		return []string{dim("no agent activity yet: agents running locally are picked up automatically")}
	case m.cfg.IngestAddr != "":
		return []string{dim("no agent activity yet: point your harness at the endpoint above")}
	default:
		return []string{dim("no agent activity yet: run with --agents to watch coding agents on this machine")}
	}
}

var kindIcons = map[string]string{
	core.AgentKindTurn:  "▸",
	core.AgentKindTool:  "⚙",
	core.AgentKindError: "✗",
	core.AgentKindNote:  "✎",
}

// feedLines renders the newest n events oldest-first, so a feed panel reads
// top down like a log tail with the newest line at the bottom.
func feedLines(events []core.AgentEvent, n, w int) []string {
	if n <= 0 || len(events) == 0 {
		return nil
	}
	out := make([]string, 0, min(n, len(events)))
	for _, ev := range events[max(len(events)-n, 0):] {
		out = append(out, clip(feedLine(ev), w))
	}
	return out
}

func feedLine(ev core.AgentEvent) string {
	icon := kindIcons[ev.Kind]
	if icon == "" {
		icon = "·"
	}
	st := styleDim
	switch ev.Kind {
	case core.AgentKindTurn:
		st = styleOK
	case core.AgentKindTool:
		st = styleInfo
	case core.AgentKindError:
		st = styleBad
	case core.AgentKindNote:
		st = styleWarn
	}
	name := shorten(core.SingleLine(ev.Agent), 16)
	// ▲ output / ▼ prompt, same directions as the header rates. Output
	// first so a feed row scans like the header: out, then in.
	tok := fmt.Sprintf("▲%s ▼%s", fmtCount(ev.OutputTokens), fmtCount(ev.PromptTokens))
	if ev.ThinkingTokens > 0 {
		tok += " think " + fmtCount(ev.ThinkingTokens)
	}
	parts := []string{
		// Event timestamps come from external senders and may carry any
		// zone (or none, which decodes as UTC); render the viewer's clock.
		styleDim.Render(ev.At.Local().Format("15:04:05")),
		st.Render(icon + " " + name),
		styleDim.Render(shorten(core.SingleLine(ev.Model), 20)),
		tok,
	}
	if ev.ViaEngine != "" {
		parts = append(parts, dim("via "+shorten(core.SingleLine(ev.ViaEngine), 18)))
	}
	if ev.Note != "" {
		parts = append(parts, styleWarn.Render(shorten(core.SingleLine(ev.Note), 28)))
	}
	return strings.Join(parts, "  ")
}
