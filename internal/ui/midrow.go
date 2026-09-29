package ui

// The mid row: the engines, engine state and probes panel titles side by side.

import (
	"fmt"
	"strings"

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

// moreTitle counts what the body dropped: the title and the marker beside it.
func moreTitle(title string, w, hidden int) string {
	return title + moreMarker(title, w, hidden)
}

// moreMarker is the overflow marker's spacing and spelling for a title: the
// leading gap plus the longest form that fits beside the title it hangs on, or
// nothing at all when not even the bare number does. Every title that drops
// rows takes it from here, so one pane's overflow is worded the same way
// wherever it is counted.
//
// The count alone is a dead end: a panel that says "+3 more" and offers no way
// to reach those three reads as three engines the tool cannot see, which is a
// different and wrong conclusion from the one that is true. The way out is a
// bigger pane, so the marker says so wherever it has the cells.
//
// A title too narrow for the marker on its usual gap keeps the count rather
// than losing it: a reading the reader cannot account for is the worse of the
// two. The gap closes before the wording does, and the bare number is the last
// form, so a title never drops the count to buy a longer sentence beside it.
func moreMarker(title string, w, hidden int) string {
	if hidden <= 0 {
		return ""
	}
	forms := append(moreForms(hidden), bareMoreForm(hidden))
	for _, form := range forms {
		for gap := 2; gap >= 1; gap-- {
			if lipgloss.Width(title)+lipgloss.Width(form)+gap <= w {
				return strings.Repeat(" ", gap) + dim(form)
			}
		}
	}
	return ""
}

// bareMoreForm is the overflow marker with nothing beside it but the number,
// for a column too narrow for the word "more" on its usual gap. It is not one
// of moreForms: a row that can shed a reading to keep the sentence naming the
// way out must go on doing that, and the bare number is the last resort of a
// column that has nothing left to shed.
func bareMoreForm(hidden int) string { return fmt.Sprintf("+%d", hidden) }

// moreForms is the overflow marker's spellings, longest first: the count with
// the way out, then the count alone. Every row that drops readings takes the
// first form that fits, so a truncated count names the same next step whether
// it sits in a panel title or in the host strip.
func moreForms(hidden int) []string {
	return []string{
		fmt.Sprintf("+%d more (enlarge window)", hidden),
		fmt.Sprintf("+%d more", hidden),
	}
}
