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
		// Half the feed, at least one row, so the stats never take the last
		// line the feed has.
		statsN = min(len(rows), max(feedIn/2, 1))
	}
	var lines []string
	// The title badge names the condition; this names the subsystem to fix,
	// and it is otherwise only on stderr, hidden under the alternate screen.
	// The empty feed and the agents view both print it (feedEmptyLines), and
	// a fleet frame whose feed is still producing rows printed the badge with
	// no reason anywhere on screen.
	if m.feedDown != "" {
		// SingleLine, as feedEmptyLines renders it: the reason arrives as its
		// producer's error text, and a newline left in it is a row the
		// dashboard reads as its own output.
		lines = append(lines, styleBad.Render(clip(shorten(core.SingleLine(m.feedDown), w), w)))
	}
	// The stats take what the reason left and never the last row, so the feed
	// below them keeps at least one line. statsN is the number actually drawn:
	// the title counts against it, and a count it was not given is a "+N more"
	// that names rows nobody can see.
	if statsN > 0 {
		if room := feedIn - len(lines); room > 0 {
			statsN = min(statsN, room)
			lines = append(lines, rows[:statsN]...)
		} else {
			statsN = 0
		}
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
		add(moreMarker(title, w, nRows-statsN))
	}
	if m.feedDown == "" && m.cfg.IngestAddr != "" {
		// Sanitized like every other render of this address (renderEmpty,
		// PlainTextFrame): a control character in a panel title is a row the
		// frame reads as its own output, and the title is not width-clipped
		// by panel(), so it also stretches every row below it.
		add(dim("  ← POST http://" + core.SanitizeText(m.cfg.IngestAddr) + "/v1/events"))
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
	if m.feedDown != "" {
		// SingleLine, not SanitizeText: the reason arrives as its producer's
		// error text, and a newline left in it becomes a row of the panel that
		// the dashboard reads as its own output.
		reason := clip(shorten(core.SingleLine(m.feedDown), w), w)
		return []string{styleBad.Render(reason)}
	}
	// The panel title on the line above already carries the endpoint, so this
	// clause points at it rather than repeating the address.
	where := ""
	if m.cfg.IngestAddr != "" {
		where = "point your harness at the endpoint above"
	}
	// A form the column can show whole. The sentence names what to do in its
	// second clause, and clipping it there left the reader with "…are picked"
	// and no instruction, which is the one half of the sentence they needed.
	// The first form that fits wins; the longest is the fallback, clipped as
	// every panel line is when nothing fits.
	forms := feedEmptyForms(m.cfg, where)
	for _, f := range forms {
		if widthOf(f) <= w {
			return []string{dim(f)}
		}
	}
	return []string{dim(forms[0])}
}

// feedEmptyHint is the one sentence an empty agent feed carries. The dashboard
// panel and the linear report answer the same question about the same empty
// panel, and both used to spell it separately: the punctuation drifted, the
// wording drifted, and a reader who had seen both had no reason to expect the
// same advice in two shapes.
//
// It is the first of feedEmptyForms, the spelling the plain report takes: that
// report has no width to fit in.
func feedEmptyHint(cfg Config, where string) string {
	return feedEmptyForms(cfg, where)[0]
}

// feedEmptyForms is the empty agent feed's advice in every spelling a panel
// column can show, longest first. The short forms are not a rewording: they
// carry the same instruction, so a column too narrow for the sentence still
// ends on something the reader can act on rather than on half of one.
func feedEmptyForms(cfg Config, where string) []string {
	switch {
	case cfg.Agents:
		return []string{
			"no agent activity yet: agents running locally are picked up automatically",
			"no agent activity yet: local agents are picked up",
		}
	case where != "":
		return []string{
			"no agent activity yet: " + where,
			"no agent activity yet: POST to the endpoint above",
		}
	default:
		return []string{
			"no agent activity yet: run with --agents to watch coding agents on this machine",
			"no agent activity yet: run with --agents",
		}
	}
}

// kindMarks is each event kind's glyph and color. A kind missing from the
// table reads as dim with a middot rather than being dropped.
var kindMarks = map[string]struct {
	icon string
	st   lipgloss.Style
}{
	core.AgentKindTurn:  {"▸", styleOK},
	core.AgentKindTool:  {"⚙", styleInfo},
	core.AgentKindError: {"✗", styleBad},
	core.AgentKindNote:  {"✎", styleWarn},
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
	mark := kindMarks[ev.Kind]
	if mark.icon == "" {
		mark = struct {
			icon string
			st   lipgloss.Style
		}{"·", styleDim}
	}
	icon, st := mark.icon, mark.st
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
