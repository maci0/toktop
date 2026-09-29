package ui

// Panel bodies: engine gauges and lines, and the probe results table.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
	unorm "golang.org/x/text/unicode/norm"
)

// providersBody renders whole engine blocks into the mid-row panel and reports
// how many engines it drew. A block is two rows for every engine: the error
// line replaces the stats line on a down one. A row budget of odd height
// therefore ends mid-block, and the cut left a name with no gauge under it, so
// the block that does not fit is dropped whole. Returning the drawn count is
// what keeps the title's "+N more" in step with the body below it.
//
// Down engines are drawn first. The panel shows as many engines as the row
// budget holds and the rest are named only as a count, so a pane too short for
// the fleet hides whatever sits at the end of the collector's order, and that
// is where a failed engine lands: the one row carrying its reason was the one
// row that got cut. Healthy engines keep their relative order, so the panel
// still lines up with ENGINE STATE beside it.
func (m Model) providersBody(w, rows int) (string, int) {
	var b strings.Builder
	used, shown := 0, 0
	for _, p := range m.providersByStatus() {
		// The row budget is settled before the block is built. An engine block
		// is always providerBlockRows rows whatever the engine reports, so the
		// last engine a short panel does not have room for can be dropped for
		// the price of a comparison instead of a badge, three styled strings,
		// a gauge and a clip that are then thrown away.
		if used+providerBlockRows > rows {
			break
		}
		for _, ln := range providerBlock(p, w) {
			b.WriteString(ln + "\n")
		}
		used += providerBlockRows
		shown++
	}
	return b.String(), shown
}

// providersByStatus lists the failed engines ahead of the healthy ones, each
// group in the snapshot's own order.
func (m Model) providersByStatus() []core.ProviderSnapshot {
	ordered := make([]core.ProviderSnapshot, 0, len(m.snap.Providers))
	for _, p := range m.snap.Providers {
		if !p.OK {
			ordered = append(ordered, p)
		}
	}
	for _, p := range m.snap.Providers {
		if p.OK {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// Cells the engine block's first row spends on identity, narrowest pane
// first: the label is what tells two engines of one kind apart, so it is
// taken before the model and before the version, and a cell with too few
// cells left is dropped whole rather than cut to an ellipsis that says
// nothing.
const (
	labelCells   = 10
	modelMinCell = 4
	versionCells = 9
	// minGaugeBar is the shortest bar that still reads as a bar; below it the
	// engine row drops the gauge rather than clip the queue counts behind it.
	// maxGaugeBar keeps the bar from swallowing a wide pane the way
	// gaugesBody's 20-cell bars do.
	minGaugeBar = 3
	maxGaugeBar = 14
)

// providerBlockRows is what an engine block costs in panel rows, healthy or
// not: the stats or error line sits under the name either way, and neither
// path drops one. providersBody settles the row budget against this before
// building a block it may not draw.
const providerBlockRows = 2

func providerBlock(p core.ProviderSnapshot, w int) []string {
	dot := dotUp
	if !p.OK {
		dot = dotBad
	}
	row := dot + " " + kindBadge(p.Kind)
	// The label comes first because the kind badge names a category two
	// engines on different ports share ("vllm"), while the label is the only
	// thing telling them apart. ENGINE STATE and the plain report name it too;
	// the block did not, so two engines of one kind read as the same line. A
	// label that only repeats the badge adds nothing and is dropped.
	room := w - lipgloss.Width(row) - 1
	label := strings.TrimSpace(core.SingleLine(p.Label))
	// The label is engine-supplied and the kind is a token, so they meet in
	// both normalization forms: an engine naming itself "café" in NFD
	// (e + combining acute) spells the same word as the kind's NFC "café",
	// and comparing the raw bytes renders a label that only repeats the
	// badge. ModelName composes at its own boundary, so this compares
	// like with like.
	if !strings.EqualFold(unorm.NFC.String(label), unorm.NFC.String(strings.TrimSpace(p.Kind))) {
		label = shorten(label, min(labelCells, max(room, 0)))
	} else {
		label = ""
	}
	if label != "" {
		row += " " + styleValue.Render(label)
		room -= widthOf(label) + 1
	}
	// No models is the normal state of a down engine, and the "-" placeholder
	// primaryModel returns for that read as a model named "-". The plain report
	// drops the same placeholder; the block does too.
	//
	// Dim, unlike the label beside it: label and model were both styleValue,
	// so "vllm-a100 Qwen/Qwen2.5-32B" rendered as one bold run and nothing on
	// the row said which half named the engine. The plain report already
	// separates them (name on the head line, model on the detail line), and the
	// stats row below is dim for the same reason.
	if model := primaryModel(p); model != "" && model != "-" && room > modelMinCell {
		row += " " + dim(shorten(core.SingleLine(model), room-1))
		room = 0
	}
	if p.Version != "" && room > versionCells {
		row += " " + dim("v"+shorten(core.SingleLine(p.Version), 12))
	}
	line1 := clip(row, w)
	block := []string{line1}
	if !p.OK {
		return append(block, styleBad.Render("  "+clip(shorten(core.SingleLine(p.Err), w-3), w-3)))
	}
	// The stats get first claim on the width and the kv bar takes what is
	// left: sizing the bar from the pane alone pushed "wait 12" off the right
	// edge of a default-width pane, so a queue backing up read as a missing
	// value. GaugeBar spends w cells of bar plus " NN%", behind the "kv " label.
	stats := styleDim.Render(engineStats(p, w-2))
	if bar := min(w-2-lipgloss.Width(stats)-1-3-4, maxGaugeBar); bar >= minGaugeBar {
		return append(block, clip("  kv "+GaugeBar(p.KVPct, bar, kvHeat)+" "+stats, w))
	}
	return append(block, clip("  "+stats, w))
}

// engineStats composes one engine's out rate, in rate and queue counts for a
// row w cells wide, dropping the in rate when the row cannot carry it: the
// header and the PROMPT chart both show fleet-wide input, while the queue
// counts are drawn from this row and no other panel, so on the narrowest legal
// pane the input rate is what gives way.
func engineStats(p core.ProviderSnapshot, w int) string {
	out := "▲" + fmtRate(p.OutTokPS)
	in := "▼" + fmtRate(p.InTokPS)
	queue := fmt.Sprintf("run %d wait %d", p.Running, p.Waiting)
	row := out + " " + in + " " + queue
	if widthOf(row) > w {
		return out + " " + queue
	}
	return row
}

// gaugesBlockRows is the floor a gauge block costs: the name and the kv bar.
// A third row for the process line is added per engine that reports one, and
// a blank spacer between blocks, so the budget is settled on procLine's answer
// before the block is built.
const gaugesBlockRows = 2

// gaugesBody renders the healthy engines' detail blocks, three rows each (or
// two when the engine reports neither vram, context length, rss nor ttft for
// one) into the row
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
		// Only procLine decides how tall the block is, so the budget is
		// settled on it before the label and the gauge are built: the engine a
		// short panel cannot fit costs one procLine rather than a sanitized,
		// clipped and styled label plus a gauge bar.
		third := procLine(p)
		need := gaugesBlockRows
		if third != "" {
			need++
		}
		if shown > 0 {
			need++
		}
		if used+need > rows {
			break
		}
		if shown > 0 {
			b.WriteString("\n")
		}
		name := styleDim.Render(clip(shorten(core.SingleLine(p.Label), w-6), w-6))
		block := []string{name, "kv  " + GaugeBar(p.KVPct, min(max(w-10, 4), 20), kvHeat)}
		if third != "" {
			block = append(block, third)
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
		bytes = core.SatAddU64(bytes, mm.SizeVRAM)
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
		// The reason, where it fits: the PROBES panel prints it on a failed
		// row, and these two views have no such row, so a failure here was a
		// bare "probe failed" with no way to learn why. The compact strip
		// clips the line, and the AGENTS title drops the whole part if it does
		// not fit, so a narrow pane keeps the badge alone.
		out := styleBad.Render("probe failed")
		if msg := strings.TrimSpace(core.SingleLine(last.Err)); msg != "" {
			out += " " + dim(shorten(msg, 24))
		}
		return out
	}
	return dim("probe") + " " + fmtMs(last.TTFTms) + " " + styleOK.Render(fmtRate(last.TokPS)+" tok/s")
}

// probesTitle is the PROBES heading, built the way feedTitle and the AGENTS
// title build theirs: the optional part joins only while it fits, because a
// panel title is not clipped by the panel and an over-wide one stretches every
// row below it. renderMidRow still clips the title as a last resort, and that
// clip is what cut the widest reading in half ("last 120ms 4…"), leaving a
// number no reader can use. A title that carries the measurement whole, or
// drops it whole, reads at every width the mid-row can give this column.
//
// The rate alone is the shorter form: PROBES takes the narrowest third of the
// mid-row, so on a 62-cell pane the ttft does not fit beside the rate and the
// rate is the half worth keeping.
func (m Model) probesTitle(w int) string {
	title := "PROBES"
	add := func(part string) {
		if lipgloss.Width(title)+lipgloss.Width(part) <= w {
			title += part
		}
	}
	if !m.probeReq.IsZero() {
		add("  " + styleWarn.Render("● probing…"))
		return title
	}
	last, ok := m.lastProbe()
	if !ok {
		return title
	}
	if !last.OK {
		// A failed last result used to print "last - 0.0/s" in the success
		// color, which reads as a measurement of nothing.
		add(" " + styleBad.Render("last failed"))
		return title
	}
	rate := styleOK.Render(fmtRate(last.TokPS) + " tok/s")
	add(" " + dim("last") + " " + fmtMs(last.TTFTms) + " " + rate)
	add(" " + rate)
	return title
}

// probeModelMin is the fewest cells a model name is worth on a probe row: a
// shorter cut is an ellipsis that names nothing, and the measurement beside it
// is what the panel exists to show.
const probeModelMin = 6

// probeOutcome is what a probe row has to say: the measured rate and time to
// first token, or the reason the probe did not land. It is measured before the
// model name, which takes what is left and is dropped rather than pushed the
// measurement off the right edge. A column too narrow for the spelled-out
// form drops the unit labels rather than the numbers: the PROBES title and the
// plain report both spell them out, and the panel is the one place the two
// measurements are side by side.
func probeOutcome(p core.ProbeSample, w int) string {
	if !p.OK {
		// Match the plain frame and ENGINES: name the failure and keep the
		// reason. Printing 0.0/s in the same shape as a success hides why the
		// probe did not land.
		out := styleBad.Render("failed")
		if msg := strings.TrimSpace(core.SanitizeText(p.Err)); msg != "" {
			out += " " + dim(shorten(msg, max(w-24, 8)))
		}
		return out
	}
	full := fmtRate(p.TokPS) + " tok/s " + dim("ttft") + " " + fmtMs(p.TTFTms)
	if lipgloss.Width(full) > w {
		return fmtRate(p.TokPS) + "/s " + fmtMs(p.TTFTms)
	}
	return full
}

func (m Model) probesBody(w, h int) string {
	vals := probeSeries(m.snap, w, m.chartCadence())
	chartH := min(max(h-3-len(m.snap.Providers), 2), 8)
	var out strings.Builder
	out.WriteString(BrailleChart(vals, w, chartH, ChartStyle{Heat: heatColor}) + "\n")
	shown := 0
	for i := len(m.snap.Probes) - 1; i >= 0 && shown < maxProbeRows; i-- {
		p := m.snap.Probes[i]
		outcome := probeOutcome(p, w)
		mark := styleOK.Render("✓")
		if !p.OK {
			mark = styleBad.Render("✗")
		}
		line := mark + " " + outcome
		// "✓ " and the separating space are the two cells the mark spends
		// before the measurement starts.
		if cells := w - 2 - lipgloss.Width(outcome) - 1; cells >= probeModelMin {
			line = mark + " " + styleDim.Render(shorten(core.SingleLine(p.Model), cells)) + " " + outcome
		}
		out.WriteString(clip(line, w) + "\n")
		shown++
	}
	if len(m.snap.Probes) == 0 {
		// Short enough for the narrowest legal pane's PROBES column, which is
		// the 31% the engine state column does not take: 62 - 38 - 31 = 20
		// cells, 16 of them body width. The longer spellings ended in an
		// ellipsis on every narrow dashboard, so the one instruction the empty
		// panel has was the one thing that could not be read.
		out.WriteString(dim("press ") + styleInfo.Render("p") + dim(" to probe") + "\n")
		// The automatic route spelled as a sentence where the column has room
		// for one, the way the empty setup card spells the same flags: "quit,
		// --probe N" alone reads as two unrelated words on a column this narrow.
		// Longest form first, the first that fits wins.
		forms := []string{
			"quit, re-run with --probe N",
			"re-run with --probe N",
			"quit, --probe N",
		}
		fit := forms[len(forms)-1]
		for _, form := range forms {
			if widthOf(form) <= w {
				fit = form
				break
			}
		}
		out.WriteString(dim(shorten(fit, w)) + "\n")
	}
	return out.String()
}
