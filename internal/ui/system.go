package ui

// The system strip: host identity, CPU, memory, GPUs and temperatures.

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// row 2 is identity and sensors (cpu, os, drivers, temps).
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
			st := lipgloss.NewStyle().Foreground(memHeat(swPct))
			vitals = append(vitals, dim("swp ")+st.Render(fmt.Sprintf("%.0f%%", swPct)))
		}
		if sy.Load1 > 0 || sy.Load5 > 0 {
			vitals = append(vitals, dim("ld ")+styleValue.Render(fmt.Sprintf("%.2f", sy.Load1)))
		}
	}
	vitals = append(vitals, gpuSegments(sy)...)

	var ident []string
	if sy != nil && (sy.CPUModel != "" || sy.OsName != "" || len(sy.Drivers) > 0 || len(sy.NPUs) > 0) {
		ident = hostSegments(sy)
	}

	cpuTemps := sysCPUTemps(sy)
	shownTemps := 0
	for _, t := range cpuTemps {
		if shownTemps >= 4 {
			break
		}
		// Labels come from local sysfs and (for remote hosts) parsed vendor
		// tooling output; they pass the terminal sanitizer like every other
		// externally sourced string.
		label := shorten(core.SanitizeText(strings.Fields(t.Label + ",")[0]), 7)
		label = strings.TrimSuffix(label, ",")
		c := tempColor(float64(t.MilliC) / 1000)
		ident = append(ident, dim(label+" ")+
			lipgloss.NewStyle().Bold(true).Foreground(c).Render(fmtTempC(t.MilliC)))
		shownTemps++
	}
	switch {
	case sy == nil:
	case shownTemps == 0 && len(sy.GPUs) == 0 && sy.CPUModel == "" &&
		len(sy.Drivers) == 0 && sy.OsName == "":
		ident = append(ident, dim("no sensors found"))
	case len(cpuTemps) > shownTemps:
		// Counted on the filtered list: GPU readings already render as their
		// own segments, so they are neither hidden nor counted here.
		ident = append(ident, dim(fmt.Sprintf("+%d more", len(cpuTemps)-shownTemps)))
	}

	row1 := padBlock(joinSpreadLeft(vitals, w), w, 1)
	row2 := ""
	if len(ident) > 0 && m.stripTwoRows() { // must match systemStripRows' budget
		row2 = "\n" + padBlock(joinSpreadLeft(ident, w), w, 1)
	}
	return panelStyle.Render(row1 + row2)
}

// hostSegments adds CPU model, OS·kernel and driver versions to the strip.
// All values can originate from another host (ssh vitals) or vendor tooling,
// so they pass the terminal sanitizer.
func hostSegments(sy *core.SysSample) []string {
	var segs []string
	if sy.CPUModel != "" {
		segs = append(segs, dim(shorten(core.SanitizeText(sy.CPUModel), 22)))
	}
	if sy.OsName != "" || sy.Kernel != "" {
		osPart := core.SanitizeText(sy.OsName)
		if sy.Kernel != "" {
			osPart = strings.TrimSpace(osPart + " · " + core.SanitizeText(sy.Kernel))
		}
		segs = append(segs, dim(shorten(osPart, 34)))
	}
	if len(sy.Drivers) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(sy.Drivers)) {
			parts = append(parts, core.SanitizeText(k)+" "+core.SanitizeText(sy.Drivers[k]))
		}
		segs = append(segs, styleInfo.Render(shorten(strings.Join(parts, " · "), 40)))
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
		c := tempColor(float64(g.MilliC) / 1000)
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(c).Render(fmtTempC(g.MilliC)) + " ")
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
	case "amd":
		return "amd"
	case "intel":
		return "intel"
	case "apple":
		return "apple"
	default:
		return "gpu"
	}
}

// sysCPUTemps filters out GPU hwmon readings; those arrive via SysSample.GPUs.
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

func tempColor(celsius float64) lipgloss.Color {
	if math.IsNaN(celsius) || celsius < 60 {
		return cGreen
	}
	switch {
	case celsius < 80:
		return cYellow
	default:
		return cRed
	}
}

func memHeat(v float64) lipgloss.Color {
	if math.IsNaN(v) || v < 70 {
		return cGreen
	}
	switch {
	case v < 90:
		return cYellow
	default:
		return cRed
	}
}

func fmtTempC(milliC int) string {
	return fmt.Sprintf("%.0f°", float64(milliC)/1000)
}
