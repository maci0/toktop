package ui

// Footer, empty state, help overlay and the compact (sub-minDashW) view.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// canProbe, canTimescale and canSwapFocus are the single answer to "does this
// key have something to act on right now". The footer and the help screen
// both read them, so the key reference never advertises a key the footer just
// hid, and never hides one the footer is printing.
func (m Model) canProbe() bool {
	// p fires real generations and the probing marker lives on the PROBES
	// panel, which is absent without engines. Advertising it here would be
	// a key that appears to do nothing (same rule as renderMinimal).
	return m.cfg.Prober != nil && len(m.snap.Providers) > 0
}

// canTimescale: t only changes the throughput chart; the empty setup card has
// none.
func (m Model) canTimescale() bool {
	return len(m.snap.Providers) > 0 || len(m.snap.Agents) > 0
}

// canSwapFocus: a swaps which side gets the panel estate. Advertised only
// where it changes something: without engines the agents view is already the
// view, and the key stays live so a focus left on agents can be undone.
func (m Model) canSwapFocus() bool {
	return len(m.snap.Providers) > 0 && (len(m.snap.Agents) > 0 || m.cfg.Agents || m.focusAgents)
}

// noticeMinCells is the shortest notice the footer will print beside the key
// list. Below it the two cannot share a row, and the key list is what gives way:
// the notice names its own key ("p: …"), so on its own it still answers the
// press, where the key list alone would drop the answer off the clipped edge.
const noticeMinCells = 16

func (m Model) renderFooter() string {
	base := styleInfo.Render("q") + dim(" quit  ") +
		styleInfo.Render("space") + dim(" pause  ")
	// The keys that are not on every frame, in the order they are dropped.
	// "?" is not among them: it is how a reader finds the reference that lists
	// the rest, so a row too narrow for the full list sheds from the right
	// rather than letting the pane clip the last hint away.
	var opt []string
	if m.canProbe() {
		opt = append(opt, styleInfo.Render("p")+dim(" probe  "))
	}
	if m.canTimescale() {
		opt = append(opt, styleInfo.Render("t")+dim(" timescale  "))
	}
	if m.canSwapFocus() {
		label := " agents  "
		if m.focusAgents {
			label = " engines  "
		}
		opt = append(opt, styleInfo.Render("a")+dim(label))
	}
	foot := func() string { return base + strings.Join(opt, "") + styleInfo.Render("?") + dim(" help") }
	tag := ""
	if m.cfg.Demo {
		tag = styleWarn.Render(fmt.Sprintf(" DEMO seed %d ", m.cfg.DemoSeed)) + " "
	}
	keys := foot()
	for m.w > 0 && len(opt) > 0 && widthOf(tag)+widthOf(keys) > m.w {
		opt = opt[:len(opt)-1]
		keys = foot()
	}
	if m.notice == "" {
		return tag + keys
	}
	// The notice shares the footer row so a key that did nothing is answered
	// where the key itself is printed.
	sep := dim("  ·  ")
	if avail := m.w - widthOf(tag) - widthOf(keys) - widthOf(sep); avail >= noticeMinCells {
		return tag + keys + sep + styleWarn.Render(m.notice)
	}
	room := max(m.w-widthOf(tag), 0)
	return tag + styleWarn.Render(shorten(m.notice, room))
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
	// A run with --agents never reaches this card: the no-engines branch in
	// View hands it the agents dashboard instead, so there is no --agents hint
	// to give here and no --agents variant of the cards below.
	switch {
	case m.feedDown != "":
		lines = append(lines, "")
		lines = append(lines, m.feedEmptyLines(m.w-8)...)
		lines = append(lines, dim("fix the cause named above, then q and restart toktop"))
	case m.cfg.IngestAddr != "":
		lines = append(lines,
			"",
			"POST agent events to the live ingest endpoint:",
			styleInfo.Render("  http://"+core.SanitizeText(m.cfg.IngestAddr)+"/v1/events"),
		)
	}
	lines = append(lines,
		"",
		dim("q quit, then re-run:"),
		"attach anything openai-compatible:",
		styleInfo.Render("  toktop --add http://127.0.0.1:9999"),
		"or watch coding agents on this machine:",
		styleInfo.Render("  toktop --agents"),
	)
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

// helpRows is the in-app key reference. It lists the same keys the footer
// advertises, under the same conditions: a key with nothing to act on here
// only earns a notice explaining that, and a reference that listed it anyway
// would send the reader looking for a control that is not on screen. Compact
// panes drop the flag list too, which names panels the compact strip does not
// draw. The "● probing…" badge p draws lives in a panel title, not here.
func (m Model) helpRows() [][2]string {
	if m.w < minDashW || m.h < minDashH {
		return [][2]string{
			{"q / ctrl+c", "quit"},
			{"esc", "close help / quit"},
			{"space", "pause / resume"},
			{"? / h", "toggle this help"},
		}
	}
	rows := [][2]string{
		{"q / ctrl+c", "quit"},
		{"esc", "close help / quit"},
		{"space", "pause / resume streaming"},
	}
	if m.canProbe() {
		rows = append(rows, [2]string{"p", "probe every engine with a real generation"})
	}
	if m.canTimescale() {
		rows = append(rows, [2]string{"t", "toggle compressed timescale + grid"})
	}
	if m.canSwapFocus() {
		label := "focus the agents dashboard (esc comes back)"
		if m.focusAgents {
			label = "go back to the engines dashboard"
		}
		rows = append(rows, [2]string{"a", label})
	}
	rows = append(rows,
		[2]string{"? / h", "toggle this help"},
		[2]string{"", ""},
		[2]string{"(flags)", "quit, then re-run with these"},
		[2]string{"--demo", "simulated fleet, zero setup"},
		[2]string{"--add URL", "attach an openai-compatible endpoint"},
		[2]string{"ssh://host", "watch engines on another host"},
		[2]string{"--agents", "also watch coding agents on this machine"},
		[2]string{"--probe N", "auto-probe every N seconds"},
		[2]string{"--once", "print one frame and exit"},
		[2]string{"--plain", "with --once: linear text report"},
	)
	return rows
}

// minimalHint is the compact view's one line of orientation: it drops the
// header and the footer, so this is all that says why the dashboard is not on
// screen and what size it needs. The forms run longest first and the first one
// that fits wins, because clipping the sentence cut the very minimum it exists
// to name ("min 62×" with the number gone read as a size nobody has).
func (m Model) minimalHint() string {
	forms := []string{
		fmt.Sprintf("enlarge window (min %d×%d, current %d×%d) for full dashboard", minDashW, minDashH, m.w, m.h),
		fmt.Sprintf("enlarge window (min %d×%d) for full dashboard", minDashW, minDashH),
		fmt.Sprintf("enlarge window to %d×%d (now %d×%d)", minDashW, minDashH, m.w, m.h),
		fmt.Sprintf("enlarge window: %d×%d", minDashW, minDashH),
		fmt.Sprintf("min %d×%d", minDashW, minDashH),
	}
	for _, f := range forms {
		if widthOf(f) <= m.w {
			return f
		}
	}
	return shorten(forms[len(forms)-1], m.w)
}

// renderMinimal is the degraded view for panes too small for the dashboard:
// one line per engine, plus orientation the compact layout must carry on its
// own because the footer and header are not rendered here.
func (m Model) renderMinimal() string {
	var lines []string
	lines = append(lines, dim(m.minimalHint()))
	// space pauses here too: without a badge a frozen strip is
	// indistinguishable from a feed that stalled.
	if m.paused {
		lines = append(lines, clip(styleWarn.Render("‖ PAUSED"), m.w))
	}
	if r := m.probeReadout(); r != "" {
		lines = append(lines, clip(r, m.w))
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
	// still work, but their results render only in the full dashboard. The
	// compact help above adds esc, which quits from here too.
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
