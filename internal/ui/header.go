package ui

// Header and the height budget the whole frame is laid out against.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

func (m Model) renderHeader() string {
	logo := wordmark
	segs := []headerSeg{{text: logo}, {text: dim("v" + m.cfg.Version), shed: 40}}

	up, tot := m.upCount()
	rates := core.AgentRates(m.snap.Agents, m.snapNow())
	if tot == 0 {
		n := len(rates)
		if n == 0 {
			n = uniqueAgents(m.snap.Agents)
		}
		label := fmt.Sprintf("%d agents", n)
		if n == 1 {
			label = "1 agent"
		}
		st := styleOK
		if n == 0 {
			st = styleWarn
		}
		segs = append(segs, headerSeg{text: st.Render(label)})
	} else {
		dot := dotUp
		st := styleOK
		switch {
		case up == 0:
			dot, st = dotBad, styleBad
		case up < tot:
			dot, st = dotWarn, styleWarn
		}
		segs = append(segs, headerSeg{text: st.Render(fmt.Sprintf("%s %d/%d engines", strip(dot), up, tot))})
		if n := len(rates); n > 0 {
			label := fmt.Sprintf("%d agents", n)
			if n == 1 {
				label = "1 agent"
			}
			segs = append(segs, headerSeg{text: dim(label), shed: 45})
		}
	}

	outV := styleValue.Foreground(heatColor(norm(m.aggLast, m.aggMax))).Render("▲ " + fmtRate(m.aggLast))
	inV := styleInfo.Render("▼ " + fmtRate(aggInAt(m.snap, m.snapNow())))
	segs = append(segs,
		headerSeg{text: outV + " " + dim("tok/s out"), shed: 10},
		headerSeg{text: inV + " " + dim("in"), shed: 20},
	)

	if up > 0 || tot > 0 {
		// "session", matching --once --plain: "up 5m" next to "2/3 engines"
		// reads as engine uptime.
		segs = append(segs, headerSeg{text: dim("session " + fmtDur(m.snap.Uptime)), shed: 50})
	}
	if m.snap.Sys != nil && m.snap.Sys.RemoteHost != "" {
		segs = append(segs, headerSeg{text: styleInfo.Render("via ssh:" + core.SanitizeText(m.snap.Sys.RemoteHost))})
	}

	right := ""
	if m.paused {
		right += styleWarn.Render("‖ PAUSED ") + dim("│ ")
	}
	// Ticks and snapshot stamps may carry UTC or a sender offset; show the
	// viewer's clock, matching feedLine.
	right += styleDim.Render(m.clock.Local().Format("15:04:05"))
	left := fitSegments(segs, m.w-lipgloss.Width(right)-1)
	return joinSpread(left, right, m.w)
}

// headerSeg is one header chunk plus how eagerly it yields space on narrow
// panes: 0 pins the segment, larger numbers shed sooner.
type headerSeg struct {
	text string
	shed int
}

// fitSegments sheds the highest-numbered segments (rightmost first) until
// the dim-piped row fits avail cells. When nothing sheddable remains it hard
// clips as a last resort: even one wrapping cell drags every later frame line
// out of alignment on terminals narrower than the row.
func fitSegments(segs []headerSeg, avail int) string {
	if avail <= 0 || len(segs) == 0 {
		return ""
	}
	kept := slices.Clone(segs)
	width := func(ss []headerSeg) int {
		n := 0
		for i, s := range ss {
			if i > 0 {
				n += lipgloss.Width(dim(" │ "))
			}
			n += lipgloss.Width(s.text)
		}
		return n
	}
	for len(kept) > 1 && width(kept) > avail {
		worst, idx := 0, -1
		for i, s := range kept {
			if s.shed >= worst && s.shed > 0 {
				worst, idx = s.shed, i
			}
		}
		if idx < 0 {
			break
		}
		kept = append(kept[:idx], kept[idx+1:]...)
	}
	parts := make([]string, len(kept))
	for i, s := range kept {
		parts[i] = s.text
	}
	line := strings.Join(parts, dim(" │ "))
	if w := lipgloss.Width(line); w > avail {
		line = clip(line, avail)
	}
	return line
}

// minDashW / minDashH are the shortest pane where the full layout still
// covers its own chrome (header, titles, borders, footer, one system strip
// row) plus the smallest panel split from sectionHeights. Below either, a
// squeezed full frame overflows and bubbletea clips it from the top, hiding
// the header; the compact view stays honest instead. Help uses the same
// gate so a pane that advertises "?" does not open a box taller than itself.
const (
	minDashW = 62
	minDashH = 30
)

// minIdentH is the shortest pane that still affords a second system strip
// row; below it identity and sensors yield rather than pushing the frame
// past the pane edge.
const minIdentH = 31

// stripTwoRows reports whether the pane affords the system strip's second
// row. It must agree with renderSystem's row-2 gate or the height budget
// lies by one row; erring toward fewer rendered rows than budgeted is safe
// (the leftover becomes padding), the opposite direction overflows.
func (m Model) stripTwoRows() bool {
	return m.snap.Sys != nil && m.h >= minIdentH
}

// systemStripRows is the total height of the system strip: border (2) plus
// one row of vitals, plus a second identity row when stripTwoRows says so.
func (m Model) systemStripRows() int {
	if m.stripTwoRows() {
		return 4
	}
	return 3
}

// sectionHeights splits the body into exact inner heights for the throughput
// chart, the mid-row panels and the agent feed. Fixed chrome is computed from
// the header, blank spacer, three panel titles, box borders, the prompt chart
// row, system strip and footer: outH+midIn+feedIn must sum to exactly f or
// the frame overflows the pane and bubbletea clips it from the top, hiding
// the header. Minimums reshuffle the split but never change the sum.
func (m Model) sectionHeights() (outH, midIn, feedIn int) {
	f := max(m.h-17-m.systemStripRows(), 10)
	outH = min(max(int(float64(f)*0.42), 3), 99)
	feedIn = min(max(int(float64(f)*0.22), 2), 12)
	midIn = f - outH - feedIn
	if midIn < 5 { // mid-row panels need room for three detail lines
		outH -= 5 - midIn
		midIn = 5
		if outH < 3 { // charts bottomed out: take the rest from the feed
			feedIn -= 3 - outH
			outH = 3
		}
	}
	return outH, midIn, feedIn
}
