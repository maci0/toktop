package ui

// Panel bodies: engine gauges and lines, and the probe results table.

import (
	"fmt"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// providersBody renders whole engine blocks into the mid-row panel and reports
// how many engines it drew. A block is two rows for every engine: the error
// line replaces the stats line on a down one. A row budget of odd height
// therefore ends mid-block, and the cut left a name with no gauge under it, so
// the block that does not fit is dropped whole. Returning the drawn count is
// what keeps the title's "+N more" in step with the body below it.
func (m Model) providersBody(w, rows int) (string, int) {
	var b strings.Builder
	used, shown := 0, 0
	for _, p := range m.snap.Providers {
		block := providerBlock(p, w)
		if used+len(block) > rows {
			break
		}
		for _, ln := range block {
			b.WriteString(ln + "\n")
		}
		used += len(block)
		shown++
	}
	return b.String(), shown
}

func providerBlock(p core.ProviderSnapshot, w int) []string {
	dot := dotUp
	if !p.OK {
		dot = dotBad
	}
	model := shorten(core.SanitizeText(primaryModel(p)), w-15)
	line1 := dot + " " + kindBadge(p.Kind) + " " + styleValue.Render(model)
	if p.Version != "" {
		line1 += " " + dim("v"+shorten(core.SanitizeText(p.Version), 12))
	}
	block := []string{clip(line1, w)}
	if !p.OK {
		return append(block, styleBad.Render("  "+clip(shorten(core.SanitizeText(p.Err), w-3), w-3)))
	}
	kvg := "kv " + GaugeBar(p.KVPct, min(max(w-30, 4), 14), kvHeat)
	stats := fmt.Sprintf("▲%s ▼%s run %d wait %d",
		fmtRate(p.OutTokPS), fmtRate(p.InTokPS), p.Running, p.Waiting)
	return append(block, clip("  "+kvg+" "+styleDim.Render(stats), w))
}

// gaugesBody renders the healthy engines' detail blocks, three rows each (or
// two when the host reports no memory, cpu or ttft for one) into the row
// budget, and reports how many it drew. Same whole-block rule as
// providersBody: a block that would not fit is dropped rather than cut, so
// ENGINE STATE never ends on a kv bar with no engine named above it. The blank
// spacer separates two blocks and is not counted against the last one, which
// would otherwise cost a whole engine on a full panel.
func (m Model) gaugesBody(w, rows int) (string, int) {
	var b strings.Builder
	used, shown := 0, 0
	for _, p := range m.snap.Providers {
		if !p.OK {
			continue
		}
		name := styleDim.Render(clip(shorten(core.SanitizeText(p.Label), w-6), w-6))
		kv := "kv  " + GaugeBar(p.KVPct, min(max(w-10, 4), 20), kvHeat)
		block := []string{name, kv}
		if third := procLine(p); third != "" {
			block = append(block, third)
		}
		need := len(block)
		if shown > 0 {
			need++
		}
		if used+need > rows {
			break
		}
		if shown > 0 {
			b.WriteString("\n")
		}
		for _, ln := range block {
			b.WriteString(clip(ln, w) + "\n")
		}
		used += need
		shown++
	}
	if shown == 0 {
		// No engines yet is genuinely "waiting"; engines present but all
		// down never resolves, so name it instead of promising telemetry.
		if len(m.snap.Providers) == 0 {
			return dim("waiting for telemetry…"), 0
		}
		return dim("no healthy engines (see ENGINES)"), 0
	}
	return b.String(), shown
}

// procLine composes the third detail row: memory/context/process stats.
func procLine(p core.ProviderSnapshot) string {
	var parts []string
	var bytes uint64
	for _, mm := range p.Models {
		bytes = satAddU64(bytes, mm.SizeVRAM)
	}
	if bytes > 0 {
		parts = append(parts, "mem "+humanBytes(bytes))
	} else if len(p.Models) > 0 && p.Models[0].CtxMax > 0 {
		// CtxMax is uint64; a direct int64() of 2^63 is negative.
		n := int64(min(p.Models[0].CtxMax, ^uint64(0)>>1))
		parts = append(parts, "ctx "+fmtCount(n)+" tok")
	}
	if p.ProcRSS > 0 {
		rss := "rss " + humanBytesShort(p.ProcRSS)
		if p.ProcCPU > 0 {
			rss += fmt.Sprintf(" %.0f%%", p.ProcCPU)
		}
		parts = append(parts, rss)
	}
	if p.TTFTms > 0 {
		parts = append(parts, "ttft "+fmtMs(p.TTFTms))
	}
	if len(parts) == 0 {
		return ""
	}
	return styleDim.Render(strings.Join(parts, " · "))
}

// satAddU64 adds saturating at MaxUint64: a wrapped sum of two engine-reported
// VRAM sizes would read as a small allocation instead of "full".
func satAddU64(a, b uint64) uint64 {
	if b > ^uint64(0)-a {
		return ^uint64(0)
	}
	return a + b
}

// probeReadout is the one-line outcome of the last probe, for the views that
// have no PROBES panel to carry it: while a probe is pending it is the same
// badge, after that the measured ttft and rate, or the failure. Empty when
// nothing has been measured, so a view with no probes yet says nothing.
func (m Model) probeReadout() string {
	if !m.probeReq.IsZero() {
		return styleWarn.Render("● probing…")
	}
	last, ok := m.lastProbe()
	if !ok {
		return ""
	}
	if !last.OK {
		return styleBad.Render("probe failed")
	}
	return dim("probe") + " " + fmtMs(last.TTFTms) + " " + styleOK.Render(fmtRate(last.TokPS)+" tok/s")
}

func (m Model) probesTitle() string {
	t := "PROBES"
	if !m.probeReq.IsZero() {
		return t + "  " + styleWarn.Render("● probing…")
	}
	if last, ok := m.lastProbe(); ok {
		if last.OK {
			t += " " + dim("last") + " " + fmtMs(last.TTFTms) + " " + styleOK.Render(fmtRate(last.TokPS)+" tok/s")
		} else {
			// A failed last result used to print "last - 0.0/s" in the success
			// color, which reads as a measurement of nothing.
			t += " " + styleBad.Render("last failed")
		}
	}
	return t
}

// probeModelMin is the narrowest model column a probe row keeps, so a narrow
// pane still names the model instead of dropping it. The successful and the
// failed row reserve the same w-18; the floor keeps that positive on the
// smallest legal dashboard pane.
const probeModelMin = 8

func (m Model) probesBody(w, h int) string {
	vals := probeSeries(m.snap, w, m.chartCadence())
	chartH := min(max(h-3-len(m.snap.Providers), 2), 8)
	var out strings.Builder
	out.WriteString(BrailleChart(vals, w, chartH, ChartStyle{Heat: heatColor}) + "\n")
	shown := 0
	for i := len(m.snap.Probes) - 1; i >= 0 && shown < 2; i-- {
		p := m.snap.Probes[i]
		// The floor matters on a legal narrow pane: w-18 goes to zero or
		// below around 62 columns, and shorten("") there drops the model
		// entirely, leaving two probe rows that name no model at all. The
		// line is clipped to w below either way.
		model := styleDim.Render(shorten(core.SanitizeText(p.Model), max(w-18, probeModelMin)))
		var line string
		if !p.OK {
			// Match the plain frame and ENGINES: name the failure and keep
			// the reason. Printing 0.0/s in the same shape as a success
			// hides why the probe did not land.
			line = styleBad.Render("✗") + " " + model + " " + styleBad.Render("failed")
			if msg := strings.TrimSpace(core.SanitizeText(p.Err)); msg != "" {
				line += " " + dim(shorten(msg, max(w-24, 8)))
			}
		} else {
			line = styleOK.Render("✓") + " " + model +
				" " + fmtRate(p.TokPS) + " tok/s " + dim("ttft") + " " + fmtMs(p.TTFTms)
		}
		out.WriteString(clip(line, w) + "\n")
		shown++
	}
	if len(m.snap.Probes) == 0 {
		out.WriteString(dim("press ") + styleInfo.Render("p") + dim(" to fire a probe") + "\n")
		// Short enough for the narrowest legal pane's PROBES column: the
		// longer spelling ended in an ellipsis on every dashboard.
		out.WriteString(dim("q to quit, re-run with --probe N"))
	}
	return out.String()
}
