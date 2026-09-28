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

	return joinAcross(prov, gaug, prb)
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

// moreTitle counts what the body dropped. The count alone is a dead end: a
// panel that says "+3 more" and offers no way to reach those three reads as
// three engines the tool cannot see, which is a different and wrong conclusion
// from the one that is true. The way out is a bigger pane, so the title says
// so, the way the compact strip's overflow line already does. A column too
// narrow for the sentence keeps the bare count rather than losing the count:
// an engine the reader cannot account for is the worse of the two.
func moreTitle(title string, w, hidden int) string {
	if hidden <= 0 {
		return title
	}
	for _, form := range []string{
		fmt.Sprintf("+%d more (enlarge window)", hidden),
		fmt.Sprintf("+%d more", hidden),
	} {
		if lipgloss.Width(title)+lipgloss.Width(form)+2 <= w {
			return title + "  " + dim(form)
		}
	}
	return title
}
