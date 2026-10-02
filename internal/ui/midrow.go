package ui

// The mid row: the engines, engine state and probes panels side by side.

import "fmt"

func (m Model) renderMidRow() string {
	pw := m.w * 38 / 100
	gw := m.w * 31 / 100
	rw := m.w - pw - gw
	_, midIn, _ := m.sectionHeights()

	provBody, provShown := m.providersBody(pw-4, midIn)
	gaugBody, gaugShown := m.gaugesBody(gw-4, midIn)
	prov := panel(m.enginesTitle(pw-4, provShown), provBody, pw-4, midIn)
	gaug := panel(m.engineStateTitle(gw-4, gaugShown), gaugBody, gw-4, midIn)
	prb := panel(m.probesTitle(rw), m.probesBody(rw-4, midIn), rw-4, midIn)

	return joinAcross(prov, gaug, prb)
}

// enginesTitle names what the ENGINES body could not fit. shown is the count
// the body reports, not a guess from the row budget: the two must agree or the
// badge reads as a sixth engine on a fleet of five.
//
// ENGINES is short enough to carry its count on every legal pane, so it has no
// narrower spelling to fall back on.
func (m Model) enginesTitle(w, shown int) string {
	return moreTitle("ENGINES", "", w, len(m.snap.Providers)-shown)
}

// engineStateTitle is enginesTitle's counterpart over the healthy engines
// only, matching the body that skips the down ones.
//
// KV/TTFT is the short spelling. It says what the gauges measure, which the
// long heading does not, and at seven cells it fits beside "+N more" on the
// narrowest legal pane, where ENGINE STATE could only keep the bare number and
// a reader had no word to tell a hidden reading from a rating.
func (m Model) engineStateTitle(w, shown int) string {
	healthy := 0
	for _, p := range m.snap.Providers {
		if p.OK {
			healthy++
		}
	}
	return moreTitle("ENGINE STATE", "KV/TTFT", w, healthy-shown)
}

// moreTitle counts what the body dropped: the title and the marker beside it.
//
// short is the same heading in fewer cells, for a column the long one cannot
// carry the marker beside. It stands in for the long title, never sits beside
// it: the marker names the way out, and a heading that has given up its cells
// to say nothing drops rows again the moment the marker grows a form. probesTitle
// sheds its measurement the same way.
//
// Only a marker that still says what the count is buys the shorter heading. A
// bare "+3" beside the long heading is the dead end moreMarker exists to avoid,
// so it is not a reason to keep the cells the heading spent.
func moreTitle(title, short string, w, hidden int) string {
	if hidden > 0 && widthOf(short) < widthOf(title) {
		// Prefer the long heading, but only while it can carry a marker that
		// names what the count is; then the short one, which takes the widest
		// marker it can hold at all before it falls back to the bare number.
		for _, t := range []string{title, short} {
			if fitsNamedMore(hidden, widthOf(t), w) {
				return t + moreMarker(t, w, hidden)
			}
		}
		if marker := moreMarker(short, w, hidden); marker != "" {
			return short + marker
		}
	}
	return title + moreMarker(title, w, hidden)
}

// fitsNamedMore reports whether a title of titleWidth cells has room for a
// moreForms spelling on some gap, the wording that names the way out rather
// than the bare number moreMarker falls back to.
func fitsNamedMore(hidden, titleWidth, w int) bool {
	for _, form := range moreForms(hidden) {
		for gap := 2; gap >= 1; gap-- {
			if titleWidth+widthOf(form)+gap <= w {
				return true
			}
		}
	}
	return false
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
	form, gap, ok := fitMore(hidden, widthOf(title), w)
	if !ok {
		return ""
	}
	return spaces(gap) + dim(form)
}

// moreNote is the same marker for a row that carries nothing else, so the
// longest spelling still fits and the leading gap is dropped. The compact
// strip's overflow line is the one row that prints the count on a line of its
// own, and it spelled it a fourth way ("+N more (enlarge window to view)")
// where every other view said the same thing from moreForms.
func moreNote(w, hidden int) string {
	form, _, ok := fitMore(hidden, 0, w)
	if !ok {
		// Nothing fit, not even the bare count: the count is still the reading
		// the reader is owed, so it takes the row and the pane clips it, the
		// same last resort packSegs takes.
		return bareMoreForm(hidden)
	}
	return form
}

// fitMore picks the marker's spelling and the gap it hangs on: the longest
// form that fits beside a title of titleWidth cells, on the widest gap that
// still fits. ok is false when not even the bare count fits there.
func fitMore(hidden, titleWidth, w int) (form string, gap int, ok bool) {
	for _, form := range append(moreForms(hidden), bareMoreForm(hidden)) {
		for gap := 2; gap >= 1; gap-- {
			if titleWidth+widthOf(form)+gap <= w {
				return form, gap, true
			}
		}
	}
	return "", 0, false
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
