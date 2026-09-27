package ui

// Panel bodies: engine gauges and lines, and the probe results table.

import (
	"fmt"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

func (m Model) providersBody(w int) string {
	var b strings.Builder
	for _, p := range m.snap.Providers {
		dot := dotUp
		if !p.OK {
			dot = dotBad
		}
		model := shorten(core.SanitizeText(primaryModel(p)), w-15)
		line1 := dot + " " + kindBadge(p.Kind) + " " + styleValue.Render(model)
		if p.Version != "" {
			line1 += " " + dim("v"+shorten(core.SanitizeText(p.Version), 12))
		}
		b.WriteString(clip(line1, w) + "\n")
		if !p.OK {
			b.WriteString(styleBad.Render("  "+clip(shorten(core.SanitizeText(p.Err), w-3), w-3)) + "\n")
		} else {
			kvg := "kv " + GaugeBar(p.KVPct, min(max(w-30, 4), 14), kvHeat)
			stats := fmt.Sprintf("▲%s ▼%s run %d wait %d",
				fmtRate(p.OutTokPS), fmtRate(p.InTokPS), p.Running, p.Waiting)
			line2 := "  " + kvg + " " + styleDim.Render(stats)
			b.WriteString(clip(line2, w) + "\n")
		}
	}
	return b.String()
}

func (m Model) gaugesBody(w int) string {
	var b strings.Builder
	for _, p := range m.snap.Providers {
		if !p.OK {
			continue
		}
		name := styleDim.Render(clip(shorten(core.SanitizeText(p.Label), w-6), w-6))
		kv := "kv  " + GaugeBar(p.KVPct, min(max(w-10, 4), 20), kvHeat)
		third := procLine(p)
		row := clipBlock(name+"\n"+kv+"\n"+third, w, -1)
		b.WriteString(row + "\n\n")
	}
	if b.Len() == 0 {
		// No engines yet is genuinely "waiting"; engines present but all
		// down never resolves, so name it instead of promising telemetry.
		if len(m.snap.Providers) == 0 {
			return dim("waiting for telemetry…")
		}
		return dim("no healthy engines (see ENGINES)")
	}
	return b.String()
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

// probeModelMin is the narrowest model column a probe row keeps. The
// successful and failed rows reserve different widths for it (the rate and
// ttft readouts), so the floor is set where neither reservation is negative
// on the smallest legal dashboard pane.
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
		out.WriteString(dim("q quit, then --probe N to auto-probe"))
	}
	return out.String()
}
