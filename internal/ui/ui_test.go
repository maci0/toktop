package ui

import (
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

func TestClosedSnapshotChannelStopsScheduling(t *testing.T) {
	ch := make(chan core.Snapshot, 1)
	ch <- core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "engine", OK: true}}}
	close(ch)
	m := New(Config{Version: "t"}, ch)

	next, cmd := m.Update(waitSnap(ch)())
	m = next.(Model)
	if len(m.snap.Providers) != 1 || m.snap.Providers[0].Label != "engine" {
		t.Fatal("buffered snapshot was not delivered")
	}
	if cmd == nil {
		t.Fatal("snapshot delivery did not schedule the next read")
	}
	next, again := m.Update(cmd())
	if again != nil {
		t.Fatal("closed snapshot channel scheduled another read")
	}
	m = next.(Model)
	if len(m.snap.Providers) != 1 || m.snap.Providers[0].Label != "engine" {
		t.Fatal("closed snapshot channel replaced the last snapshot")
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// The key map is the product surface: q quits (help first), space freezes the
// stream so incoming snapshots are dropped rather than merged, p fires
// probes, t flips the timescale, and a paused frame must advertise itself.
func TestUpdateKeyMap(t *testing.T) {
	var probes atomic.Int32
	m := New(Config{Version: "t", Prober: func() { probes.Add(1) }}, nil)
	key := func(s string) tea.Cmd {
		nm, cmd := m.Update(keyMsg(s))
		m = nm.(Model)
		return cmd
	}
	sendSnap := func(label string) {
		nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{{Label: label}}}))
		m = nm.(Model)
	}

	if key("q") == nil {
		t.Error("q must return a quit command")
	}

	key("?")
	if !m.help {
		t.Fatal("? did not open help")
	}
	if key("q") != nil {
		t.Error("q with help open must close help, not quit")
	}
	if m.help {
		t.Error("q with help open did not close help")
	}
	key("?")
	if !m.help {
		t.Fatal("? did not reopen help")
	}
	// Help is a full-screen replacement view: it must actually render its
	// content, not just flip the flag.
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.View())
	if !strings.Contains(out, "pause / resume streaming") {
		t.Errorf("help view missing key rows:\n%s", out)
	}
	// Every key the footer advertises must be documented here too: t was
	// missing for a long time and users had no discoverable path back from
	// the compressed timescale.
	for _, want := range []string{"compressed timescale", "esc"} {
		if !strings.Contains(out, want) {
			t.Errorf("help view missing %q:\n%s", want, out)
		}
	}
	if key("esc") != nil {
		t.Error("esc with help open must close help, not quit")
	}
	if m.help {
		t.Error("esc with help open did not close help")
	}

	if key(" "); !m.paused {
		t.Fatal("space did not pause")
	}
	sendSnap("late")
	if len(m.snap.Providers) != 0 {
		t.Error("paused model absorbed a snapshot")
	}
	key(" ")
	if m.paused {
		t.Fatal("second space did not resume")
	}
	sendSnap("late")
	if len(m.snap.Providers) != 1 || m.snap.Providers[0].Label != "late" {
		t.Error("resumed model dropped the snapshot")
	}

	before := m.chartCompressed
	key("t")
	if m.chartCompressed == before {
		t.Error("t did not toggle the timescale")
	}

	n := probes.Load()
	key("p") // the prober fires on its own goroutine
	deadline := time.Now().Add(time.Second)
	for probes.Load() == n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := probes.Load(); got != n+1 {
		t.Errorf("p fired %d probes, want 1", got-n)
	}

	m.paused = true
	m.w, m.h, m.ready, m.clock = 110, 36, true, time.Now()
	if out := m.View(); !strings.Contains(strip(out), "PAUSED") {
		t.Error("paused frame lacks PAUSED badge")
	}
}

// Help covers the whole screen: keys that act on the dashboard behind it
// must be inert while it is up. Otherwise a keyboard user reading help
// silently pauses (space) or fires real probe generations (p) with no
// feedback until the overlay is dismissed.
func TestHelpOverlayMutesActionKeys(t *testing.T) {
	synctest.Test(t, testHelpOverlayMutesActionKeys)
}

func testHelpOverlayMutesActionKeys(t *testing.T) {
	var probes atomic.Int32
	m := New(Config{Version: "t", Prober: func() { probes.Add(1) }}, nil)
	key := func(s string) tea.Cmd {
		nm, cmd := m.Update(keyMsg(s))
		m = nm.(Model)
		return cmd
	}

	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "engine", OK: true}},
	}))
	m = nm.(Model)

	key("?")
	if !m.help {
		t.Fatal("? did not open help")
	}
	key(" ")
	if m.paused {
		t.Error("space acted while help was open")
	}
	before := m.chartCompressed
	key("t")
	if m.chartCompressed != before {
		t.Error("t toggled the timescale while help was open")
	}
	for _, k := range []string{"p", "P"} {
		key(k)
		synctest.Wait()
		if got := probes.Load(); got != 0 {
			t.Errorf("%s fired %d probes while help was open", k, got)
		}
		if !m.probeReq.IsZero() {
			t.Errorf("%s set the probing marker while help was open", k)
		}
	}
	if key("esc") != nil {
		t.Error("esc with help open must close help, not quit")
	}
	if m.help {
		t.Fatal("esc did not close help")
	}
	// Dismissed, the same keys work again.
	if key(" "); !m.paused {
		t.Error("space did not pause after help closed")
	}
	for i, k := range []string{"p", "P"} {
		key(k)
		synctest.Wait()
		if got := probes.Load(); got != int32(i+1) {
			t.Errorf("%s after help closed: probes = %d, want %d", k, got, i+1)
		}
		if m.probeReq.IsZero() {
			t.Errorf("%s did not set the probing marker after help closed", k)
		}
	}
}

// Terminals below the minimum geometry degrade to a one-line-per-engine
// strip; it must still carry each engine's label and live rate.
func TestMinimalViewRendersRates(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "ollama", OK: true, OutTokPS: 42},
	}}))
	m = nm.(Model)
	m.w, m.h, m.ready = 40, 10, true
	out := strip(m.View())
	if !strings.Contains(out, "ollama") || !strings.Contains(out, "42") || !strings.Contains(out, "tok/s") {
		t.Errorf("minimal view missing engine rate:\n%s", out)
	}
}

// space pauses in the compact strip too: without an in-view badge the frozen
// rates are indistinguishable from a stalled feed.
func TestMinimalViewShowsPaused(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.paused = true
	m.w, m.h, m.ready = 40, 10, true
	if out := strip(m.View()); !strings.Contains(out, "PAUSED") {
		t.Errorf("paused minimal view lacks PAUSED badge:\n%s", out)
	}
}

// A down engine cannot be signaled by the dot's color alone (WCAG 1.4.1):
// the minimal strip must say "down" and keep the error reason visible.
func TestMinimalViewNamesDownEngines(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "vllm", OK: false, Err: "connection refused"},
	}}))
	m = nm.(Model)
	m.w, m.h, m.ready = 40, 10, true
	out := strip(m.View())
	if !strings.Contains(out, "vllm") || !strings.Contains(out, "down") || !strings.Contains(out, "connection refused") {
		t.Errorf("minimal view hides engine failure:\n%s", out)
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("minimal view marks down engines with color-only ●, not ✗:\n%s", out)
	}
}

// The pre-ready frame must name itself, and its status marker comes from the
// dashboard's monochrome glyph set (●), not a color emoji.
func TestWarmupFrameUsesStatusGlyphs(t *testing.T) {
	m := New(Config{Version: "t"}, nil) // ready stays false until WindowSizeMsg
	out := strip(m.View())
	if !strings.Contains(out, "warming up") {
		t.Errorf("warmup frame does not say what it is doing:\n%s", out)
	}
	if strings.ContainsRune(out, '⏳') {
		t.Errorf("warmup frame uses an emoji instead of the status glyphs:\n%s", out)
	}
}

// The header clock is the viewer's wall time: a snapshot stamp (or tick)
// may carry UTC or a sender offset, and Format without Local would print
// that zone's hour instead of the operator's.
func TestHeaderClockRendersInLocalZone(t *testing.T) {
	at := time.Date(2026, 8, 24, 23, 30, 5, 0, time.FixedZone("sender", 5*3600+1800))
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 110, 36, true
	m.clock = at
	m.snap = core.Snapshot{
		At: at,
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, OK: true, OutTokPS: 10,
		}},
	}
	out := strip(m.View())
	want := at.Local().Format("15:04:05")
	if !strings.Contains(out, want) {
		t.Fatalf("header clock missing local %q:\n%s", want, out)
	}
	if foreign := at.Format("15:04:05"); foreign != want && strings.Contains(out, foreign) {
		t.Fatalf("header clock still in stamp zone %q:\n%s", foreign, out)
	}
}

// feedLine must render event timestamps in the viewer's zone: ingest events
// carry sender-supplied RFC 3339 stamps whose offset (or absent offset,
// decoded as UTC) is otherwise shown as-is.
func TestFeedLineRendersEventTimeInLocalZone(t *testing.T) {
	at := time.Date(2026, 8, 24, 23, 30, 5, 0, time.FixedZone("sender", 5*3600+1800))
	ev := core.AgentEvent{At: at, Agent: "ci-bot", Kind: "turn"}
	line := strip(feedLine(ev))
	want := at.Local().Format("15:04:05")
	if !strings.Contains(line, want) {
		t.Fatalf("feedLine time = line %q, want it to show local %q", line, want)
	}
}

// Header rates are ▲ out / ▼ in. Feed counts used the opposite arrows, so
// one screen taught two directions for the same quantities.
func TestFeedLineTokenArrowsMatchHeader(t *testing.T) {
	line := strip(feedLine(core.AgentEvent{
		At: time.Now(), Agent: "claude", Kind: "turn",
		PromptTokens: 4200, OutputTokens: 310,
	}))
	if !strings.Contains(line, "▲310") || !strings.Contains(line, "▼4.2k") {
		t.Errorf("feed line arrows = %q, want ▲ output then ▼ prompt", line)
	}
	if strings.Contains(line, "↑") || strings.Contains(line, "↓") {
		t.Errorf("feed line still uses the old ↑↓ pair: %q", line)
	}
}

func TestGaugeBar(t *testing.T) {
	g := GaugeBar(50, 10, kvHeat)
	if !strings.Contains(g, "50%") {
		t.Errorf("missing label: %q", g)
	}
	if w := lipgloss.Width(GaugeBar(50, 10, kvHeat)); w != 14 { // 10 + space + "50%"
		t.Errorf("width = %d, want 14", w)
	}
	if w := lipgloss.Width(GaugeBar(120, 10, kvHeat)); w != 15 { // clamped label "100%"
		t.Errorf("overflow width = %d, want 15", w)
	}
	if lipgloss.Width(GaugeBar(-5, 10, kvHeat)) != 13 { // clamps to "0%"
		t.Error("negative pct not clamped")
	}
	// A NaN gauge (a 0/0 upstream, or a sensor that slipped past the parse
	// filters) must render as an empty bar: int(NaN) is implementation-
	// defined and fed strings.Repeat a huge negative count on amd64.
	if got := GaugeBar(math.NaN(), 10, kvHeat); !strings.Contains(got, "0%") {
		t.Errorf("NaN pct = %q, want it clamped to 0%%", strip(got))
	}
}

func TestProcLineVRAMSumSaturates(t *testing.T) {
	got := procLine(core.ProviderSnapshot{
		Models: []core.ModelInfo{
			{Name: "a", SizeVRAM: ^uint64(0)},
			{Name: "b", SizeVRAM: 1 << 30},
		},
	})
	// MaxUint64 + 1GiB wraps to ~1GiB. Saturated sum formats as a huge GiB figure.
	if strings.Contains(got, "MiB") {
		t.Fatalf("VRAM sum wrapped to a small readout: %q", got)
	}
	if !strings.Contains(got, "GiB") {
		t.Fatalf("expected saturated GiB readout, got %q", got)
	}
}

func TestProcLineCtxCountFitsInt64(t *testing.T) {
	got := strip(procLine(core.ProviderSnapshot{
		Models: []core.ModelInfo{{Name: "a", CtxMax: 1 << 63}},
	}))
	if strings.Contains(got, "-") {
		t.Fatalf("uint64 CtxMax printed as a negative int64: %q", got)
	}
	if strings.Contains(got, "0M") {
		t.Fatalf("ctx count collapsed to the old byte-estimate zero: %q", got)
	}
	if !strings.Contains(got, "ctx") || !strings.Contains(got, "tok") {
		t.Fatalf("expected ctx token count, got %q", got)
	}
}

// BrailleChart fills a fixed pane: a degenerate size is empty, and a real
// size occupies exactly h rows of w cells even when the series is all zeros.
func TestBrailleChartPane(t *testing.T) {
	st := ChartStyle{Heat: kvHeat}
	if BrailleChart([]float64{1, 2, 3}, 0, 4, st) != "" {
		t.Fatal("zero width must render nothing")
	}
	if BrailleChart([]float64{1, 2, 3}, 8, 0, st) != "" {
		t.Fatal("zero height must render nothing")
	}
	out := BrailleChart([]float64{0, 1, 2, 3}, 8, 3, st)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("rows = %d, want 3", len(lines))
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w != 8 {
			t.Errorf("row %d width = %d, want 8", i, w)
		}
	}
	z := BrailleChart([]float64{0, 0, 0}, 4, 2, st)
	zlines := strings.Split(z, "\n")
	if z == "" || len(zlines) != 2 {
		t.Fatalf("zero series = %q, want a 2-row pane", z)
	}
	for i, line := range zlines {
		if w := lipgloss.Width(line); w != 4 {
			t.Errorf("zero series row %d width = %d, want 4", i, w)
		}
	}
}

func TestShortenAndClip(t *testing.T) {
	if got := shorten("abcdef", 4); got != "abc…" {
		t.Errorf("shorten = %q", got)
	}
	if shorten("ab", 0) != "" || shorten("ab", 1) != "…" {
		t.Error("degenerate widths mishandled")
	}
	styled := lipgloss.NewStyle().Foreground(cRed).Render("hello world")
	if got := clip(styled, 6); got != "hello…" {
		t.Errorf("clip styled = %q", got)
	}
	// Wide glyphs count two cells: the result must never render wider than n.
	wide := "世界世界世界"
	if got := shorten(wide, 5); lipgloss.Width(got) > 5 {
		t.Errorf("shorten wide = %q renders %d cells, want <= 5", got, lipgloss.Width(got))
	}
	if got := clip(wide, 6); lipgloss.Width(got) > 6 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip wide = %q (width %d)", got, lipgloss.Width(got))
	}
}

func TestFmtRateAndCount(t *testing.T) {
	if fmtRate(1234.5) != "1.2k" || fmtRate(42.3) != "42.3" || fmtRate(250) != "250" ||
		fmtRate(15000) != "15k" {
		t.Error("fmtRate drift")
	}
	if fmtCount(999) != "999" || fmtCount(2000) != "2.0k" || fmtCount(3_400_000) != "3.4M" {
		t.Error("fmtCount drift")
	}
	if fmtMs(210.4) != "210ms" || fmtMs(1400) != "1.40s" || fmtMs(0) != "-" {
		t.Error("fmtMs drift")
	}
	if fmtRate(math.NaN()) != "0.0" || fmtRate(math.Inf(1)) != "0.0" {
		t.Error("fmtRate must not print NaN/Inf")
	}
	if fmtMs(math.NaN()) != "-" || fmtMs(math.Inf(1)) != "-" {
		t.Error("fmtMs must not print NaN/Inf")
	}
}

func TestAggHistTimeAligned(t *testing.T) {
	// Provider B joins 3 cadences after A: tail-index alignment would smear
	// the window; absolute-time alignment must not.
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutT0: t0, OutHist: []float64{1, 1, 1, 1, 1, 1}},             // samples at t0..t0+5
		{OutT0: t0.Add(3 * time.Second), OutHist: []float64{2, 2, 2}}, // t0+3..t0+5
	}}
	got := aggHist(s, true, 6, time.Second)
	want := []float64{1, 1, 1, 3, 3, 3}
	if len(got) != len(want) {
		t.Fatalf("aggHist len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aggHist[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

// An engine that stopped reporting must leave its buckets empty rather than
// dragging the whole window leftward.
func TestAggHistIgnoresStaleEngine(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * time.Second)
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutT0: old, OutHist: []float64{9, 9}},
		{OutT0: now.Add(-2 * time.Second), OutHist: []float64{4, 4}},
	}}
	got := aggHist(s, true, 4, time.Second)
	// window = [now-3s .. now]; the stale engine's last sample is 29s old
	want := []float64{0, 0, 4, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// At w=500 the doubling spans overflow a time.Duration partway through the
// accumulation: total wraps, the bucket walk collapses, and every sample
// bunches into one middle column leaving the rest of the chart empty.
func TestCompressSeriesWideTerminalKeepsSamples(t *testing.T) {
	const w = 500 // wide enough that naive doubling overflows
	end := time.Unix(1_000_000_000, 0)
	tv := []timedVal{
		{at: end.Add(-300 * time.Second), rate: 7},
		{at: end.Add(-time.Second), rate: 1},
	}
	grid, bounds := compressSeries(tv, w, compressBlock)
	if len(grid) != w || len(bounds) == 0 {
		t.Fatalf("grid=%d bounds=%d", len(grid), len(bounds))
	}
	sum := 0.0
	for _, g := range grid {
		sum += g
	}
	if sum != 8 {
		t.Fatalf("samples lost or bunched on wide chart: sum=%v", sum)
	}
	if grid[w-1] != 1 {
		t.Fatalf("newest sample misplaced: grid[w-1]=%v", grid[w-1])
	}
}

// Compressed columns are fleet aggregates: engines reporting at the same
// instant must sum, or the chart reads N times below the aggregate its own
// panel title prints (uniform mode and aggHist both sum).
func TestCompressSeriesSumsAcrossEngines(t *testing.T) {
	end := time.Unix(1_000_000_000, 0)
	tv := []timedVal{
		{at: end.Add(-time.Second), rate: 100, engine: 0},
		{at: end.Add(-time.Second), rate: 200, engine: 1},
	}
	grid, _ := compressSeries(tv, 24, compressBlock)
	if got := grid[len(grid)-1]; got != 300 {
		t.Fatalf("newest column = %v, want 300 (engines sum, not average)", got)
	}
}

// Time downsampling still averages within one engine: several cadences share
// a coarse bucket, and their mean is that engine's rate over the span.
func TestCompressSeriesAveragesWithinEngine(t *testing.T) {
	end := time.Unix(1_000_000_000, 0)
	tv := []timedVal{
		{at: end.Add(-30 * time.Second), rate: 100, engine: 0},
		{at: end.Add(-29 * time.Second), rate: 300, engine: 0},
		{at: end.Add(-time.Second), rate: 50, engine: 1},
	}
	grid, _ := compressSeries(tv, 24, compressBlock)
	var nonzero []float64
	for _, g := range grid {
		if g > 0 {
			nonzero = append(nonzero, g)
		}
	}
	if len(nonzero) != 2 || nonzero[0]+nonzero[1] != 250 {
		t.Fatalf("columns = %v, want one 200 bucket (mean of 100,300) and one 50", nonzero)
	}
}

func TestProbeSeriesStepHold(t *testing.T) {
	base := time.Now()
	s := core.Snapshot{Probes: []core.ProbeSample{
		{At: base.Add(-4 * time.Second), TokPS: 100},
		{At: base.Add(-1 * time.Second), TokPS: 300},
	}}
	got := probeSeries(s, 6, time.Second)
	// grid ends at the newest probe: buckets [-6..-1s]; probes hold forward
	want := []float64{0, 0, 100, 100, 100, 300}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probeSeries = %v, want %v", got, want)
		}
	}
	if got := probeSeries(s, 0, time.Second); got != nil {
		t.Fatalf("probeSeries(w=0) = %v, want nil", got)
	}
	if got := probeSeries(s, -5, time.Second); got != nil {
		t.Fatalf("probeSeries(w=-5) = %v, want nil", got)
	}
}

func TestSpanCapBounds(t *testing.T) {
	if got := spanCap(0); got != 0 {
		t.Errorf("spanCap(0) = %d, want 0", got)
	}
	if got := spanCap(-1); got != 0 {
		t.Errorf("spanCap(-1) = %d, want 0", got)
	}
	if got := spanCap(100); got <= 0 || got > 63 {
		t.Errorf("spanCap(100) = %d, want positive <= 63", got)
	}
	if got := spanCap(math.MaxInt); got != 0 {
		t.Errorf("spanCap(MaxInt) = %d, want 0", got)
	}
}

func TestCompressSeriesZeroBlock(t *testing.T) {
	end := time.Unix(1_000_000_000, 0)
	tv := []timedVal{{at: end, rate: 100, engine: 0}}
	if grid, bounds := compressSeries(tv, 24, 0); grid != nil || bounds != nil {
		t.Errorf("compressSeries with block=0 must return nil, got %v %v", grid, bounds)
	}
	if grid, bounds := compressSeries(tv, 24, -1); grid != nil || bounds != nil {
		t.Errorf("compressSeries with block=-1 must return nil, got %v %v", grid, bounds)
	}
}

func TestHeatFunctionsNaN(t *testing.T) {
	if got := tempColor(math.NaN()); got == cRed {
		t.Errorf("tempColor(NaN) returned cRed")
	}
	if got := memHeat(math.NaN()); got == cRed {
		t.Errorf("memHeat(NaN) returned cRed")
	}
	if got := kvHeat(math.NaN()); got == cRed {
		t.Errorf("kvHeat(NaN) returned cRed")
	}
}

func TestStaticFrameRenders(t *testing.T) {
	snap := core.Snapshot{
		At:     time.Now(),
		Uptime: time.Minute,
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, Addr: "http://127.0.0.1:11434", OK: true,
			Models:   []core.ModelInfo{{Name: "llama3"}},
			OutTokPS: 42, InTokPS: 100, KVPct: 55,
			OutHist: []float64{1, 2, 3, 4},
			InHist:  []float64{2, 4, 6, 8},
		}},
		Agents: []core.AgentEvent{{At: time.Now(), Agent: "tester", Kind: "turn",
			PromptTokens: 10, OutputTokens: 5}},
	}
	out := StaticFrame(Config{Version: "t"}, snap, 110, 36)
	for _, want := range []string{"TOKTOP", "ENGINES", "ENGINE STATE", "PROBES", "AGENT FEED", "SYS", "llama3"} {
		if !strings.Contains(strip(out), want) {
			t.Errorf("frame missing %q", want)
		}
	}
}

func TestStaticFrameSystemStrip(t *testing.T) {
	snap := core.Snapshot{
		At: time.Now(),
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, OK: true,
			Models: []core.ModelInfo{{Name: "llama3"}},
		}},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			SwapTotal: 8 << 30, SwapUsed: 1 << 30,
			Load1: 1.5, Load5: 1.2, Load15: 0.9,
			CPUModel: "Test CPU",
			Temps: []core.TempReading{
				{Label: "package", MilliC: 64000},
			},
			GPUs: []core.GPUDevice{
				{Vendor: "nvidia", Index: 0, Name: "A100", MilliC: 71000,
					MemTotal: 80 << 30, MemUsed: 20 << 30, UtilPct: 42, PowerW: 310},
				{Vendor: "amd", Index: 0, Name: "MI210", MilliC: 55000,
					MemTotal: 64 << 30, MemUsed: 8 << 30},
			},
		},
	}
	out := StaticFrame(Config{Version: "t"}, snap, 110, 34)
	plain := strip(out)
	for _, want := range []string{"SYS", "mem ", "50%", "swp ", "ld ",
		"nv0 71°", "42%", "20G/80G", "310W"} {
		if !strings.Contains(plain, want) {
			t.Errorf("system strip missing %q in:\n%s", want, plain)
		}
	}
	// wider terminals fit the second GPU and CPU temps too
	wide := strip(StaticFrame(Config{Version: "t"}, snap, 170, 34))
	for _, want := range []string{"amd0 55°", "8.0G/64G", "64°"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide strip missing %q in:\n%s", want, wide)
		}
	}
}

// GPU temps from hwmon must be suppressed once vendor GPU devices exist.
func TestSystemStripSuppressesHwmonGPUDupes(t *testing.T) {
	snap := core.Snapshot{
		At:        time.Now(),
		Providers: []core.ProviderSnapshot{{Label: "x", Kind: core.KindOllama, OK: true}},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			Temps: []core.TempReading{
				{Label: "edge", MilliC: 71000, IsGPU: true}, // would duplicate nv0
				{Label: "Tctl", MilliC: 66000},
			},
			GPUs: []core.GPUDevice{{Vendor: "nvidia", Index: 0, MilliC: 70000}},
		},
	}
	out := strip(StaticFrame(Config{Version: "t"}, snap, 110, 34))
	if strings.Contains(out, "edge") {
		t.Errorf("hwmon GPU temp leaked into strip:\n%s", out)
	}
	if !strings.Contains(out, "nv0 70°") || !strings.Contains(out, "66°") {
		t.Errorf("expected nv GPU seg + cpu temp:\n%s", out)
	}
}

func TestStaticFrameNoSensors(t *testing.T) {
	snap := core.Snapshot{
		At:        time.Now(),
		Providers: []core.ProviderSnapshot{{Label: "x", Kind: core.KindOllama, OK: true}},
		Sys:       &core.SysSample{},
	}
	out := strip(StaticFrame(Config{Version: "t"}, snap, 110, 34))
	if !strings.Contains(out, "mem n/a") || !strings.Contains(out, "no sensors found") {
		t.Errorf("empty sys sample not handled:\n%s", out)
	}
}

// With GPU devices present, hwmon GPU readings are filtered out of the CPU
// temp list (they already render as GPU segments). The "+N more" overflow
// marker must then still appear when the remaining CPU temps alone exceed
// the four shown, counting only the filtered list.
func TestSystemStripCountsFilteredTempsInMore(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "x", Kind: core.KindOllama, OK: true}},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			Temps: []core.TempReading{
				{Label: "cpu1", MilliC: 50000},
				{Label: "cpu2", MilliC: 51000},
				{Label: "cpu3", MilliC: 52000},
				{Label: "cpu4", MilliC: 53000},
				{Label: "cpu5", MilliC: 54000},
				{Label: "cpu6", MilliC: 55000},
				{Label: "edge", MilliC: 71000, IsGPU: true}, // filtered: renders as nv0
			},
			GPUs: []core.GPUDevice{{Vendor: "nvidia", Index: 0, MilliC: 70000}},
		},
	}
	m.w, m.h, m.ready = 170, 40, true
	out := strip(m.renderSystem())
	if !strings.Contains(out, "+2 more") {
		t.Errorf("strip = %q, want exactly the two hidden cpu temps counted as +2 more:\n%s",
			out, out)
	}
}

// The timescale toggle lives in the chart title; both modes must show the
// current one plus a clearly delimited key, not a run-together "←t".
func TestThroughputTitleAdvertisesTimescaleToggle(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	if got := strip(m.throughputTitle()); !strings.Contains(got, "compressed") || !strings.Contains(got, "[t]") {
		t.Errorf("compressed title = %q, want mode word plus [t] switch", got)
	}
	m.chartCompressed = false
	if got := strip(m.throughputTitle()); !strings.Contains(got, "uniform") || !strings.Contains(got, "[t]") {
		t.Errorf("uniform title = %q, want mode word plus [t] switch", got)
	}
}

// The help screen is the only in-app reference: both ways of pointing
// toktop at engines away from localhost must be discoverable there.
func TestHelpCoversAttachModes(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help = true
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.View())
	for _, want := range []string{"ssh://host", "--agents"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

// An empty AGENT FEED may promise automatic pickup only when --agents is on;
// otherwise it must name the knob (or the POST target) that fills it.
func TestFeedEmptyStateGuidesByMode(t *testing.T) {
	view := func(cfg Config) string {
		m := New(cfg, nil)
		m.w, m.h, m.ready = 110, 36, true
		return strip(m.renderFeed())
	}
	if out := view(Config{Version: "t", Agents: true}); !strings.Contains(out, "picked up automatically") {
		t.Errorf("--agents on, but the feed does not say so:\n%s", out)
	}
	out := view(Config{Version: "t", IngestAddr: "127.0.0.1:8420"})
	if !strings.Contains(out, "point your harness") {
		t.Errorf("ingest-only run lost the POST hint:\n%s", out)
	}
	if strings.Contains(out, "picked up automatically") {
		t.Errorf("feed promises automatic pickup with --agents off:\n%s", out)
	}
	if out := view(Config{Version: "t"}); !strings.Contains(out, "--agents") {
		t.Errorf("nothing watching: feed must point at --agents:\n%s", out)
	}
}

func TestStaticFrameEmptyState(t *testing.T) {
	out := StaticFrame(Config{Version: "t"}, core.Snapshot{}, 90, 30)
	plain := strip(out)
	if !strings.Contains(plain, "no inference engines detected") {
		t.Error("empty state hint missing")
	}
	// Every recovery path is named: attaching an endpoint, agent watching,
	// and the zero-setup demo. Flags are a re-run, so q must be visible.
	for _, want := range []string{"--add", "--agents", "--demo", "q quit"} {
		if !strings.Contains(plain, want) {
			t.Errorf("empty state missing %q recovery hint", want)
		}
	}
}

func TestEmptyStateNamesIngestDown(t *testing.T) {
	for _, width := range []int{minDashW, 90, 110} {
		for _, agents := range []bool{false, true} {
			m := New(Config{Version: "t", Agents: agents, IngestAddr: "127.0.0.1:8420"}, nil)
			m.w, m.h, m.ready = width, minDashH, true
			nm, _ := m.Update(feedDownMsg("http: Server closed"))
			m = nm.(Model)
			out := strip(m.View())
			for _, want := range []string{"ingest stopped", "http: Server closed", "restart toktop", "q quit"} {
				if !strings.Contains(out, want) {
					t.Errorf("width %d, agents %t: missing %q:\n%s", width, agents, want, out)
				}
			}
			if strings.Contains(out, "127.0.0.1:8420") {
				t.Errorf("dead endpoint still advertised:\n%s", out)
			}
			if lipgloss.Width(out) > width || lipgloss.Height(out) > m.h {
				t.Errorf("failure frame exceeds %dx%d", width, m.h)
			}
		}
	}
}

// space still pauses on the setup card: without a badge, discovery later
// finding an engine looks like the dashboard died.
func TestEmptyStateShowsPaused(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready, m.paused = 90, 30, true, true
	if out := strip(m.View()); !strings.Contains(out, "PAUSED") {
		t.Errorf("paused empty state lacks PAUSED badge:\n%s", out)
	}
}

// Empty-state copy must match how toktop was actually started: do not tell
// someone already on --agents to pass it again, and name the ingest
// endpoint that is already listening (the only next action that does not
// need a restart).
func TestEmptyStateGuidesByMode(t *testing.T) {
	view := func(cfg Config) string {
		m := New(cfg, nil)
		m.w, m.h, m.ready = 90, 32, true
		return strip(m.View())
	}
	on := view(Config{Version: "t", Agents: true})
	if !strings.Contains(on, "AGENTS") || !strings.Contains(on, "waiting for an agent to report tokens") {
		t.Errorf("--agents run without engines should show the agents dashboard:\n%s", on)
	}
	if strings.Contains(on, "no inference engines detected") {
		t.Errorf("--agents run still complains about missing engines:\n%s", on)
	}
	if strings.Contains(on, "toktop --agents") {
		t.Errorf("empty state still tells an --agents run to pass --agents:\n%s", on)
	}
	ingest := view(Config{Version: "t", IngestAddr: "127.0.0.1:8420"})
	if !strings.Contains(ingest, "http://127.0.0.1:8420/v1/events") {
		t.Errorf("live ingest lost from empty state:\n%s", ingest)
	}
	m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420"}, nil)
	m.w, m.h, m.ready, m.feedDown = 90, 32, true, "listener closed"
	if out := strip(m.View()); strings.Contains(out, "127.0.0.1:8420") {
		t.Errorf("dead ingest still advertised on empty state:\n%s", out)
	}
}

func TestEmptyStateFitsPane(t *testing.T) {
	cfg := Config{Version: "t", IngestAddr: "127.0.0.1:8420", Agents: true}
	for _, sz := range [][2]int{{62, 30}, {90, 30}, {110, 36}} {
		w, h := sz[0], sz[1]
		out := StaticFrame(cfg, core.Snapshot{}, w, h)
		if got := lipgloss.Height(out); got > h {
			t.Errorf("%dx%d: empty frame is %d lines, overflows pane", w, h, got)
		}
		for i, ln := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(ln); lw > w {
				t.Fatalf("%dx%d: line %d renders %d cells, want <= %d:\n%s",
					w, h, i, lw, w, ln)
			}
		}
	}
}

func TestFmtDurMinuteRollover(t *testing.T) {
	cases := map[time.Duration]string{
		42 * time.Second:                          "42s",
		5*time.Minute + 30*time.Second:            "5m30s",
		time.Hour + 2*time.Minute + 3*time.Second: "1h02m",
		3*time.Hour + 5*time.Minute:               "3h05m",
		-5 * time.Second:                          "0s",
		-2 * time.Hour:                            "0s",
	}
	for d, want := range cases {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}

// Vendor tags must cover every vendor the samplers emit (gpu.go vendorOrder,
// gpu_darwin's apple devices): an uncovered variant degrades to the anonymous
// "gpu" tag and the strip loses the vendor identity it already carries.
func TestShortVendorCoversKnownVendors(t *testing.T) {
	cases := map[string]string{
		"nvidia": "nv",
		"amd":    "amd",
		"intel":  "intel",
		"apple":  "apple",
		"acme":   "gpu", // unknown vendors stay anonymous
	}
	for v, want := range cases {
		if got := shortVendor(v); got != want {
			t.Errorf("shortVendor(%q) = %q, want %q", v, got, want)
		}
	}
}

// Every engine kind the core model defines must have a badge style; a
// missing entry silently downgrades that backend's row to dim text.
func TestKindStylesCoverCoreKinds(t *testing.T) {
	for _, k := range []string{
		core.KindOllama, core.KindVLLM, core.KindLlamaCPP, core.KindOpenAI,
		core.KindSGLang, core.KindTRTLLM, core.KindMLX, core.KindLMStudio,
		core.KindKoboldCPP, core.KindLocalAI, core.KindTGI, core.KindLiteLLM,
		core.KindGPUStack, core.KindLemonade, core.KindOmniRoute,
	} {
		if _, ok := kindStyles[k]; !ok {
			t.Errorf("kindStyles missing %q", k)
		}
	}
}

// Every documented agent-event kind must have a feed icon; a missing entry
// silently falls through to the generic dot.
func TestKindIconsCoverAgentKinds(t *testing.T) {
	for _, k := range []string{
		core.AgentKindTurn, core.AgentKindTool, core.AgentKindError, core.AgentKindNote,
	} {
		if _, ok := kindIcons[k]; !ok {
			t.Errorf("kindIcons missing %q", k)
		}
	}
}

// Sample spacing on the compressed timescale must follow the poll cadence,
// not an assumed 1s: --interval 2s covers twice the wall-clock window.
func TestTimedSeriesHonorsCadence(t *testing.T) {
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutT0: t0, OutHist: []float64{1, 2}},
	}}
	tv := timedSeries(s, 2*time.Second)
	if len(tv) != 2 {
		t.Fatalf("len = %d, want 2", len(tv))
	}
	if !tv[0].at.Equal(t0) || !tv[1].at.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("timestamps %v..%v, want %v..%v", tv[0].at, tv[1].at, t0, t0.Add(2*time.Second))
	}
}

// GPU names and driver strings can originate on a remote host (ssh vitals
// relay nvidia-smi output verbatim); they must pass the terminal sanitizer
// like every other externally sourced value in the host strip.
func TestGPUSegmentSanitizesName(t *testing.T) {
	g := core.GPUDevice{Vendor: "nvidia", Index: 0, Name: "A\x1b[31mB"} // no VRAM: name row renders
	out := gpuSegment(g)
	if strings.ContainsRune(strip(out), '\x1b') {
		t.Errorf("gpuSegment leaked escape bytes: %q", strip(out))
	}
	if !strings.Contains(strip(out), "AB") {
		t.Errorf("gpuSegment lost model name: %q", strip(out))
	}
}

func TestHostSegmentsSanitizeDrivers(t *testing.T) {
	sy := &core.SysSample{
		Drivers: map[string]string{"nv\x1b]0;title": "5\x1b[35m50"},
		NPUs:    []string{"ane\x1b]52;c;QUJD\x07"},
	}
	segs := hostSegments(sy)
	for _, s := range segs {
		if strings.ContainsRune(strip(s), '\x1b') || strings.ContainsRune(strip(s), '\x07') {
			t.Errorf("hostSegments leaked escape bytes: %q", strip(s))
		}
	}
	joined := strip(strings.Join(segs, " "))
	if !strings.Contains(joined, "ane") {
		t.Errorf("hostSegments lost NPU name: %q", joined)
	}
}

// busySnap packs long labels, identity data, sensors and history so layout
// tests exercise near-worst-case line widths and heights.
func busySnap() core.Snapshot {
	return core.Snapshot{
		At: time.Now(), Uptime: 90 * time.Second,
		Providers: []core.ProviderSnapshot{
			{Label: "ollama", Kind: core.KindOllama, OK: true,
				Models: []core.ModelInfo{{Name: "llama3:8b-instruct-q5_K_M"}}, Version: "0.12.1",
				OutTokPS: 42.7, InTokPS: 1200.5, KVPct: 55, Running: 1, Waiting: 2,
				OutT0: time.Now(), InT0: time.Now(),
				OutHist: []float64{1, 2, 3}, InHist: []float64{1, 2, 3}},
			{Label: "vllm", Kind: core.KindVLLM, OK: false, Err: "connection refused"},
		},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30, CPUModel: "AMD Ryzen 9 7950X",
			OsName: "Fedora Linux", Kernel: "6.11.0",
			Temps: []core.TempReading{{Label: "Tctl", MilliC: 64000}},
			GPUs:  []core.GPUDevice{{Vendor: "nvidia", Index: 0, MilliC: 70000}},
		},
		Agents: []core.AgentEvent{{At: time.Now(), Agent: "ci-bot", Kind: "turn",
			PromptTokens: 10, OutputTokens: 5}},
	}
}

// The frame must fit its pane exactly: bubbletea clips overflow from the
// top, which hides the header, and any line wider than the pane wraps and
// drags every later row out of alignment.
func TestFrameFitsPaneAtCommonSizes(t *testing.T) {
	for _, sz := range [][2]int{{62, 30}, {70, 31}, {80, 32}, {100, 34}, {120, 38}, {160, 44}} {
		w, h := sz[0], sz[1]
		out := StaticFrame(Config{Version: "0.1.0", IngestAddr: "127.0.0.1:8420"}, busySnap(), w, h)
		if got := lipgloss.Height(out); got > h {
			t.Errorf("%dx%d: frame is %d lines, overflows pane", w, h, got)
		}
		for i, ln := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(ln); lw > w {
				t.Fatalf("%dx%d: line %d renders %d cells, want <= %d:\n%s", w, h, i, lw, w, ln)
			}
		}
	}
	// The header must survive intact on roomy panes.
	out := strip(StaticFrame(Config{Version: "t"}, busySnap(), 160, 40))
	if !strings.Contains(out, "engines") || !strings.Contains(out, "tok/s") {
		t.Errorf("full header lost on wide pane:\n%s", out)
	}
}

// Narrow panes shed decorative header segments before letting the row wrap:
// uptime goes first, then version, then inbound rate, then outbound; pinned
// segments are hard-clipped only as a last resort.
func TestFitSegmentsShedsByPriority(t *testing.T) {
	segs := []headerSeg{
		{text: "AAAA"},
		{text: "BBBB", shed: 40},
		{text: "CCCC", shed: 50},
		{text: "DDDD"},
	}
	if got := fitSegments(segs, 60); !strings.Contains(got, "CCCC") {
		t.Errorf("wide enough: nothing should be shed: %q", got)
	}
	got := fitSegments(segs, 17)
	if strings.Contains(got, "CCCC") || strings.Contains(got, "BBBB") || !strings.Contains(got, "DDDD") {
		t.Errorf("tight: want CCCC then BBBB shed first, pins kept: %q", got)
	}
	if got := fitSegments(segs, 6); lipgloss.Width(got) > 6 || !strings.HasPrefix(got, "AAAA") {
		t.Errorf("overflowing pins must hard-clip from the left: %q", got)
	}
}

// Pressing p fires generations that take seconds: the keypress must be
// acknowledged immediately, then hand over once a result lands.
func TestProbePressAcknowledgesUntilResult(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}))
	m = nm.(Model)
	m.w, m.h, m.ready, m.clock = 110, 36, true, time.Now()

	nm, _ = m.Update(keyMsg("p"))
	m = nm.(Model)
	if out := strip(m.View()); !strings.Contains(out, "probing") {
		t.Fatalf("pressing p gave no feedback:\n%s", out)
	}
	nm, _ = m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes:    []core.ProbeSample{{At: time.Now().Add(time.Second), OK: true, TokPS: 10, TTFTms: 5}},
	}))
	m = nm.(Model)
	if out := strip(m.View()); strings.Contains(out, "probing") {
		t.Errorf("probe result did not clear the pending marker:\n%s", out)
	}
}

func TestProbeStatusKeepsPanelsAligned(t *testing.T) {
	for _, width := range []int{62, 80, 110, 170} {
		for _, pending := range []bool{false, true} {
			m := New(Config{Version: "t", Prober: func() {}}, nil)
			m.w, m.h, m.ready = width, 36, true
			m.snap = core.Snapshot{
				Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
				Probes: []core.ProbeSample{{
					At: m.clock.Add(-time.Second), OK: true, Model: "llama3", TokPS: 966, TTFTms: 147,
				}},
			}
			if pending {
				next, _ := m.Update(keyMsg("p"))
				m = next.(Model)
			}
			row := m.renderMidRow()
			if got := lipgloss.Width(row); got != width {
				t.Errorf("width %d pending %v: mid-row width = %d", width, pending, got)
			}
			if pending && !strings.Contains(strip(m.View()), "probing") {
				t.Errorf("width %d: previous result hides new probe feedback", width)
			}
		}
	}
}

func TestProcLineContextIsTokenCount(t *testing.T) {
	got := strip(procLine(core.ProviderSnapshot{
		Models: []core.ModelInfo{{Name: "llama", CtxMax: 8192}},
	}))
	if !strings.Contains(got, "8.2k") || !strings.Contains(got, "tok") {
		t.Errorf("ctx = %q, want a token count like 8.2k tok", got)
	}
	if strings.Contains(got, "Mtok") || strings.Contains(got, "0M") {
		t.Errorf("ctx still uses the byte estimate: %q", got)
	}
}

// Engine rows used a bare bar and "r1 w2": first-timers could not tell the
// bar was KV cache or that r/w were running/waiting queues.
func TestEnginesPanelLabelsKVAndQueue(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{
		Label: "ollama", Kind: core.KindOllama, OK: true,
		OutTokPS: 42, InTokPS: 10, KVPct: 55, Running: 1, Waiting: 2,
	}}}
	out := strip(m.providersBody(50))
	for _, want := range []string{"kv ", "run 1", "wait 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("engine row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "r1 w2") {
		t.Errorf("engine row still uses terse r/w queue labels:\n%s", out)
	}
}

func TestEngineStateKeepsRowsWhenDetailsOverflow(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{
		Label: "engine-a", OK: true, KVPct: 55,
		Models:  []core.ModelInfo{{SizeVRAM: 8 << 30}},
		ProcRSS: 4 << 30, ProcCPU: 75, TTFTms: 150,
	}, {
		Label: "engine-b", OK: true, KVPct: 20,
	}}}
	for _, w := range []int{15, 20, 33} {
		lines := strings.Split(m.gaugesBody(w), "\n")
		for i, want := range map[int]string{0: "engine-a", 1: "kv ", 2: "mem ", 4: "engine-b", 5: "kv "} {
			if i >= len(lines) || !strings.Contains(strip(lines[i]), want) {
				t.Errorf("width %d: row %d missing %q in %q", w, i, want, lines)
			}
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got > w {
				t.Errorf("width %d: row %d occupies %d columns", w, i, got)
			}
		}
	}
}

// All backends down: ENGINE STATE must say so instead of promising telemetry
// that will never arrive.
func TestEngineStateNamesAllDownEngines(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "a", OK: false}, {Label: "b", OK: false},
	}}
	if out := strip(m.gaugesBody(30)); !strings.Contains(out, "no healthy engines") {
		t.Errorf("gaugesBody hides all-down state: %q", out)
	}
	m.snap = core.Snapshot{}
	if out := strip(m.gaugesBody(30)); !strings.Contains(out, "waiting for telemetry") {
		t.Errorf("gaugesBody lost the pre-discovery message: %q", out)
	}
}

// The compact view stands alone: it must explain why it is compact, how to
// quit, and what to do when no engines are found (previously a blank pane).
func TestMinimalViewGuidesRecovery(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "ollama", OK: true, OutTokPS: 42},
	}}))
	m = nm.(Model)
	m.w, m.h, m.ready = 40, 12, true
	out := strip(m.View())
	for _, want := range []string{"enlarge window", "q quit", "ollama"} {
		if !strings.Contains(out, want) {
			t.Errorf("minimal view missing %q:\n%s", want, out)
		}
	}

	empty := New(Config{Version: "t"}, nil)
	empty.w, empty.h, empty.ready = 40, 12, true
	out = strip(empty.View())
	for _, want := range []string{"no inference engines detected", "--demo", "--agents"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty minimal view missing %q:\n%s", want, out)
		}
	}
	watching := New(Config{Version: "t", Agents: true}, nil)
	watching.w, watching.h, watching.ready = 40, 12, true
	out = strip(watching.View())
	if !strings.Contains(out, "watching local agents") {
		t.Errorf("minimal --agents empty view lost the wait hint:\n%s", out)
	}
	if strings.Contains(out, "--demo") {
		t.Errorf("minimal --agents empty view still tells them to restart:\n%s", out)
	}

	now := time.Now()
	agents := New(Config{Version: "t"}, nil)
	nm, _ = agents.Update(snapMsg(core.Snapshot{Agents: []core.AgentEvent{
		{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn", OutputTokens: 40},
		{At: now.Add(-time.Second), Agent: "claude", Kind: "turn", OutputTokens: 40},
	}}))
	agents = nm.(Model)
	agents.w, agents.h, agents.ready, agents.clock = 40, 12, true, now
	out = strip(agents.View())
	if !strings.Contains(out, "claude") || !strings.Contains(out, "tok/s") {
		t.Errorf("minimal view lost agent stats:\n%s", out)
	}
	if strings.Contains(out, "no inference engines detected") {
		t.Errorf("minimal view hid agents behind the engines-empty message:\n%s", out)
	}
}

// Long engine labels and a crowded strip must not wrap (bubbletea then
// scrambles later rows) or push the key hint off the pane.
func TestMinimalViewFitsPane(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	ps := make([]core.ProviderSnapshot, 20)
	for i := range ps {
		ps[i] = core.ProviderSnapshot{
			Label:    "engine-" + strings.Repeat("x", 40),
			OK:       true,
			OutTokPS: 1,
			Err:      strings.Repeat("connection refused elsewhere", 3),
		}
	}
	m.snap = core.Snapshot{Providers: ps}
	m.w, m.h, m.ready = 40, 10, true
	out := m.View()
	if got := lipgloss.Height(out); got > 10 {
		t.Errorf("compact frame is %d lines, overflows pane 10", got)
	}
	for i, ln := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(ln); lw > 40 {
			t.Fatalf("line %d renders %d cells, want <= 40:\n%s", i, lw, ln)
		}
	}
	if !strings.Contains(strip(out), "q quit") {
		t.Errorf("key hint lost when engines overflow the pane:\n%s", strip(out))
	}
}

// Recovery flags are alternatives, not one command with every switch.
func TestMinimalViewRecoveryAreAlternatives(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 40, 12, true
	out := strip(m.View())
	if strings.Contains(out, "toktop --demo --add") {
		t.Errorf("compact empty state mashes flags into one command:\n%s", out)
	}
	if !strings.Contains(out, "or --agents") && !strings.Contains(out, "or --add") {
		t.Errorf("compact empty state does not mark flags as alternatives:\n%s", out)
	}
}

// A dead ingest endpoint must be visible in-band: stderr is hidden under the
// alternate screen, so without this the UI advertises a dead endpoint (and a
// POST target that swallows events) forever.
func TestFeedDeathSurfacesInDashboard(t *testing.T) {
	feedErr := make(chan string, 1)
	m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420", FeedErr: feedErr}, nil)

	init := m.Init()
	if init == nil {
		t.Fatal("Init must watch the feed-error channel")
	}
	m.w, m.h, m.ready, m.clock = 110, 36, true, time.Now()
	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
	}))
	m = nm.(Model)
	// A live endpoint advertises its POST target.
	if out := strip(m.View()); !strings.Contains(out, "POST http://127.0.0.1:8420/v1/events") {
		t.Fatalf("live feed lost its POST hint:\n%s", out)
	}

	feedErr <- "http: Server closed"
	cmds := batchCmds(init)
	if len(cmds) == 0 {
		t.Fatal("Init returned no commands")
	}
	// Run every leaf: waitSnap/tickClock legitimately idle or sleep, but the
	// pre-loaded feed channel must deliver immediately.
	done := make(chan tea.Msg, len(cmds))
	for _, c := range cmds {
		go func(c tea.Cmd) { done <- c() }(c)
	}
	var got tea.Msg
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("no Init command produced a message")
	}
	if _, ok := got.(feedDownMsg); !ok {
		t.Fatalf("waiting on the feed channel produced %v, want feedDownMsg", got)
	}
	nm, again := m.Update(got)
	m = nm.(Model)
	if m.feedDown != "http: Server closed" {
		t.Fatalf("feedDownMsg not recorded: %q", m.feedDown)
	}
	if again == nil {
		t.Error("Update must re-arm the feed watcher for later signals")
	}

	out := strip(m.View())
	if strings.Contains(out, "POST http://127.0.0.1:8420") {
		t.Errorf("dead endpoint still advertised:\n%s", out)
	}
	for _, want := range []string{"ingest down", "ingest stopped: http: Server closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q after feed death:\n%s", want, out)
		}
	}
}

// batchCmds flattens a tea.Cmd (possibly tea.Batch) into its leaves.
func batchCmds(cmd tea.Cmd) []tea.Cmd {
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Cmd{cmd}
	}
	return []tea.Cmd(batch)
}

// Pause is a stability affordance for screen-reader and magnifier users:
// while paused nothing on screen may keep moving, and the header clock was
// the one element still churning every second.
func TestPauseFreezesHeaderClock(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
	}))
	m = nm.(Model)
	m.w, m.h, m.ready = 110, 36, true
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.Local)
	nm, _ = m.Update(tickMsg(t0))
	m = nm.(Model)
	if out := strip(m.View()); !strings.Contains(out, "12:00:00") {
		t.Fatalf("header clock missing before pause:\n%s", out)
	}

	nm, _ = m.Update(keyMsg(" "))
	m = nm.(Model)
	later := t0.Add(11 * time.Second)
	nm, _ = m.Update(tickMsg(later))
	m = nm.(Model)
	if out := strip(m.View()); strings.Contains(out, "12:00:11") {
		t.Error("paused frame kept ticking the clock")
	}

	nm, _ = m.Update(keyMsg(" "))
	m = nm.(Model)
	nm, _ = m.Update(tickMsg(later))
	m = nm.(Model)
	if out := strip(m.View()); !strings.Contains(out, "12:00:11") {
		t.Error("resumed frame did not pick the clock back up")
	}
}

// --once must score agent rates against the snapshot's own stamp. Reading
// wall time would drop events that were live when the sample was taken
// once that sample is older than the 30s window.
func TestStaticFrameUsesSnapshotTime(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	snap := core.Snapshot{
		At: past,
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, OK: true, OutTokPS: 10,
		}},
		Agents: []core.AgentEvent{
			{At: past.Add(-2 * time.Second), Agent: "bot", Kind: "turn",
				PromptTokens: 80, OutputTokens: 40},
			{At: past, Agent: "bot", Kind: "turn",
				PromptTokens: 80, OutputTokens: 40},
		},
	}
	out := strip(StaticFrame(Config{Version: "t"}, snap, 110, 36))
	if !strings.Contains(out, "1 agent") {
		t.Fatalf("hour-old snapshot dropped in-window agents:\n%s", out)
	}

	// The live view used to score rates against the tick clock, so a
	// snapshot whose At (demo, or a held --once-style frame) sat outside
	// AgentRateWindow of wall time drew an empty agent list.
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 110, 36, true
	m.clock = time.Now()
	m.snap = snap
	live := strip(m.View())
	if !strings.Contains(live, "1 agent") {
		t.Fatalf("live view dropped in-window agents against a later clock:\n%s", live)
	}
}

func TestStaticFrameReplayIgnoresWallClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, at := range []time.Time{{}, time.Unix(1_700_000_000, 0).UTC()} {
			snap := core.Snapshot{
				At: at,
				Providers: []core.ProviderSnapshot{{
					Label: "engine", OK: true, OutTokPS: 10,
				}},
			}
			cfg := Config{Version: "t"}
			first := StaticFrame(cfg, snap, 110, 36)
			time.Sleep(time.Minute)
			if replay := StaticFrame(cfg, snap, 110, 36); replay != first {
				t.Fatalf("static frame changed with wall time for At=%v:\nfirst:\n%s\nreplay:\n%s", at, first, replay)
			}
		}
	})
}

func TestFrameNowDoesNotReadWallClock(t *testing.T) {
	if got := frameNow(core.Snapshot{}, time.Time{}); !got.IsZero() {
		t.Fatalf("zero snapshot and fallback produced %v, want zero", got)
	}
	stamp := time.Unix(1_700_000_000, 0).UTC()
	if got := frameNow(core.Snapshot{At: stamp}, time.Time{}); !got.Equal(stamp) {
		t.Fatalf("snapshot At = %v, got %v", stamp, got)
	}
	fb := stamp.Add(time.Second)
	if got := frameNow(core.Snapshot{}, fb); !got.Equal(fb) {
		t.Fatalf("fallback = %v, got %v", fb, got)
	}
}

// The probing marker expires on the UI clock, not wall time, so a paused
// frame does not clear it while frozen and a tick 16s later does.
func TestProbeTimeoutFollowsClock(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	nm, _ := m.Update(tickMsg(t0))
	m = nm.(Model)
	m.probeReq = t0

	nm, _ = m.Update(tickMsg(t0.Add(14 * time.Second)))
	m = nm.(Model)
	if m.probeReq.IsZero() {
		t.Fatal("probe marker cleared before 15s of clock time")
	}

	nm, _ = m.Update(keyMsg(" "))
	m = nm.(Model)
	nm, _ = m.Update(tickMsg(t0.Add(time.Minute)))
	m = nm.(Model)
	if m.probeReq.IsZero() {
		t.Fatal("paused frame expired the probe marker")
	}

	nm, _ = m.Update(keyMsg(" "))
	m = nm.(Model)
	nm, _ = m.Update(tickMsg(t0.Add(16 * time.Second)))
	m = nm.(Model)
	if !m.probeReq.IsZero() {
		t.Fatal("probe marker survived 16s of clock time")
	}
}

// The ENGINES panel must mark down engines by more than color (WCAG 1.4.1):
// the ✗ glyph matches the probe feed's convention and survives greyscale.
func TestEnginesPanelMarksDownEnginesWithoutColor(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "vllm", OK: false, Err: "connection refused"},
	}}))
	m = nm.(Model)
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.View())
	if !strings.Contains(out, "✗") || !strings.Contains(out, "connection refused") {
		t.Errorf("down engine lacks shape marker or error text:\n%s", out)
	}
}

// A failed probe must say so and keep the reason: the same row used to
// print 0.0/s in the success shape, which hid the failure.
func TestFailedProbeShowsError(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes:    []core.ProbeSample{{At: time.Now(), Model: "gone", OK: false, Err: "timeout"}},
	}))
	m = nm.(Model)
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.View())
	for _, want := range []string{"failed", "timeout", "last failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("failed probe missing %q:\n%s", want, out)
		}
	}
	title, row := strip(m.probesTitle()), strip(m.probesBody(40, 8))
	if strings.Contains(title, "tok/s") || strings.Contains(row, "tok/s") || strings.Contains(row, "/s") {
		t.Errorf("failed probe still prints a rate: title %q row %q", title, row)
	}
}

func TestSuccessfulProbeUsesTokPerSec(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes:    []core.ProbeSample{{At: time.Now(), Model: "llama3", OK: true, TokPS: 340, TTFTms: 97}},
	}
	if got := strip(m.probesTitle()); !strings.Contains(got, "tok/s") {
		t.Errorf("probe title = %q, want tok/s like the rest of the dashboard", got)
	}
	if got := strip(m.probesBody(40, 8)); !strings.Contains(got, "tok/s") {
		t.Errorf("probe row = %q, want tok/s", got)
	}
}

// --probe is a startup flag: without "quit, then" this panel used to look
// like you could type it into the dashboard, the same trap the empty card
// used to have.
func TestProbeEmptyHintsRerunForAuto(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	out := strip(m.probesBody(40, 8))
	if !strings.Contains(out, "press") || !strings.Contains(out, "p") {
		t.Errorf("empty probes lost the p hint:\n%s", out)
	}
	if !strings.Contains(out, "quit") || !strings.Contains(out, "--probe") {
		t.Errorf("empty probes must say --probe needs a re-run:\n%s", out)
	}
}

// p and t only have a visible effect with engines (or agents, for t). The
// compact strip already hides them; the full footer must match.
func TestFooterOmitsDeadKeys(t *testing.T) {
	empty := New(Config{Version: "t", Prober: func() {}}, nil)
	if got := strip(empty.renderFooter()); strings.Contains(got, "probe") || strings.Contains(got, "timescale") {
		t.Errorf("empty footer advertised keys with no effect: %q", got)
	}
	agents := New(Config{Version: "t", Prober: func() {}}, nil)
	agents.snap = core.Snapshot{Agents: []core.AgentEvent{{Agent: "claude"}}}
	got := strip(agents.renderFooter())
	if strings.Contains(got, "probe") {
		t.Errorf("agents-only footer advertised p: %q", got)
	}
	if !strings.Contains(got, "timescale") {
		t.Errorf("agents-only footer lost t: %q", got)
	}
	full := New(Config{Version: "t", Prober: func() {}}, nil)
	full.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "x", OK: true}}}
	got = strip(full.renderFooter())
	if !strings.Contains(got, "probe") || !strings.Contains(got, "timescale") {
		t.Errorf("engine footer lost live keys: %q", got)
	}
}

func TestHeaderSessionMatchesPlain(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Uptime:    90 * time.Second,
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
	}
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.renderHeader())
	if !strings.Contains(out, "session 1m30s") {
		t.Errorf("header missing session duration: %q", out)
	}
	if strings.Contains(out, "up 1m30s") {
		t.Errorf("header still says up instead of session: %q", out)
	}
}

func TestProbeKeyNoopsWithoutEngines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var probes atomic.Int32
		m := New(Config{Version: "t", Prober: func() { probes.Add(1) }}, nil)
		for _, k := range []string{"p", "P"} {
			nm, _ := m.Update(keyMsg(k))
			m = nm.(Model)
			synctest.Wait()
			if got := probes.Load(); got != 0 {
				t.Errorf("%s fired %d probes with no engines attached", k, got)
			}
			if !m.probeReq.IsZero() {
				t.Errorf("%s set the probing marker with no engines attached", k)
			}
		}
	})
}

func TestHelpFitsCompactPane(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help, m.w, m.h, m.ready = true, 40, 10, true
	out := m.View()
	if got := lipgloss.Height(out); got > 10 {
		t.Errorf("help is %d lines, overflows pane 10", got)
	}
	for i, ln := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(ln); lw > 40 {
			t.Fatalf("help line %d renders %d cells, want <= 40:\n%s", i, lw, ln)
		}
	}
	plain := strip(out)
	for _, want := range []string{"quit", "pause", "help"} {
		if !strings.Contains(plain, want) {
			t.Errorf("compact help missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "--demo") || strings.Contains(plain, "real generation") {
		t.Errorf("compact help still lists full-dashboard keys/flags:\n%s", plain)
	}
}

func TestHelpSaysFlagsNeedRerun(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help, m.w, m.h, m.ready = true, 110, 36, true
	out := strip(m.View())
	if !strings.Contains(out, "quit, then re-run") {
		t.Errorf("help does not say flags need a restart:\n%s", out)
	}
}

// p fires a real generation that burns tokens and GPU time: calling it
// "synthetic" read as a no-op simulation.
func TestHelpDescribesLiveProbe(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help, m.w, m.h, m.ready = true, 110, 36, true
	out := strip(m.View())
	if !strings.Contains(out, "real generation") {
		t.Errorf("help does not say p runs a real generation:\n%s", out)
	}
	if strings.Contains(out, "synthetic") {
		t.Errorf("help still calls the probe synthetic:\n%s", out)
	}
}
