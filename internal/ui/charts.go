package ui

// Throughput and probe charts: series extraction, cadence and block
// compression for the braille panel View draws.

import (
	"math"
	"math/bits"
	"slices"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

func (m Model) renderCharts() string {
	w := m.w - 4 // panel borders + padding
	cad := m.chartCadence()
	outH, _, _ := m.sectionHeights()
	agg, grid := m.outSeries(w, cad)
	out := panel(
		m.throughputTitle(),
		BrailleChart(agg, w, outH, ChartStyle{Heat: heatColor, Grid: grid}),
		w, outH,
	)
	in := panel(
		"PROMPT "+styleInfo.Render("▼ "+fmtRate(aggInAt(m.snap, m.snapNow()))+" tok/s"),
		BrailleChart(aggHist(m.snap, false, w, cad), w, 1,
			ChartStyle{Heat: func(float64) lipgloss.Color { return cCyan }}),
		w, 1,
	)
	return out + "\n" + in
}

// chartCompressedDefault is on: recent seconds stay detailed while older
// history compresses leftward, btop-zoom style. 't' toggles it.
const chartCompressedDefault = true

// compressBlock is how many columns share the same timespan before it
// doubles moving left.
const compressBlock = 12

// outSeries produces the throughput series plus grid boundaries for the
// active timescale mode.
func (m Model) outSeries(w int, cadence time.Duration) ([]float64, map[int]bool) {
	if !m.chartCompressed {
		return aggHist(m.snap, true, w, cadence), nil
	}
	vals, bounds := compressSeries(timedSeries(m.snap, cadence), w, compressBlock)
	return vals, bounds
}

func (m Model) throughputTitle() string {
	kind := dim("  decode")
	if len(m.snap.Providers) == 0 {
		kind = dim("  output")
	}
	title := "THROUGHPUT " + styleValue.Foreground(heatColor(norm(m.aggLast, m.aggMax))).Render("▲ "+fmtRate(m.aggLast)+" tok/s") + kind
	// Advertise the toggle in both modes: the hint only showing while
	// compressed hid how to get back to the uniform timescale. Brackets mark
	// the key so "compressed [t]" reads as mode plus switch rather than one
	// run-together token.
	mode := dim(" · compressed ")
	if !m.chartCompressed {
		mode = dim(" · uniform ")
	}
	return title + mode + styleInfo.Render("[t]")
}

// timedVal is one sample with its absolute timestamp, its rate, and the
// engine it came from.
type timedVal struct {
	at     time.Time
	rate   float64
	engine int
}

// timedSeries flattens every provider's history onto absolute timestamps
// spaced one cadence apart, tagging each sample with its engine: compressed
// buckets must tell engines apart to average within one and sum across all.
//
// The slice is pre-sized: the caller replays this every frame, and growing
// it from nil reallocated per provider row.
func timedSeries(s core.Snapshot, cadence time.Duration) []timedVal {
	n := 0
	for i := range s.Providers {
		n += len(s.Providers[i].OutHist)
	}
	n += core.AgentHistoryLen + core.HistoryLen
	tv := make([]timedVal, 0, n)
	var end time.Time
	for i := range s.Providers {
		p := &s.Providers[i]
		if p.OutT0.IsZero() {
			continue
		}
		for j, v := range p.OutHist {
			t := p.OutT0.Add(time.Duration(j) * cadence)
			tv = append(tv, timedVal{at: t, rate: v, engine: i})
			if t.After(end) {
				end = t
			}
		}
	}
	if aend := agentHistEnd(s.Agents); aend.After(end) {
		end = aend
	}
	if !end.IsZero() {
		n := core.HistoryLen
		hist := agentDenseHist(s.Agents, true, end, n, cadence)
		engine := len(s.Providers)
		nonzero := false
		for _, v := range hist {
			if v > 0 {
				nonzero = true
				break
			}
		}
		if nonzero {
			start := end.Add(-time.Duration(n-1) * cadence)
			for j, v := range hist {
				tv = append(tv, timedVal{at: start.Add(time.Duration(j) * cadence), rate: v, engine: engine})
			}
		}
	}
	slices.SortFunc(tv, func(a, b timedVal) int { return a.at.Compare(b.at) })
	return tv
}

// compressSeries maps samples onto w columns whose covered timespan doubles
// every `block` columns moving away from the newest sample: right edge shows
// per-cadence detail, the far left packs hours. bounds marks where each
// coarser block begins so charts can draw faint separators.
//
// Aggregation matches uniform mode (aggHist) and the aggregate the panel
// title prints: engines sum. Within one engine, samples sharing a coarse
// bucket average into that span's mean rate. Dividing a bucket by its total
// sample count instead would scale the chart down by the engine count and
// make the two timescale modes disagree about what a column means.
func compressSeries(tv []timedVal, w, block int) ([]float64, map[int]bool) {
	if len(tv) == 0 || w <= 0 || block <= 0 {
		return nil, nil
	}
	end := tv[len(tv)-1].at
	spans := make([]time.Duration, w)
	total := time.Duration(0)
	maxLevel := spanCap(w)
	for j := range w { // j=0 oldest … w-1 newest
		level := min((w-1-j)/block,
			// wider shifts would overflow the span sums
			maxLevel)
		spans[j] = time.Second << level
		total += spans[j]
	}

	bounds := map[int]bool{}
	for j := range w {
		if (w-1-j)%block == 0 && j < w-1 {
			bounds[j] = true
		}
	}

	nEngines := 0
	for _, sample := range tv {
		nEngines = max(nEngines, sample.engine+1)
	}
	// cum[j] is the age of bucket j's newest edge, cum[0] = 0. The linear
	// scan this replaces kept the first bucket with offset >= e[j+1],
	// i.e. the smallest j with cum[j+1] >= total-offset; the binary search
	// below tests exactly that predicate, so bucketing is unchanged while
	// the per-sample walk drops from O(w) to O(log w).
	cum := make([]time.Duration, w+1)
	for j := range w {
		cum[j+1] = cum[j] + spans[j]
	}
	// Per-engine bucket sums and counts; rows materialize only for buckets
	// samples actually land in.
	sums := make([][]float64, w)
	cnts := make([][]int, w)
	for _, sample := range tv {
		offset := end.Sub(sample.at)
		if offset < 0 || offset >= total {
			continue
		}
		x := total - offset
		j := sort.Search(w, func(j int) bool { return cum[j+1] >= x })
		if j >= w {
			j = w - 1
		}
		if sums[j] == nil {
			sums[j] = make([]float64, nEngines)
			cnts[j] = make([]int, nEngines)
		}
		sums[j][sample.engine] += sample.rate
		cnts[j][sample.engine]++
	}
	grid := make([]float64, w)
	for j := range grid {
		for e, cnt := range cnts[j] { // nil row for empty buckets: loop body skipped
			if cnt > 0 {
				grid[j] += sums[j][e] / float64(cnt)
			}
		}
	}
	return grid, bounds
}

// spanCap is the largest shift keeping w spans, and thus the whole summed
// window, inside a time.Duration: past it the leftward timescale stops
// doubling instead of wrapping negative and collapsing the chart's buckets.
func spanCap(w int) int {
	if w <= 0 || uint64(w) > uint64(math.MaxInt64)/uint64(time.Second) {
		return 0
	}
	maxLevel := 63 - bits.Len64(uint64(w)*uint64(time.Second))
	if maxLevel < 0 {
		return 0
	}
	return maxLevel
}

// chartCadence is the sampling interval charts are drawn at.
func (m Model) chartCadence() time.Duration {
	if m.cfg.PollEvery > 0 {
		return m.cfg.PollEvery
	}
	return time.Second
}
