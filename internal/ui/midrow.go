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

	prov := panel(m.enginesTitle(pw-4, midIn), m.providersBody(pw-4), pw-4, midIn)
	gaug := panel(m.engineStateTitle(gw-4, midIn), m.gaugesBody(gw-4), gw-4, midIn)
	prb := panel(clip(m.probesTitle(), rw), m.probesBody(rw-4, midIn), rw-4, midIn)

	return lipgloss.JoinHorizontal(lipgloss.Top, prov, gaug, prb)
}

func (m Model) enginesTitle(w, midIn int) string {
	title := "ENGINES"
	visible := max(midIn/2, 1)
	if hidden := len(m.snap.Providers) - visible; hidden > 0 {
		more := fmt.Sprintf("+%d more", hidden)
		if lipgloss.Width(title)+lipgloss.Width(more)+2 <= w {
			title += "  " + dim(more)
		}
	}
	return title
}

func (m Model) engineStateTitle(w, midIn int) string {
	title := "ENGINE STATE"
	healthy := 0
	for _, p := range m.snap.Providers {
		if p.OK {
			healthy++
		}
	}
	visible := max((midIn+1)/4, 1)
	if hidden := healthy - visible; hidden > 0 {
		more := fmt.Sprintf("+%d more", hidden)
		if lipgloss.Width(title)+lipgloss.Width(more)+2 <= w {
			title += "  " + dim(more)
		}
	}
	return title
}

// renderSystem is the two-row host strip: row 1 is live vitals (mem, gpus),
