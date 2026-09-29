package ui

// The system strip: host identity, CPU, memory, GPUs and temperatures.

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// shownCPUTemps is how many core temperature readings either renderer prints.
// The rest become a "+N more" count in the dashboard and are dropped by the
// text report, which has no width to fit them in.
const shownCPUTemps = 4

// tempLabelWidth caps a sensor label in the dashboard. The text report does
// not cap: a terminal wraps, so the full field stays readable there.
const tempLabelWidth = 7

// cpuTempLabel is a sensor's label as both renderers show it: the first
// comma-separated field, with the separator Fields needed dropped after the
// split. Trimming before the width cap matters in the dashboard, which
// shortens: capping "coretemp," to 7 cells and trimming the comma afterwards
// rendered six cells of label where seven had been budgeted.
func cpuTempLabel(label string) string {
	return strings.TrimSuffix(strings.Fields(label + ",")[0], ",")
}

func (m Model) renderSystem() string {
	w := m.w - 4
	sy := m.snap.Sys
	vitals := []string{styleTitle.Render("SYS")}
	switch {
	case sy == nil || sy.MemTotal == 0:
		vitals = append(vitals, dim("mem n/a"))
	default:
		memPct := float64(sy.MemUsed) / float64(sy.MemTotal) * 100
		vitals = append(vitals,
			"mem "+GaugeBar(memPct, min(max(w/6, 8), 18), memHeat)+
				" "+dim(humanBytesShort(sy.MemUsed)+"/"+humanBytesShort(sy.MemTotal)))
		if sy.SwapTotal > 0 {
			swPct := float64(sy.SwapUsed) / float64(sy.SwapTotal) * 100
			mb := memBand(swPct)
			vitals = append(vitals, dim("swp ")+
				wrap(fgRun(mb.color()), mb.markPrefix()+fmt.Sprintf("%.0f%%", swPct)))
		}
		if sy.Load1 > 0 || sy.Load5 > 0 {
			vitals = append(vitals, dim("ld ")+styleValue.Render(fmt.Sprintf("%.2f", sy.Load1)))
		}
	}
	vitals = append(vitals, gpuSegments(sy)...)

	var ident []string
	if sy != nil && (sy.CPUModel != "" || sy.OsName != "" || len(sy.Drivers) > 0 || len(sy.NPUs) > 0) {
		ident = hostSegments(sy, stripHostLimits)
	}

	cpuTemps := sysCPUTemps(sy)
	shownTemps := 0
	for _, t := range cpuTemps {
		if shownTemps >= shownCPUTemps {
			break
		}
		// Labels come from local sysfs and (for remote hosts) parsed vendor
		// tooling output; they pass the terminal sanitizer like every other
		// externally sourced string.
		label := shorten(core.SanitizeText(cpuTempLabel(t.Label)), tempLabelWidth)
		tb := tempBand(float64(t.MilliC) / 1000)
		ident = append(ident, dim(label+" ")+
			wrap(boldFgRun(tb.color()), tb.markPrefix()+fmtTempC(t.MilliC)))
		shownTemps++
	}
	switch {
	case sy == nil:
	case shownTemps == 0 && len(ident) == 0:
		// Nothing rendered above: a kernel or NPU-only host used to miss
		// this and got both a segment and the "no sensors" line.
		ident = append(ident, dim("no sensors found"))
	}

	// The strip is a panel with no title row: frame(padBlock) draws it the same
	// way panel does, without lipgloss's border and padding pass over a block
	// this function had already cut to one known width.
	//
	// A row that ran out of cells reports what it left out rather than ending
	// mid-list: on a host with several accelerators the vitals row stops after
	// the first GPU and says nothing, which reads as one GPU on the machine.
	// The identity row counts the temperatures its cap never turned into
	// segments together with the ones the pack shed, so one "+N more" covers
	// the whole row.
	content := padBlock(packSegs(vitals, w, 0), w, 1)
	if len(ident) > 0 && m.stripTwoRows() { // must match systemStripRows' budget
		content += "\n" + padBlock(packSegs(ident, w, len(cpuTemps)-shownTemps), w, 1)
	}
	return frame(content, w)
}

// packSegs fits segs into w cells and, when some do not fit, ends the row with
// the number left out. Segments are shed from the right, and a kept segment is
// given up if the count will not fit beside it: an unaccounted-for reading is
// the worse of the two losses.
//
// The count carries the way out, in the spellings moreTitle uses for the same
// overflow: the shed segments are shed for want of width, so a wider pane
// reaches them, and a bare "+N more" reads as readings the tool cannot see.
//
// extraHidden counts readings that never became segments at all (the
// temperature cap), which have to land in the same number as the ones the pack
// shed, or the row carries two "+N more" a reader cannot tell apart.
func packSegs(segs []string, w int, extraHidden int) string {
	// Segment widths are measured once, into a prefix sum, so asking how wide
	// a prefix is, or whether the overflow marker now fits beside it, is
	// arithmetic. The shed search walked the whole row it had just built for
	// every candidate it tried, once per segment it dropped, so a host with
	// several accelerators and sensors paid a full re-measure of the strip per
	// candidate.
	sums := make([]int, len(segs)+1)
	for i, s := range segs {
		wSeg := widthOf(s)
		if i > 0 {
			wSeg = widthOf(sepDim + s)
		}
		sums[i+1] = sums[i] + wSeg
	}
	kept := 0
	for kept < len(segs) && sums[kept+1] <= w {
		kept++
	}
	hidden := extraHidden + len(segs) - kept
	if hidden == 0 {
		return spreadRow(segs)
	}
	sepW := widthOf(sepDim)
	for kept > 0 {
		for _, form := range moreForms(hidden) {
			mark := dim(form)
			if sums[kept]+sepW+widthOf(mark) <= w {
				return spreadRow(slices.Concat(segs[:kept], []string{mark}))
			}
		}
		kept--
		hidden++
	}
	// Nothing fit to sit beside the count, so the count takes the row on its
	// own, in its shortest spelling: the row is packed to w and a longer marker
	// would run off the end of it.
	return dim(bareMoreForm(hidden))
}

// spreadRow joins segs with the strip's separator, for the one place that has
// to rebuild a packed row instead of appending to it.
func spreadRow(segs []string) string {
	return strings.Join(segs, sepDim)
}

// hostSegmentLimits caps each identity segment's cells. The SYS strip packs
// these left to right on one row, so cutting a long CPU model to a readable
// segment is right there; the plain report has no row to fit and asks for the
// same fields uncapped, since a truncated model name there is a fact the
// reader cannot recover.
type hostSegmentLimits struct{ cpu, os, drivers int }

// stripHostLimits are the SYS strip's per-segment caps.
var stripHostLimits = hostSegmentLimits{cpu: 22, os: 34, drivers: 40}

// fitSeg caps s to n cells, or returns it whole when n is 0 or less.
func fitSeg(s string, n int) string {
	if n <= 0 {
		return s
	}
	return shorten(s, n)
}

// hostSegments adds CPU model, OS·kernel and driver versions to the strip.
// All values can originate from another host (ssh vitals) or vendor tooling,
// so they pass the terminal sanitizer.
func hostSegments(sy *core.SysSample, lim hostSegmentLimits) []string {
	var segs []string
	if sy.CPUModel != "" {
		segs = append(segs, dim(fitSeg(core.SanitizeText(sy.CPUModel), lim.cpu)))
	}
	if sy.OsName != "" || sy.Kernel != "" {
		osPart := core.SanitizeText(sy.OsName)
		if sy.Kernel != "" {
			osPart = strings.TrimSpace(osPart + " · " + core.SanitizeText(sy.Kernel))
		}
		segs = append(segs, dim(fitSeg(osPart, lim.os)))
	}
	if len(sy.Drivers) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(sy.Drivers)) {
			parts = append(parts, core.SanitizeText(k)+" "+core.SanitizeText(sy.Drivers[k]))
		}
		segs = append(segs, styleInfo.Render(fitSeg(strings.Join(parts, " · "), lim.drivers)))
	}
	if len(sy.NPUs) > 0 {
		names := make([]string, len(sy.NPUs))
		for i, n := range sy.NPUs {
			names[i] = core.SanitizeText(n)
		}
		segs = append(segs, styleInfo.Render("npu: "+strings.Join(names, ",")))
	}
	return segs
}

// gpuSegment renders one compact accelerator readout: temp util vram watts.
func gpuSegment(g core.GPUDevice) string {
	var b strings.Builder
	b.WriteString(dim(shortVendor(g.Vendor) + strconv.Itoa(g.Index) + " "))
	if g.MilliC > 0 {
		tb := tempBand(float64(g.MilliC) / 1000)
		b.WriteString(wrap(boldFgRun(tb.color()), tb.markPrefix()+fmtTempC(g.MilliC)) + " ")
	}
	if g.UtilPct > 0 {
		b.WriteString(styleInfo.Render(fmt.Sprintf("%.0f%%", g.UtilPct)) + " ")
	}
	switch {
	case g.MemTotal > 0:
		b.WriteString(dim(humanBytesShort(g.MemUsed) + "/" + humanBytesShort(g.MemTotal)))
	case g.Name != "":
		// Model names can come from a remote host's nvidia-smi output.
		b.WriteString(dim(shorten(core.SanitizeText(g.Name), 16)))
	}
	if g.PowerW > 0 {
		b.WriteString(" " + styleWarn.Render(fmt.Sprintf("%.0fW", g.PowerW)))
	}
	return b.String()
}

func gpuSegments(sy *core.SysSample) []string {
	if sy == nil {
		return nil
	}
	segs := make([]string, 0, len(sy.GPUs))
	for _, g := range sy.GPUs {
		segs = append(segs, gpuSegment(g))
	}
	return segs
}

func shortVendor(v string) string {
	switch v {
	case "nvidia":
		return "nv"
	case "amd", "intel", "apple":
		return v
	default:
		return "gpu"
	}
}

// sysCPUTemps drops GPU-marked readings, which arrive via SysSample.GPUs
// instead, but only when the sample carries GPU devices. With none, every
// reading is kept: a host whose only temperature sensors are GPU-bound would
// otherwise print no temperatures at all.
func sysCPUTemps(sy *core.SysSample) []core.TempReading {
	if sy == nil {
		return nil
	}
	if len(sy.GPUs) > 0 {
		var cpu []core.TempReading
		for _, t := range sy.Temps {
			if !t.IsGPU {
				cpu = append(cpu, t)
			}
		}
		return cpu
	}
	return sy.Temps
}

// tempBand and memBand are the ramps these readings are classified on, and
// the color each one draws with. A bare reading (a temperature, a swap
// percentage) is prefixed with its band's mark, so its severity survives a
// monochrome terminal and a colorblind reader; the gauge meters carry a bar
// and a number already and are left alone.
func tempBand(celsius float64) band { return bandOf(celsius, tempWarnC, tempCritC) }

func tempColor(celsius float64) lipgloss.Color { return tempBand(celsius).color() }

func memBand(v float64) band { return bandOf(v, memWarnPct, memCritPct) }

func memHeat(v float64) lipgloss.Color { return memBand(v).color() }

func fmtTempC(milliC int) string {
	return fmt.Sprintf("%.0f°", float64(milliC)/1000)
}
