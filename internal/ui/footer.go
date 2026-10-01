package ui

// Footer, empty state, help overlay and the compact (sub-minDashW) view.

import (
	"fmt"
	"slices"
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
	// panel, which is absent without engines. The compact strip is the one
	// layout that advertises p anyway, because it prints the probe outcome
	// itself; everywhere else it would be a key that appears to do nothing.
	return m.cfg.Prober != nil && len(m.snap.Providers) > 0
}

// canTimescale: t only changes the throughput chart; the empty setup card has
// none, and the text report is not a chart in any layout.
func (m Model) canTimescale() bool {
	if m.cfg.Plain {
		return false
	}
	return len(m.snap.Providers) > 0 || len(m.snap.Agents) > 0
}

// canSwapFocus: a swaps which side gets the panel estate. Advertised only
// where it changes something: without engines the agents view is already the
// view, and the key stays live so a focus left on agents can be undone. The
// text report carries both halves in one linear document, so there is no
// estate to swap and the key is not advertised there.
func (m Model) canSwapFocus() bool {
	if m.cfg.Plain {
		return false
	}
	return len(m.snap.Providers) > 0 && (len(m.snap.Agents) > 0 || m.cfg.Agents || m.focusAgents)
}

// demoTag names the seed a demo frame was drawn from, or nothing on a real
// run. The seed is reported so a demo frame is reproducible, which only holds
// if the frame says which run produced it, so every layout that draws a frame
// carries it: the footer everywhere but the compact strip, which renders no
// footer and takes the tag as a line of its own.
func (m Model) demoTag() string {
	if !m.cfg.Demo {
		return ""
	}
	return m.footNotice(fmt.Sprintf(" DEMO seed %d ", m.cfg.DemoSeed)) + " "
}

func (m Model) renderFooter() string {
	base := m.footKey("q") + m.footLabel(" quit  ") +
		m.footKey("space") + m.footLabel(" pause  ")
	// The keys that are not on every frame, in the order they are dropped.
	// "?" is not among them: it is how a reader finds the reference that lists
	// the rest, so a row too narrow for the full list sheds from the right
	// rather than letting the pane clip the last hint away.
	var opt []string
	if m.canProbe() {
		opt = append(opt, m.footKey("p")+m.footLabel(" probe  "))
	}
	if m.canTimescale() {
		opt = append(opt, m.footKey("t")+m.footLabel(" timescale  "))
	}
	if m.canSwapFocus() {
		label := " agents  "
		if m.focusAgents {
			label = " engines  "
		}
		opt = append(opt, m.footKey("a")+m.footLabel(label))
	}
	foot := func(opt []string) string {
		return base + strings.Join(opt, "") + m.footKey("?") + m.footLabel(" help")
	}
	tag := m.demoTag()
	keys := foot(opt)
	for m.w > 0 && len(opt) > 0 && widthOf(tag)+widthOf(keys) > m.w {
		opt = opt[:len(opt)-1]
		keys = foot(opt)
	}
	if m.notice == "" {
		return tag + keys
	}
	// The notice shares the footer row so a key that did nothing is answered
	// where the key itself is printed. Sharing is bought by shedding the
	// optional keys, and then by closing the gap between the two, rather than by
	// dropping the list: a pane narrow enough for the full list lost every key,
	// q quit and ? help included, for as long as the notice stood, so the key
	// that did nothing took the reader's map of the app away with it. The
	// notice itself is printed whole or not at all; cut mid-sentence it stops
	// answering the press it exists to answer.
	notice := m.footNotice(m.notice)
	for _, sep := range []string{m.footLabel("  ·  "), " "} {
		kept := slices.Clone(opt)
		for {
			if widthOf(tag+foot(kept)+sep+notice) <= m.w {
				return tag + foot(kept) + sep + notice
			}
			if len(kept) == 0 {
				break
			}
			kept = kept[:len(kept)-1]
		}
	}
	// Nothing beside the notice fits, and the notice names its own key, so it
	// takes the row on its own rather than being clipped off it.
	room := max(m.w-widthOf(tag), 0)
	return tag + m.footNotice(shorten(m.notice, room))
}

// footKey, footLabel and footNotice style the footer row, and return their
// argument unstyled for the plain report. That report is the non-visual frame:
// a screen reader reads an SGR run as nothing (WCAG 1.4.3, which the drawn
// frame meets instead by painting cBase behind itself). The words already
// carry what the colors were carrying.
func (m Model) footKey(s string) string {
	if m.cfg.Plain {
		return s
	}
	return styleInfo.Render(s)
}

func (m Model) footLabel(s string) string {
	if m.cfg.Plain {
		return s
	}
	return dim(s)
}

func (m Model) footNotice(s string) string {
	if m.cfg.Plain {
		return s
	}
	return styleWarn.Render(s)
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

// helpChromeRows is what the key box spends around its rows: the border and
// the padding above and below, plus the KEYS title and the mute line. The
// remainder is the key list itself.
const helpChromeRows = 6

// helpContentRows is how many key rows the box has left after its chrome.
func (m Model) helpContentRows() int {
	return max(m.h-helpChromeRows, 1)
}

// helpWindow is how many key rows the box shows at once. A list taller than
// the box gives up one row to the indicator naming what is off-screen, so the
// row that indicator replaces is a row the reader can still scroll to.
func (m Model) helpWindow() int {
	n := m.helpContentRows()
	if len(m.helpRows()) > n {
		n--
	}
	return max(n, 1)
}

// helpScrollMax is the furthest the list can be scrolled: the rows that do not
// fit below the first window.
func (m Model) helpScrollMax() int {
	return max(len(m.helpRows())-m.helpWindow(), 0)
}

func (m Model) renderHelp() string {
	var b strings.Builder
	// The overlay replaces the whole screen, so it carries the same bold
	// title every panel does; without one it reads as an untitled fragment.
	b.WriteString(styleTitle.Render("KEYS") + "\n")
	// This view mutes every action key (Update), so the reference has to say
	// so: pressing space here and watching nothing happen is the difference
	// between "read the list first" and "this thing is broken".
	// The scroll affordance is the indicator row below, not a note on this
	// line: "↓ 3 more" names the key that reaches the rows a short pane cannot
	// show, and it stays inside the box on a pane this narrow. A hint spelled
	// out here was the longest line in it, and clipped the box.
	b.WriteString(dim("action keys are muted here") + "\n")
	// A pane too short for the whole list is scrolled, not clipped: dropping
	// the tail from the bottom left the flag list unreachable with no key that
	// could reach it (WCAG 2.1.1).
	rows := m.helpRows()
	first := min(m.helpScroll, m.helpScrollMax())
	shown := rows[first:min(first+m.helpWindow(), len(rows))]
	for _, r := range shown {
		key := styleInfo.Render(padTo(r[0], 12))
		b.WriteString(key + dim(r[1]) + "\n")
	}
	if above, below := first, len(rows)-first-len(shown); above > 0 || below > 0 {
		b.WriteString(styleInfo.Render(helpScrollLabel(above, below)) + "\n")
	}
	box := helpStyle.Render(strings.TrimSuffix(b.String(), "\n"))
	// A pane that advertises "?" may be narrower than this box. Centering a
	// too-wide or too-tall box overflows an edge; clip in place.
	if lipgloss.Width(box) > m.w || lipgloss.Height(box) > m.h {
		return clipBlock(box, m.w, m.h)
	}
	placed := lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
	return clipBlock(placed, m.w, m.h)
}

// helpScrollLabel says what up and down will reach, so the reader knows the
// list continues rather than guessing from a row that stops mid-list. "end"
// rides along because the rows below are usually the flags, and on a pane with
// a two-row window a one-line key is the difference between one press and one
// press per row.
func helpScrollLabel(above, below int) string {
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("↑ %d above · ↓ %d more · end", above, below)
	case above > 0:
		return fmt.Sprintf("↑ %d above · end", above)
	case below > 0:
		return fmt.Sprintf("↓ %d more · end", below)
	default:
		return ""
	}
}

// helpRows is the in-app key reference. It lists the same keys the footer
// advertises, under the same conditions: a key with nothing to act on here
// only earns a notice explaining that, and a reference that listed it anyway
// would send the reader looking for a control that is not on screen. Compact
// panes drop the flag list too, which names panels the compact strip does not
// draw. The "● probing…" badge p draws lives in a panel title, not here.
func (m Model) helpRows() [][2]string {
	if m.w < minDashW || m.h < minDashH {
		// Short forms: the pane can be too short to hold the whole list, and
		// a narrow one leaves the box body a few cells across once the border
		// and padding take their six, so a longer
		// description is clipped mid-word by the pane. The three keys that
		// close the box lead, so a pane too short for the whole list drops the
		// row at the bottom rather than the way out of it.
		rows := [][2]string{
			{"q / ctrl+c", "close help, then quit"},
			// esc is the one key whose job differs between this screen and the
			// dashboard behind it, and the compact pane is where naming both
			// matters most: a reader who shrank the pane or raised the font
			// past the full layout is here by necessity, and this row is the
			// only place inside the product that still tells them what esc
			// does once the box is closed. The spelling is the short one, at
			// the width the rows above already hold.
			{"esc", "close, or quit here"},
			{"? / h", "close help"},
			{"space", "pause / resume"},
		}
		// p belongs here too: the compact strip renders the probe outcome
		// (renderMinimal prints probeReadout), so the key has a visible effect
		// in this layout even though the PROBES panel it usually updates is
		// not on screen. t and a stay out, because the strip draws no chart
		// and no panels to swap; they answer the press with a notice instead.
		if m.canProbe() {
			rows = append(rows, [2]string{"p", "probe every engine"})
		}
		return rows
	}
	rows := [][2]string{
		// Every key here is read from inside the help view, where q and esc
		// close the box instead of acting on the dashboard behind it. Spelling
		// that out is the difference between a reader who presses q, watches
		// the overlay go away and understands, and one who reads it as a
		// dropped keystroke.
		{"q / ctrl+c", "close this help (press again to quit)"},
		// The esc row is the one key whose job differs between this screen and
		// the dashboard behind it, so it names both. Read from here, esc closes
		// the box; read from the dashboard it comes back from the agents view or
		// quits. The footer advertises q alone, so without this line the quit is
		// documented nowhere in the app.
		{"esc", "close this; back or quit from the dashboard"},
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
		[2]string{"? / h", "close this help"},
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

// compactKeys is the compact strip's one-line key list, shed whole items in
// the order renderFooter sheds them rather than clipped: a half-printed
// "p probe", or a row ending on a dangling separator, reads as a rendering
// fault instead of as a shorter list. p goes first, then the pointer to the
// help that names the rest, and "q quit" is the last thing standing.
func (m Model) compactKeys() string {
	items := []string{"q quit", "space pause"}
	if m.canProbe() {
		items = append(items, "p probe")
	}
	items = append(items, "? help")
	for _, shed := range []string{"p probe", "? help", "space pause"} {
		if widthOf(strings.Join(items, keySep)) <= m.w {
			break
		}
		items = slices.DeleteFunc(items, func(k string) bool { return k == shed })
	}
	return strings.Join(items, keySep)
}

// keySep divides the keys on a footer row.
const keySep = " · "

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

// compactEmptyHint is the compact strip's one line of advice for a run that has
// neither engines nor agents: where to send work, or which flag to re-run with.
//
// It is the strip's counterpart to renderEmpty's setup card and to the AGENT
// FEED panel title, which both name the ingest endpoint when the run has one.
// That endpoint is the only piece of state unique to such a run — nothing else
// on the strip distinguishes `toktop --ingest` from a bare `toktop` — so a
// strip that dropped it left a reader who shrank their pane with no way back to
// the one thing they asked the run to do. It is graded longest-first the way
// feedEmptyForms grades the feed's own hint, so a narrow strip still ends on a
// whole instruction rather than half of an address.
//
// "or" keeps the flags from reading as one command carrying all of them.
func (m Model) compactEmptyHint() string {
	forms := []string{"try --demo, --add URL, or --agents"}
	if m.cfg.IngestAddr != "" {
		forms = []string{
			"POST events to http://" + core.SanitizeText(m.cfg.IngestAddr) +
				"/v1/events, or try --demo or --agents",
			"POST to http://" + core.SanitizeText(m.cfg.IngestAddr) + "/v1/events",
			"POST to the ingest endpoint",
			"POST to /v1/events",
		}
	}
	for _, f := range forms {
		if widthOf(f) <= m.w {
			return f
		}
	}
	// The last form is a prefix of every one above it, so on a pane narrower
	// than it the strip still ends on the action rather than on half a URL.
	return shorten(forms[len(forms)-1], m.w)
}

// renderMinimal is the degraded view for panes too small for the dashboard:
// one line per engine, plus orientation the compact layout must carry on its
// own because the footer and header are not rendered here.
func (m Model) renderMinimal() string {
	var lines []string
	// Identity first, the way the header leads the full frame: a demo run on a
	// pane too small for the footer has nowhere else to say which seed produced
	// the numbers below it.
	if tag := m.demoTag(); tag != "" {
		lines = append(lines, clip(tag, m.w))
	}
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
	// The agent feed degrades under this layout too, and there is no AGENT
	// FEED panel here to carry the reason: the strip is the only place it can
	// live, and on stderr it is hidden under the alternate screen.
	if m.feedDown != "" {
		lines = append(lines, clip(styleBad.Render(shorten(core.SingleLine(m.feedDown), m.w)), m.w))
	}
	rates := m.agentRates()
	if len(m.snap.Providers) == 0 {
		// Events, not rates, decide this: a run fed only over the ingest
		// endpoint with nothing inside the rate window holds agents and no
		// rates, and telling it no engines were detected contradicts the full
		// layout, which renders the agents view for the same snapshot. The
		// rule is draw's, restated here.
		if len(rates) == 0 {
			if m.cfg.Agents || len(m.snap.Agents) > 0 {
				// This run asked for agents; leading with the engines it was
				// told not to need reads as a failure instead of a wait.
				lines = append(lines, dim(clip("watching local agents…", m.w)))
			} else {
				lines = append(lines, clip(styleWarn.Render("no inference engines detected"), m.w))
				lines = append(lines, dim(clip(m.compactEmptyHint(), m.w)))
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
		line := dot + " " + core.SingleLine(p.Label)
		if !p.OK {
			// ✗ matches ENGINES/probes so greyscale still reads, and "down"
			// plus the error keep the reason the full view shows. No rate: a
			// failed engine has none, and "0.0 tok/s down" names two states at
			// once. The full view's answer (an error line, no stats row) and
			// probeOutcome's are the same rule.
			line += " " + styleBad.Render("down")
			if msg := strings.TrimSpace(core.SingleLine(p.Err)); msg != "" {
				line += " " + dim(shorten(msg, 32))
			}
		} else {
			line += " " + fmtRate(p.OutTokPS) + " tok/s"
		}
		lines = append(lines, clip(line, m.w))
	}
	if len(m.snap.Providers) > 0 {
		for _, r := range rates {
			lines = append(lines, clip(agentMiniLine(r), m.w))
		}
	}
	// Only keys with a visible effect in this layout are advertised. p is one
	// of them: the strip prints the probe outcome above, so the key that
	// produces it belongs beside the ones that act here. t is not (no chart on
	// this layout) and a is not (no panels to swap); both answer the press with
	// a notice instead. The compact help adds esc, which quits from here too.
	foot := dim(clip(m.compactKeys(), m.w))
	bodyH := max(m.h-lipgloss.Height(foot)-1, 0)
	if len(lines) > bodyH && bodyH >= 3 {
		hidden := len(lines) - (bodyH - 1)
		lines = lines[:bodyH-1]
		lines = append(lines, dim(clip(moreNote(m.w, hidden), m.w)))
	}
	body := clipBlock(strings.Join(lines, "\n"), m.w, bodyH)
	if bodyH == 0 {
		return clipBlock(foot, m.w, m.h)
	}
	return composeFrame(body, foot, m.w, m.h)
}
