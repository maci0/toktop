package ui

// The mid row: the engines, engine state and probes panel titles side by side.

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) renderMidRow() string {
	pw := m.w * 38 / 100
	gw := m.w * 31 / 100
	rw := m.w - pw - gw
	_, midIn, _ := m.sectionHeights()

	provBody, provShown := m.providersBody(pw-4, midIn)
	gaugBody, gaugShown := m.gaugesBody(gw-4, midIn)
	prov := panel(m.enginesTitle(pw-4, provShown), provBody, pw-4, midIn)
	gaug := panel(m.engineStateTitle(gw-4, gaugShown), gaugBody, gw-4, midIn)
	prb := panel(clip(m.probesTitle(), rw), m.probesBody(rw-4, midIn), rw-4, midIn)

	return lipgloss.JoinHorizontal(lipgloss.Top, prov, gaug, prb)
}

// enginesTitle names what the ENGINES body could not fit. shown is the count
// the body reports, not a guess from the row budget: the two must agree or the
// badge reads as a sixth engine on a fleet of five.
func (m Model) enginesTitle(w, shown int) string {
	return moreTitle("ENGINES", w, len(m.snap.Providers)-shown)
}

// engineStateTitle is enginesTitle's counterpart over the healthy engines
// only, matching the body that skips the down ones.
func (m Model) engineStateTitle(w, shown int) string {
	healthy := 0
	for _, p := range m.snap.Providers {
		if p.OK {
			healthy++
		}
	}
	return moreTitle("ENGINE STATE", w, healthy-shown)
}

func moreTitle(title string, w, hidden int) string {
	if hidden > 0 {
		more := fmt.Sprintf("+%d more", hidden)
		if lipgloss.Width(title)+lipgloss.Width(more)+2 <= w {
			title += "  " + dim(more)
		}
	}
	return title
}
