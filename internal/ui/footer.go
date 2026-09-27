package ui

// Footer, empty state, help overlay and the compact (sub-minDashW) view.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

func (m Model) renderFooter() string {
	foot := styleInfo.Render("q") + dim(" quit  ") +
		styleInfo.Render("space") + dim(" pause  ")
	// p fires real generations and the probing marker lives on the PROBES
	// panel, which is absent without engines. Advertising it here would be
	// a key that appears to do nothing (same rule as renderMinimal).
	if m.cfg.Prober != nil && len(m.snap.Providers) > 0 {
		foot += styleInfo.Render("p") + dim(" probe  ")
	}
	// t only changes the throughput chart; the empty setup card has none.
	if len(m.snap.Providers) > 0 || len(m.snap.Agents) > 0 {
		foot += styleInfo.Render("t") + dim(" timescale  ")
	}
	// a swaps which side gets the panel estate. Advertised only where it
	// changes something: without engines the agents view is already the
	// view, and the key stays live so a focus left on agents can be undone.
	if len(m.snap.Providers) > 0 && (len(m.snap.Agents) > 0 || m.cfg.Agents || m.focusAgents) {
		label := " agents"
		if m.focusAgents {
			label = " engines"
		}
		foot += styleInfo.Render("a") + dim(label+"  ")
	}
	foot += styleInfo.Render("?") + dim(" help")
	// The notice shares the footer row so a key that did nothing is answered
	// where the key itself is printed.
	if m.notice != "" {
		foot += dim("  ·  ") + styleWarn.Render(m.notice)
	}
	tag := ""
	if m.cfg.Demo {
		tag = styleWarn.Render(fmt.Sprintf(" DEMO seed %d ", m.cfg.DemoSeed)) + " "
	}
	return tag + foot
}

func (m Model) renderEmpty() string {
	logo := wordmark
	lines := []string{
		logo,
		"",
		styleWarn.Render("no inference engines detected"),
		dim("scanned localhost :11434 :30000 :8000 :8080 :1234 …"),
	}
	if m.paused {
		// space pauses here too: without a badge the setup card stays frozen
		// after an engine appears, with no hint that snapshots are dropped.
		lines = append(lines, "", styleWarn.Render("‖ PAUSED"))
	}
	// In-session next actions first: ingest is already listening, and
	// --agents may already be on. Restart commands come after, named as
	// such, so first-timers do not type them into the dashboard.
	if m.cfg.Agents {
		lines = append(lines, "", dim("watching local agents; waiting for one to report tokens"))
	}
	switch {
	case m.feedDown != "":
		lines = append(lines, "")
		lines = append(lines, m.feedEmptyLines(m.w-8)...)
		lines = append(lines, dim("q quit, then restart toktop to restore ingest"))
	case m.cfg.IngestAddr != "":
		if !m.cfg.Agents {
			lines = append(lines, "")
		}
		lines = append(lines,
			"POST agent events to the live ingest endpoint:",
			styleInfo.Render("  http://"+core.SanitizeText(m.cfg.IngestAddr)+"/v1/events"),
		)
	}
	lines = append(lines,
		"",
		dim("q quit, then re-run:"),
		"attach anything openai-compatible:",
		styleInfo.Render("  toktop --add http://127.0.0.1:9999"),
	)
	if !m.cfg.Agents {
		lines = append(lines,
			"or watch coding agents on this machine:",
			styleInfo.Render("  toktop --agents"),
		)
	}
	lines = append(lines,
		"or preview the dashboard:",
		styleInfo.Render("  toktop --demo"),
	)
	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Padding(1, 3).
		Render(strings.Join(lines, "\n"))
	// Footer sits on the last row; Place into the remaining height so the
	// centered card plus keys still fit the pane.
	body := lipgloss.Place(m.w, max(m.h-2, 1), lipgloss.Center, lipgloss.Center, card)
	return composeFrame(body, m.renderFooter(), m.w, m.h)
}

func (m Model) renderHelp() string {
	var b strings.Builder
	// The overlay replaces the whole screen, so it carries the same bold
	// title every panel does; without one it reads as an untitled fragment.
	b.WriteString(styleTitle.Render("KEYS") + "\n")
	for _, r := range m.helpRows() {
		key := styleInfo.Render(padTo(r[0], 12))
		b.WriteString(key + dim(r[1]) + "\n")
	}
	box := helpStyle.Render(strings.TrimSuffix(b.String(), "\n"))
	// A pane that advertises "?" may be smaller than this box. Centering a
	// too-tall box pads the top and overflows the bottom; clip in place.
	if lipgloss.Width(box) > m.w || lipgloss.Height(box) > m.h {
		return clipBlock(box, m.w, m.h)
	}
	placed := lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
	return clipBlock(placed, m.w, m.h)
}

// helpRows is the in-app key reference. Compact panes drop p, t and the
// restart-flag list, which name panels the compact strip does not draw;
// matching that list here keeps help from advertising keys that appear to do
// nothing. The "● probing…" badge p draws lives in the strip, not here.
func (m Model) helpRows() [][2]string {
	if m.w < minDashW || m.h < minDashH {
		return [][2]string{
			{"q / ctrl+c", "quit"},
			{"esc", "close help / quit"},
			{"space", "pause / resume"},
			{"? / h", "toggle this help"},
		}
	}
	return [][2]string{
		{"q / ctrl+c", "quit"},
		{"esc", "close help / quit"},
		{"space", "pause / resume streaming"},
		{"p", "probe every engine with a real generation"},
		{"t", "toggle compressed timescale + grid"},
		{"a", "focus engines or agents"},
		{"? / h", "toggle this help"},
		{"", ""},
		{"(flags)", "quit, then re-run with these"},
		{"--demo", "simulated fleet, zero setup"},
		{"--add URL", "attach an openai-compatible endpoint"},
		{"ssh://host", "watch engines on another host"},
		{"--agents", "also watch coding agents on this machine"},
		{"--probe N", "auto-probe every N seconds"},
		{"--once", "print one frame and exit"},
		{"--plain", "with --once: linear text report"},
	}
}

// renderMinimal is the degraded view for panes too small for the dashboard:
// one line per engine, plus orientation the compact layout must carry on its
// own because the footer and header are not rendered here.
func (m Model) renderMinimal() string {
	var lines []string
	hint := fmt.Sprintf("enlarge window (min %d×%d) for full dashboard", minDashW, minDashH)
	if m.w >= 60 {
		hint = fmt.Sprintf("enlarge window (min %d×%d, current %d×%d) for full dashboard", minDashW, minDashH, m.w, m.h)
	}
	lines = append(lines, dim(clip(hint, m.w)))
	// space pauses here too: without a badge a frozen strip is
	// indistinguishable from a feed that stalled.
	if m.paused {
		lines = append(lines, clip(styleWarn.Render("‖ PAUSED"), m.w))
	}
	if !m.probeReq.IsZero() {
		lines = append(lines, clip(styleWarn.Render("● probing…"), m.w))
	}
	// The compact foot is one clipped line wide, too narrow to carry a notice
	// beside the keys; the body is the only place it fits.
	if m.notice != "" {
		lines = append(lines, clip(styleWarn.Render(m.notice), m.w))
	}
	rates := m.agentRates()
	if len(m.snap.Providers) == 0 {
		if len(rates) == 0 {
			if m.cfg.Agents {
				// This run asked for agents; leading with the engines it was
				// told not to need reads as a failure instead of a wait.
				lines = append(lines, dim(clip("watching local agents…", m.w)))
			} else {
				lines = append(lines, clip(styleWarn.Render("no inference engines detected"), m.w))
				// "or" so this is not read as one command with every flag.
				lines = append(lines, dim(clip("try --demo, --add URL, or --agents", m.w)))
			}
		}
		for _, r := range rates {
			lines = append(lines, clip(agentMiniLine(r), m.w))
		}
	}
	for _, p := range m.snap.Providers {
		dot := dotUp
		if !p.OK {
			dot = dotBad
		}
		line := dot + " " + core.SanitizeText(p.Label) + " " + fmtRate(p.OutTokPS) + " tok/s"
		if !p.OK {
			// ✗ matches ENGINES/probes so greyscale still reads, and "down"
			// plus the error keep the reason the full view shows.
			line += " " + styleBad.Render("down")
			if msg := strings.TrimSpace(core.SanitizeText(p.Err)); msg != "" {
				line += " " + dim(shorten(msg, 32))
			}
		}
		lines = append(lines, clip(line, m.w))
	}
	if len(m.snap.Providers) > 0 {
		for _, r := range rates {
			lines = append(lines, clip(agentMiniLine(r), m.w))
		}
	}
	// Only keys with a visible effect in this layout are advertised. p and t
	// still work, but their results render only in the full dashboard; compact
	// help matches this list.
	foot := dim(clip("q quit · space pause · ? help", m.w))
	bodyH := max(m.h-lipgloss.Height(foot)-1, 0)
	if len(lines) > bodyH && bodyH >= 3 {
		hidden := len(lines) - (bodyH - 1)
		lines = lines[:bodyH-1]
		lines = append(lines, dim(clip(fmt.Sprintf("+%d more (enlarge window to view)", hidden), m.w)))
	}
	body := clipBlock(strings.Join(lines, "\n"), m.w, bodyH)
	if bodyH == 0 {
		return clipBlock(foot, m.w, m.h)
	}
	return composeFrame(body, foot, m.w, m.h)
}
