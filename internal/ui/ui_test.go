package ui

import (
	"fmt"
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

// pressKey sends a key to m and stores the model the update returned, so a
// test can step the model forward and still read the command it scheduled.
func pressKey(m *Model, s string) tea.Cmd {
	nm, cmd := m.Update(keyMsg(s))
	*m = nm.(Model)
	return cmd
}

// assertFitsPane fails unless out is at most h lines and every line is at
// most w cells wide. The frame must fit its pane exactly: bubbletea clips
// overflow from the top, which hides the header, and any line wider than
// the pane wraps and drags every later row out of alignment.
func assertFitsPane(t *testing.T, label, out string, w, h int) {
	t.Helper()
	if got := lipgloss.Height(out); got > h {
		t.Errorf("%s is %d lines, overflows pane %d", label, got, h)
	}
	for i, ln := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(ln); lw > w {
			t.Fatalf("%s line %d renders %d cells, want <= %d:\n%s", label, i, lw, w, ln)
		}
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
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
	key := func(s string) tea.Cmd { return pressKey(&m, s) }
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
	// p and t are documented only where they have something to act on, the
	// same condition the footer puts on them, so the frame under test needs
	// an engine for the key reference to carry those rows. The rest of the
	// test runs from an empty snapshot, so put that back afterwards.
	saved := m.snap
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	out := strip(m.View())
	m.snap = saved
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
	probe := key("p")
	if probe == nil {
		t.Fatal("p with engines attached must return a probe command")
	}
	// The dispatch is a program command, so the test runs it rather than
	// waiting on a goroutine it cannot see.
	if msg := probe(); msg != nil {
		t.Errorf("probe command produced %v, want no message", msg)
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
	key := func(s string) tea.Cmd { return pressKey(&m, s) }

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
		// p dispatches through a program command, so a nil command is the
		// whole claim: running it is what fires a probe.
		if cmd := key(k); cmd != nil {
			t.Errorf("%s returned a probe command while help was open: %v", k, cmd())
		}
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
		// p dispatches through a program command, so the test runs it: there
		// is no detached goroutine left to wait on.
		probe := key(k)
		if probe == nil {
			t.Fatalf("%s after help closed returned no probe command", k)
		}
		probe()
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
	// A down engine has no rate. Printing its 0.0 beside the failure names two
	// states at once, and the row reads as a measurement.
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "down") && strings.Contains(ln, "tok/s") {
			t.Errorf("minimal view prints a rate for a down engine: %q", ln)
		}
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
	// MaxUint64 + 1GiB saturates to MaxUint64, and MaxUint64/(1<<30) is
	// 17179869184.0. A wrapping sum lands on exactly 1<<30 instead, which
	// formats as "1.0GiB" and would satisfy a bare "contains GiB" check.
	const want = "17179869184.0GiB"
	if !strings.Contains(got, want) {
		t.Fatalf("VRAM sum = %q, want the saturated figure %q", got, want)
	}
}

func TestProcLineCtxCountFitsInt64(t *testing.T) {
	got := strip(procLine(core.ProviderSnapshot{
		Models: []core.ModelInfo{{Name: "a", CtxMax: 1 << 63}},
	}))
	// 1<<63 token counts: a scan for a minus sign anywhere in the row would
	// also fire on a model name like llama-3, and a wrapping conversion lands
	// on 1<<63/1e9 = 9.2M rather than 9223372036854.8M.
	const want = "9223372036854.8M"
	if !strings.Contains(got, want) {
		t.Fatalf("ctx = %q, want the unsigned 1<<63 token count %q", got, want)
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

// stamps builds one instant per history sample, cadence apart: the shape a
// provider snapshot carries, so a test states the real sample times.
func stamps(t0 time.Time, n int, cad time.Duration) []time.Time {
	ts := make([]time.Time, n)
	for i := range ts {
		ts[i] = t0.Add(time.Duration(i) * cad)
	}
	return ts
}

func TestAggHistTimeAligned(t *testing.T) {
	// Provider B joins 3 cadences after A: tail-index alignment would smear
	// the window; absolute-time alignment must not.
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutStamps: stamps(t0, 6, time.Second), OutHist: []float64{1, 1, 1, 1, 1, 1}}, // t0..t0+5
		{OutStamps: stamps(t0.Add(3*time.Second), 3, time.Second), OutHist: []float64{2, 2, 2}},
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

// A provider sampled slower than the grid keeps its real spacing: the
// samples land in the buckets they were taken in, and the stretch between
// them is a real stretch, not a compressed one-cadence fiction.
func TestAggHistKeepsIrregularSampleSpacing(t *testing.T) {
	t0 := time.Now()
	ts := []time.Time{t0, t0.Add(4 * time.Second), t0.Add(5 * time.Second)}
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutStamps: ts, OutHist: []float64{2, 4, 6}},
	}}
	got := aggHist(s, true, 6, time.Second)
	want := []float64{2, 0, 0, 0, 4, 6}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aggHist = %v, want %v", got, want)
		}
	}
}

// A sample exactly half a cadence from a column centre is within reach of two
// of them, and both must carry it: the bucket rule is a half-cadence window
// around every column, and deriving which columns a sample reaches cannot
// quietly narrow that to one.
func TestAggHistCountsBoundarySampleInBothColumns(t *testing.T) {
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutStamps: []time.Time{t0.Add(-3 * time.Second), t0.Add(-1500 * time.Millisecond), t0},
			OutHist: []float64{1, 5, 3}},
	}}
	got := aggHist(s, true, 4, time.Second)
	// end is the newest sample, t0, so the centres are t0-3s .. t0. The
	// middle sample sits exactly between the centres at t0-2s and t0-1s.
	want := []float64{1, 5, 5, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A sample further than half a cadence from every column contributes to none,
// rather than being pulled into the nearest one.
func TestAggHistDropsSamplesOutsideWindow(t *testing.T) {
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		// end is t0, so the centres are t0-3s .. t0 and their reach starts
		// at t0-3.5s. The first sample is half a second outside it.
		{OutStamps: []time.Time{t0.Add(-4 * time.Second), t0},
			OutHist: []float64{7, 8}},
	}}
	got := aggHist(s, true, 4, time.Second)
	want := []float64{0, 0, 0, 8}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// An engine that stopped reporting must leave its buckets empty rather than
// dragging the whole window leftward.
func TestAggHistIgnoresStaleEngine(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * time.Second)
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutStamps: stamps(old, 2, time.Second), OutHist: []float64{9, 9}},
		{OutStamps: stamps(now.Add(-2*time.Second), 2, time.Second), OutHist: []float64{4, 4}},
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
	if len(nonzero) != 2 || nonzero[0] != 200 || nonzero[1] != 50 {
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
	// 100 columns is 1e11ns, 37 bits, so 63-37 levels of halving are left.
	if got := spanCap(100); got != 26 {
		t.Errorf("spanCap(100) = %d, want 26", got)
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
	if got := tempColor(math.NaN()); got != cGreen {
		t.Errorf("tempColor(NaN) = %v, want %v", got, cGreen)
	}
	if got := memHeat(math.NaN()); got != cGreen {
		t.Errorf("memHeat(NaN) = %v, want %v", got, cGreen)
	}
	if got := kvHeat(math.NaN()); got != cGreen {
		t.Errorf("kvHeat(NaN) = %v, want %v", got, cGreen)
	}
}

func TestHeatFunctionsThresholds(t *testing.T) {
	tempCases := []struct {
		v    float64
		want lipgloss.Color
	}{
		{59.9, cGreen},
		{60.0, cYellow},
		{79.9, cYellow},
		{80.0, cRed},
		{100.0, cRed},
	}
	for _, tc := range tempCases {
		if got := tempColor(tc.v); got != tc.want {
			t.Errorf("tempColor(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}

	memCases := []struct {
		v    float64
		want lipgloss.Color
	}{
		{69.9, cGreen},
		{70.0, cYellow},
		{89.9, cYellow},
		{90.0, cRed},
		{100.0, cRed},
	}
	for _, tc := range memCases {
		if got := memHeat(tc.v); got != tc.want {
			t.Errorf("memHeat(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}

	kvCases := []struct {
		v    float64
		want lipgloss.Color
	}{
		{59.9, cGreen},
		{60.0, cYellow},
		{84.9, cYellow},
		{85.0, cRed},
		{100.0, cRed},
	}
	for _, tc := range kvCases {
		if got := kvHeat(tc.v); got != tc.want {
			t.Errorf("kvHeat(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestUniqueAgents(t *testing.T) {
	cases := []struct {
		name   string
		events []core.AgentEvent
		want   int
	}{
		{"empty", nil, 0},
		{"empty strings", []core.AgentEvent{{Agent: ""}, {Agent: ""}}, 0},
		{"duplicates and empty", []core.AgentEvent{
			{Agent: "claude"},
			{Agent: "codex"},
			{Agent: "claude"},
			{Agent: ""},
			{Agent: "gemini"},
		}, 3},
		// One agent spelled two ways is one agent, the way Summarize
		// groups them: the header count must not out-count the list.
		{"nfc and nfd spell one agent", []core.AgentEvent{
			{Agent: "caf\u00e9"},
			{Agent: "cafe\u0301"},
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uniqueAgents(tc.events); got != tc.want {
				t.Errorf("uniqueAgents() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestStaticFrameRenders(t *testing.T) {
	t0 := time.Now()
	snap := core.Snapshot{
		At:     time.Now(),
		Uptime: time.Minute,
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, Addr: "http://127.0.0.1:11434", OK: true,
			Models:   []core.ModelInfo{{Name: "llama3"}},
			OutTokPS: 42, InTokPS: 100, KVPct: 55,
			OutHist:   []float64{1, 2, 3, 4},
			InHist:    []float64{2, 4, 6, 8},
			OutStamps: stamps(t0, 4, time.Second),
			InStamps:  stamps(t0, 4, time.Second),
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

// The SYS strip packs its readings left to right and sheds from the right, so
// a host with several accelerators loses all but the first one on a narrow
// pane. A row that silently stops mid-list reads as one GPU on the machine, so
// what it dropped has to be counted, on both rows.
func TestSystemStripCountsShedSegments(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "x", Kind: core.KindOllama, OK: true}},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			GPUs: []core.GPUDevice{
				{Vendor: "nvidia", Index: 0, MilliC: 70000, MemTotal: 80 << 30, MemUsed: 40 << 30, PowerW: 297},
				{Vendor: "nvidia", Index: 1, MilliC: 71000, MemTotal: 80 << 30, MemUsed: 41 << 30, PowerW: 301},
				{Vendor: "nvidia", Index: 2, MilliC: 72000, MemTotal: 80 << 30, MemUsed: 42 << 30, PowerW: 288},
				{Vendor: "nvidia", Index: 3, MilliC: 73000, MemTotal: 80 << 30, MemUsed: 43 << 30, PowerW: 290},
			},
		},
	}
	m.w, m.h, m.ready = 100, 40, true
	out := strip(m.renderSystem())
	if !strings.Contains(out, "nv0") {
		t.Fatalf("strip = %q, want the first GPU rendered:\n%s", out, out)
	}
	if !strings.Contains(out, "more") {
		t.Errorf("strip = %q, want the GPUs it could not fit counted:\n%s", out, out)
	}
	// The count has to name the whole row, not one group of it: exactly one.
	if n := strings.Count(out, "more"); n != 1 {
		t.Errorf("strip = %q, want one overflow count, got %d", out, n)
	}
	assertFitsPane(t, "100x40 sys strip", m.renderSystem(), m.w, m.h)
}

// The host strip drops readings for want of width, so its count names the way
// out the same way the panel titles do, and sheds the sentence on a row too
// narrow to carry it.
func TestSystemStripOverflowNamesTheWayOut(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "x", Kind: core.KindOllama, OK: true}},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			GPUs: []core.GPUDevice{
				{Vendor: "nvidia", Index: 0, MilliC: 70000, MemTotal: 80 << 30, MemUsed: 40 << 30, PowerW: 297},
				{Vendor: "nvidia", Index: 1, MilliC: 71000, MemTotal: 80 << 30, MemUsed: 41 << 30, PowerW: 301},
				{Vendor: "nvidia", Index: 2, MilliC: 72000, MemTotal: 80 << 30, MemUsed: 42 << 30, PowerW: 288},
				{Vendor: "nvidia", Index: 3, MilliC: 73000, MemTotal: 80 << 30, MemUsed: 43 << 30, PowerW: 290},
			},
		},
	}
	m.w, m.h, m.ready = 120, 40, true
	got := strip(m.renderSystem())
	if !strings.Contains(got, "more (enlarge window)") {
		t.Errorf("strip = %q, want the overflow count to name the way out", got)
	}
	if n := strings.Count(got, "more"); n != 1 {
		t.Errorf("strip = %q, want one overflow count, got %d", got, n)
	}
	m.w = 62
	if got := strip(m.renderSystem()); !strings.Contains(got, "more") {
		t.Errorf("strip = %q, want the bare count the narrowest row has room for", got)
	}
}

// The timescale toggle lives in the chart title; both modes must show the
// current one plus a clearly delimited key, not a run-together "←t".
func TestThroughputTitleAdvertisesTimescaleToggle(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	if got := strip(m.throughputTitle(80, 0)); !strings.Contains(got, "compressed") || !strings.Contains(got, "[t]") {
		t.Errorf("compressed title = %q, want mode word plus [t] switch", got)
	}
	m.chartCompressed = false
	if got := strip(m.throughputTitle(80, 0)); !strings.Contains(got, "uniform") || !strings.Contains(got, "[t]") {
		t.Errorf("uniform title = %q, want mode word plus [t] switch", got)
	}
}

// The two charts are stacked with no axis labels on either, so a reader reads
// them as one time axis. The timescale toggle must move both: a prompt plot
// left on the uniform cadence put the pair on different time bases with nothing
// on screen to say so, and toggling t looked like it had broken the second one.
func TestTimescaleToggleMovesBothCharts(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = busySnap()
	m.w, m.h, m.ready = 120, 40, true

	for _, tc := range []struct {
		compressed bool
		wantBounds bool
	}{{true, true}, {false, false}} {
		m.chartCompressed = tc.compressed
		for _, out := range []bool{true, false} {
			vals, bounds := m.rateSeries(100, time.Second, out)
			if len(vals) == 0 {
				t.Fatalf("compressed=%v out=%v: empty series", tc.compressed, out)
			}
			if (len(bounds) > 0) != tc.wantBounds {
				t.Errorf("compressed=%v out=%v: boundaries = %v, want %v",
					tc.compressed, out, bounds, tc.wantBounds)
			}
		}
	}
}

// A frame redraws once a second against a feed retaining 512 events, and
// the header, charts, feed, footer and agents view each need a different
// slice of that feed's accounting. View must do it once: a consumer that
// recomputes costs a full extra walk of the feed per frame.
func TestFrameAccountsAgentsOnce(t *testing.T) {
	m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420", Agents: true}, nil)
	m.snap = perfSnap()
	m.w, m.h, m.ready = 120, 40, true

	if m.sum != nil {
		t.Fatal("a model built for a frame already holds a summary")
	}
	frame := strip(m.View())
	// The frame's own summary is what its consumers read, so the agents
	// from the snapshot have to reach the output. The rate is the proof:
	// the events carry token counts but no tok/s, so the figure can only
	// come from the summary the frame computed.
	for _, want := range []string{"claude", "codex", "22.2 tok/s"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame is missing %q; View did not account the feed:\n%s", want, frame)
		}
	}
	// View takes the model by value, so the summary it fills belongs to
	// that frame alone: the model the caller kept must not carry it into
	// the next one, where the feed and the clock have both moved on. A
	// pointer receiver would leave it there and the next frame would draw
	// rates computed against the previous snapshot.
	if m.sum != nil {
		t.Error("the frame's summary escaped onto the caller's model; the next frame would draw stale rates")
	}
}

// A consumer called outside a frame still gets correct numbers: the accessor
// computes the same summary on demand rather than returning nothing.
func TestAgentConsumersOutsideAFrame(t *testing.T) {
	m := New(Config{Version: "t", Agents: true}, nil)
	m.snap = perfSnap()
	m.w, m.h, m.ready = 120, 40, true

	got := m.agentRates()
	want := core.Summarize(m.snap.Agents, m.snapNow()).Rates
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("agentRates outside a frame = %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if m.sum != nil {
		t.Error("reading rates outside a frame cached a summary on the model")
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

// The POST target in the AGENT FEED title reaches a panel title, which does
// not clip its own width, so an unsanitized address turns a control character
// into a row of the frame and stretches every row under it. Every other
// render of this string sanitizes it; this one has to as well.
func TestFeedTitleSanitizesIngestAddr(t *testing.T) {
	m := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420\x1b[2Jboom"}, nil)
	m.w, m.h, m.ready = 120, 36, true
	m.snap = core.Snapshot{Agents: []core.AgentEvent{{Agent: "a", At: time.Now()}}}
	title := m.feedTitle(m.w-4, 0, 0, m.agentRates())
	if strings.ContainsRune(title, '\x1b') {
		t.Errorf("feed title kept an escape sequence: %q", title)
	}
	if lipgloss.Width(title) > m.w-4 {
		t.Errorf("feed title is %d cells, over the %d it was given", lipgloss.Width(title), m.w-4)
	}
	assertFitsPane(t, "feed", m.View(), m.w, m.h)
}

// The help screen mutes every action key and swallows q and esc, so the
// reference it prints has to say so: a reader who presses q on a list that
// said "quit" and lands back on the dashboard reads it as a dropped key.
func TestHelpSaysWhatClosesItAndThatActionsAreMuted(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
	}{
		{"full", 110, 36},
		{"compact", 40, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Config{Version: "t", Prober: func() {}}, nil)
			m.help, m.w, m.h, m.ready = true, tc.w, tc.h, true
			m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "e", OK: true}}}
			out := strip(m.View())
			for _, want := range []string{"action keys are muted", "close"} {
				if !strings.Contains(out, want) {
					t.Errorf("help missing %q:\n%s", want, out)
				}
			}
			// No key row may claim its key quits on its own: on this screen
			// it does not. The blank row ends the key section; the "(flags)"
			// heading below it is not a key.
			for _, r := range m.helpRows() {
				if r[0] == "" {
					break
				}
				if strings.Contains(r[1], "quit") && !strings.Contains(r[1], "close") {
					t.Errorf("help row %q claims %q quits from this view", r[0], r[1])
				}
			}
		})
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
			nm, _ := m.Update(feedDownMsg("ingest stopped: http: Server closed"))
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
		assertFitsPane(t, fmt.Sprintf("%dx%d empty frame", w, h), StaticFrame(cfg, core.Snapshot{}, w, h), w, h)
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

// Every documented agent-event kind must have a feed mark; a missing entry
// silently falls through to the generic dot.
func TestKindMarksCoverAgentKinds(t *testing.T) {
	for _, k := range []string{
		core.AgentKindTurn, core.AgentKindTool, core.AgentKindError, core.AgentKindNote,
	} {
		if _, ok := kindMarks[k]; !ok {
			t.Errorf("kindMarks missing %q", k)
		}
	}
}

// Sample spacing on the compressed timescale comes from the stamps the
// collector recorded, not an assumed 1s: --interval 2s covers twice the
// wall-clock window, and the render cadence does not move a sample.
func TestTimedSeriesUsesSampleStamps(t *testing.T) {
	t0 := time.Now()
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OutStamps: stamps(t0, 2, 2*time.Second), OutHist: []float64{1, 2}},
	}}
	tv := timedSeries(s, true, 2*time.Second)
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
	// Raw output, not strip(): the payload is what the sanitizer must remove,
	// so checking the post-sanitize string proves nothing about gpuSegment.
	out := gpuSegment(g)
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("gpuSegment leaked escape bytes: %q", out)
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
	segs := hostSegments(sy, stripHostLimits)
	// Raw segments: the driver key, the driver value and the NPU name each
	// carry a payload, and each is sanitized at its own call site in
	// hostSegments, so only the unstripped string can fail here.
	for _, s := range segs {
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("hostSegments leaked escape bytes: %q", s)
		}
	}
	joined := strip(strings.Join(segs, " "))
	if !strings.Contains(joined, "ane") {
		t.Errorf("hostSegments lost NPU name: %q", joined)
	}
	if !strings.Contains(joined, "50") {
		t.Errorf("hostSegments lost the driver version: %q", joined)
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
				OutHist: []float64{1, 2, 3}, InHist: []float64{1, 2, 3},
				OutStamps: stamps(time.Now().Add(-2*time.Second), 3, time.Second),
				InStamps:  stamps(time.Now().Add(-2*time.Second), 3, time.Second)},
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

func TestFrameFitsPaneAtCommonSizes(t *testing.T) {
	for _, sz := range [][2]int{{62, 30}, {70, 31}, {80, 32}, {100, 34}, {120, 38}, {160, 44}} {
		w, h := sz[0], sz[1]
		assertFitsPane(t, fmt.Sprintf("%dx%d frame", w, h),
			StaticFrame(Config{Version: "0.1.0", IngestAddr: "127.0.0.1:8420"}, busySnap(), w, h), w, h)
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
	out, _ := m.providersBody(50, 4)
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
		body, _ := m.gaugesBody(w, 8)
		lines := strings.Split(body, "\n")
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
	if out, _ := m.gaugesBody(30, 8); !strings.Contains(strip(out), "no healthy engines") {
		t.Errorf("gaugesBody hides all-down state: %q", out)
	}
	m.snap = core.Snapshot{}
	if out, _ := m.gaugesBody(30, 8); !strings.Contains(strip(out), "waiting for telemetry") {
		t.Errorf("gaugesBody lost the pre-discovery message: %q", out)
	}
}

// The compact view stands alone: it must explain why it is compact, how to
// The compact strip's key row is one line: a pane too narrow for the whole
// list sheds whole keys, in the order the full footer sheds them, instead of
// clipping a key in half and leaving the row ending on a bare separator.
func TestCompactKeyRowShedsWholeKeys(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	m.h, m.ready = 12, true
	for w := 12; w <= 60; w++ {
		m.w = w
		row := strip(m.compactKeys())
		if strings.Contains(row, "probe") && !strings.Contains(row, "p probe") {
			t.Errorf("width %d clipped a key: %q", w, row)
		}
		if strings.Contains(row, keySep) && strings.HasSuffix(row, keySep) {
			t.Errorf("width %d left a dangling separator: %q", w, row)
		}
		if !strings.HasPrefix(row, "q quit") {
			t.Errorf("width %d shed the quit key: %q", w, row)
		}
	}
	// Wide enough for everything, the row is the whole list.
	m.w = 60
	if got, want := strip(m.compactKeys()), "q quit · space pause · p probe · ? help"; got != want {
		t.Errorf("compact keys = %q, want %q", got, want)
	}
	// One cell short of that, p goes and the pointer to the help stays.
	m.w = 38
	if got, want := strip(m.compactKeys()), "q quit · space pause · ? help"; got != want {
		t.Errorf("compact keys = %q, want %q", got, want)
	}
}

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
	assertFitsPane(t, "compact frame", out, 40, 10)
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

	feedErr <- "ingest stopped: http: Server closed"
	cmds := batchCmds(init)
	if len(cmds) == 0 {
		t.Fatal("Init returned no commands")
	}
	// Run every leaf: waitSnap and tickClock legitimately idle or sleep, and
	// tickClock returns at the 1s mark, so this collects what the batch
	// produces over a window rather than taking whichever leaf wins the race.
	done := make(chan tea.Msg, len(cmds))
	for _, c := range cmds {
		go func(c tea.Cmd) { done <- c() }(c)
	}
	var got tea.Msg
	deadline := time.After(2 * time.Second)
	for got == nil {
		select {
		case msg := <-done:
			if _, ok := msg.(feedDownMsg); ok {
				got = msg
			}
		case <-deadline:
			t.Fatal("no Init command produced a feedDownMsg")
		}
	}
	nm, again := m.Update(got)
	m = nm.(Model)
	if m.feedDown != "ingest stopped: http: Server closed" {
		t.Fatalf("feedDownMsg not recorded: %q", m.feedDown)
	}
	if again == nil {
		t.Error("Update must re-arm the feed watcher for later signals")
	}

	out := strip(m.View())
	if strings.Contains(out, "POST http://127.0.0.1:8420") {
		t.Errorf("dead endpoint still advertised:\n%s", out)
	}
	for _, want := range []string{"feed error", "ingest stopped: http: Server closed"} {
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

// The same re-run hint as a sentence wherever the column holds one: "quit,
// --probe N" beside a key hint reads as two unrelated words.
func TestProbeEmptyHintSpellsTheRerunAsASentence(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	if got := strip(m.probesBody(40, 8)); !strings.Contains(got, "quit, re-run with --probe N") {
		t.Errorf("empty probes on a 40-cell column = %q, want the full sentence", got)
	}
}

// p and t only have a visible effect with engines (or agents, for t). The
// compact strip already hides them; the full footer must match.
// The mid-row panels clip to a row budget, and a half-drawn engine reads as a
// sixth engine on a fleet of five: the body must stop at whole blocks and the
// title must count what the body drew, not what the budget could guess.
func TestMidRowPanelsCountOnlyWholeEngines(t *testing.T) {
	provs := make([]core.ProviderSnapshot, 5)
	for i := range provs {
		provs[i] = core.ProviderSnapshot{Label: fmt.Sprintf("engine-%d", i), OK: true,
			Models: []core.ModelInfo{{SizeVRAM: 8 << 30}}, TTFTms: 120}
	}
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: provs}
	// Seven rows is what the mid row gets on the default pane: three two-row
	// engine blocks fit, three three-row state blocks plus a spacer fit.
	body, shown := m.providersBody(40, 7)
	if shown != 3 {
		t.Errorf("ENGINES drew %d blocks into 7 rows, want 3", shown)
	}
	if rows := len(strings.Split(strings.TrimRight(body, "\n"), "\n")); rows != 6 {
		t.Errorf("ENGINES wrote %d rows, want 6 whole blocks:\n%s", rows, body)
	}
	// The count and the way out: a bare "+2 more" names two engines the frame
	// gives no way to reach, which reads as two the tool cannot see.
	if got := strip(m.enginesTitle(40, shown)); got != "ENGINES  +2 more (enlarge window)" {
		t.Errorf("ENGINES title = %q, want the two engines it did not draw and how to reach them", got)
	}
	state, stateShown := m.gaugesBody(40, 7)
	if stateShown != 2 {
		t.Errorf("ENGINE STATE drew %d blocks into 7 rows, want 2", stateShown)
	}
	if got := strip(m.engineStateTitle(40, stateShown)); got != "ENGINE STATE  +3 more (enlarge window)" {
		t.Errorf("ENGINE STATE title = %q, want the three engines it did not draw and how to reach them", got)
	}
	if strings.Contains(strip(state), "engine-2") {
		t.Errorf("ENGINE STATE drew a block it had no rows for:\n%s", state)
	}
}

// An engine with no memory, cpu or ttft reading has a two-row state block
// where a measured one has three, so a count derived from the row budget alone
// miscounts the panel. The title follows the body either way.
func TestEngineStateCountsShortBlocksByRow(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "a", OK: true}, {Label: "b", OK: true}, {Label: "c", OK: true},
	}}
	body, shown := m.gaugesBody(40, 8)
	if shown != 3 {
		t.Errorf("drawn %d two-row blocks into 8 rows, want 3:\n%s", shown, body)
	}
	if got := strip(m.engineStateTitle(40, shown)); got != "ENGINE STATE" {
		t.Errorf("ENGINE STATE title = %q, want no hidden-engine badge", got)
	}
	// A measured engine needs a third row, so only two of the same three fit.
	m.snap.Providers[0].TTFTms = 120
	_, shown = m.gaugesBody(40, 7)
	if shown != 2 {
		t.Errorf("drawn %d blocks into 7 rows, want 2", shown)
	}
	if got := strip(m.engineStateTitle(40, shown)); got != "ENGINE STATE  +1 more (enlarge window)" {
		t.Errorf("ENGINE STATE title = %q, want +1 more and the way to reach it", got)
	}
	// A column too narrow for the sentence keeps the count rather than losing
	// it: an engine the reader cannot account for is the worse of the two.
	if got := strip(m.engineStateTitle(22, shown)); got != "ENGINE STATE  +1 more" {
		t.Errorf("narrow ENGINE STATE title = %q, want the bare count", got)
	}
}

// A notice is the answer to a key that changed nothing. On a pane too narrow to
// carry it beside the key list it has to stand alone: the key list is what
// gives way, or the explanation is clipped off the row entirely.
func TestFooterNoticeSurvivesANarrowPane(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "x", OK: true}}}
	m.notice = "p: no engines to probe"
	m.w = minDashW
	got := strip(m.renderFooter())
	if !strings.Contains(got, "p: no engines to probe") {
		t.Errorf("notice dropped on a %d-column pane: %q", minDashW, got)
	}
	if w := lipgloss.Width(got); w > minDashW {
		t.Errorf("footer is %d columns wide on a %d-column pane: %q", w, minDashW, got)
	}
	// Where both fit, the notice answers beside the key it belongs to.
	m.w = 120
	got = strip(m.renderFooter())
	if !strings.Contains(got, "help") || !strings.Contains(got, "p: no engines to probe") {
		t.Errorf("wide footer lost either the keys or the notice: %q", got)
	}
}

func TestFooterOmitsDeadKeys(t *testing.T) {
	empty := New(Config{Version: "t", Prober: func() {}}, nil)
	foot := strip(empty.renderFooter())
	if strings.Contains(foot, "probe") || strings.Contains(foot, "timescale") {
		t.Errorf("empty footer advertised keys with no effect: %q", foot)
	}
	// The dead keys are the claim; the footer still has to carry the live
	// ones, or an empty render would satisfy the check above.
	if !strings.Contains(foot, "help") || !strings.Contains(foot, "q") {
		t.Errorf("empty footer dropped the keys that do work: %q", foot)
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

// A demo frame is reproducible from its seed alone, so the frame has to name
// it: a screenshot or a --once frame with no seed in it cannot be re-run.
func TestDemoFooterNamesSeed(t *testing.T) {
	m := New(Config{Version: "t", Demo: true, DemoSeed: 7}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "x", OK: true}}}
	got := strip(m.renderFooter())
	if !strings.Contains(got, "DEMO seed 7") {
		t.Errorf("demo footer does not name the seed: %q", got)
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

// A remote whose vitals poll is failing must stay on screen with its reason:
// a silent drop leaves the local host's numbers passing for the watched one.
func TestHeaderNamesFailingRemote(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 160, 36, true
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Sys: &core.SysSample{
			RemoteHost: "box",
			RemoteErr:  "ssh session: connection lost",
		},
	}
	out := strip(m.renderHeader())
	if !strings.Contains(out, "ssh box: ssh session: connection lost") {
		t.Errorf("header hides the failing remote:\n%s", out)
	}
	if strings.Contains(out, "via ssh:box") {
		t.Errorf("header still reports the remote as healthy:\n%s", out)
	}

	plain := PlainTextFrame(Config{Version: "t"}, m.snap)
	if !strings.Contains(plain, "not answering: ssh session: connection lost") {
		t.Errorf("plain report hides the failing remote:\n%s", plain)
	}
}

func TestProbeKeyNoopsWithoutEngines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var probes atomic.Int32
		m := New(Config{Version: "t", Prober: func() { probes.Add(1) }}, nil)
		for _, k := range []string{"p", "P"} {
			// The probe travels as a program command, so the noop has to
			// return none at all, not merely avoid calling the prober.
			nm, cmd := m.Update(keyMsg(k))
			m = nm.(Model)
			if cmd != nil {
				t.Errorf("%s returned a probe command with no engines attached: %v", k, cmd())
			}
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

// The compact strip has no panels to swap, so a silent focus flip reads as a
// dropped key. It answers instead, like the other keys with nothing to act on.
func TestFocusKeyExplainsItselfOnCompactPane(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.w, m.h, m.ready = 40, 10, true
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	nm, _ := m.Update(keyMsg("a"))
	m = nm.(Model)
	if m.focusAgents {
		t.Error("a swapped focus on a pane with no panels to swap")
	}
	if !strings.Contains(strip(m.View()), "enlarge window") {
		t.Errorf("compact pane did not explain the no-op key:\n%s", strip(m.View()))
	}
}

// A probe fired from the compact strip has no PROBES panel to report into, so
// the strip carries the result itself.
func TestProbeResultShownOnCompactPane(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.w, m.h, m.ready = 40, 10, true
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes:    []core.ProbeSample{{At: time.Now(), Model: "llama3", TokPS: 42, TTFTms: 310, OK: true}},
	}
	if out := strip(m.View()); !strings.Contains(out, "probe 310ms 42.0 tok/s") {
		t.Errorf("compact pane missing probe result:\n%s", out)
	}
}

func TestHelpFitsCompactPane(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help, m.w, m.h, m.ready = true, 40, 10, true
	out := m.View()
	assertFitsPane(t, "help", out, 40, 10)
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

// A page key that moves one row is indistinguishable from a dropped keypress on
// the pane that needs scrolling at all. Paging moves a window, and the two ends
// are one press each, so the last row is not one press per row away.
func TestHelpPagesAndJumpsToItsEnds(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.help, m.w, m.h, m.ready = true, 40, 10, true
	// An engine in the frame adds the p row, so the list outgrows the window.
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	rows := m.helpRows()
	if len(rows) <= m.helpWindow() {
		t.Fatalf("test needs a list taller than the window: %d rows, window %d",
			len(rows), m.helpWindow())
	}
	pressKey(&m, "pgdown")
	if want := min(m.helpWindow(), m.helpScrollMax()); m.helpScroll != want {
		t.Errorf("pgdown moved to row %d, want a full window (%d)", m.helpScroll, want)
	}
	if m.helpScroll < 2 {
		t.Errorf("pgdown moved %d rows, want a page not a line", m.helpScroll)
	}
	pressKey(&m, "pgup")
	if m.helpScroll != 0 {
		t.Errorf("pgup left the list at row %d, want the top", m.helpScroll)
	}
	pressKey(&m, "end")
	if m.helpScroll != m.helpScrollMax() {
		t.Errorf("end left the list at row %d, want the last (%d)", m.helpScroll, m.helpScrollMax())
	}
	if out := strip(m.View()); !strings.Contains(out, rows[len(rows)-1][1]) {
		t.Errorf("end did not reach the last row:\n%s", out)
	}
	pressKey(&m, "home")
	if m.helpScroll != 0 {
		t.Errorf("home left the list at row %d, want the top", m.helpScroll)
	}
	// The box names the jump it offers, or it is a key nobody finds.
	if out := strip(m.View()); !strings.Contains(out, "end") {
		t.Errorf("scrolled help does not name the key that reaches its end:\n%s", out)
	}
	assertFitsPane(t, "paged help", m.View(), 40, 10)
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
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	// p is documented only where it has engines to probe, same as the footer.
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	m.help, m.w, m.h, m.ready = true, 110, 36, true
	out := strip(m.View())
	if !strings.Contains(out, "real generation") {
		t.Errorf("help does not say p runs a real generation:\n%s", out)
	}
	if strings.Contains(out, "synthetic") {
		t.Errorf("help still calls the probe synthetic:\n%s", out)
	}
}

func TestKeyMapCaseInsensitiveAndDismiss(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	key := func(s string) tea.Cmd { return pressKey(&m, s) }

	// Upper-case Q quits
	if cmd := key("Q"); cmd == nil {
		t.Error("Q must quit")
	}

	// Upper-case H opens help
	key("H")
	if !m.help {
		t.Fatal("H did not open help")
	}

	// Enter dismisses help
	key("enter")
	if m.help {
		t.Error("enter did not dismiss help")
	}

	// Upper-case H opens help, Q dismisses help
	key("H")
	if !m.help {
		t.Fatal("H did not open help")
	}
	key("Q")
	if m.help {
		t.Error("Q with help open did not dismiss help")
	}

	// esc unfocuses agents without quitting
	m.focusAgents = true
	if cmd := key("esc"); cmd != nil {
		t.Error("esc while focused on agents must not quit")
	}
	if m.focusAgents {
		t.Error("esc while focused on agents did not unfocus")
	}
}

func TestProbeFeedbackInAgentsView(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.w, m.h, m.ready, m.focusAgents = 100, 36, true, true
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Agents:    []core.AgentEvent{{Agent: "codex", OutputTokens: 100}},
	}
	m.probeReq = time.Now()
	out := strip(m.View())
	if !strings.Contains(out, "probing") {
		t.Errorf("renderAgentsOnly missing probing indicator:\n%s", out)
	}
}

// The agents view has no PROBES panel, so a probe that lands there is only
// visible in the title. Without it, p leaves nothing on screen at all.
func TestProbeResultShownInAgentsViewTitle(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.w, m.h, m.ready, m.focusAgents = 100, 36, true, true
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes: []core.ProbeSample{{
			At:     time.Now(),
			Model:  "llama3",
			TokPS:  42,
			TTFTms: 310,
			OK:     true,
		}},
	}
	if out := strip(m.View()); !strings.Contains(out, "probe 310ms 42.0 tok/s") {
		t.Errorf("renderAgentsOnly missing probe result:\n%s", out)
	}

	m.snap.Probes[0].OK = false
	if out := strip(m.View()); !strings.Contains(out, "probe failed") {
		t.Errorf("renderAgentsOnly missing failed probe:\n%s", out)
	}
}

func TestPausedModelReceivesManualProbeResult(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.paused = true
	m.probeReq = time.Now().Add(-time.Second)

	nm, _ := m.Update(snapMsg(core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Probes: []core.ProbeSample{{
			At:    time.Now(),
			Model: "llama3",
			TokPS: 50.0,
			OK:    true,
		}},
	}))
	m = nm.(Model)
	if !m.probeReq.IsZero() {
		t.Error("probeReq was not cleared on probe result arrival while paused")
	}
	if len(m.snap.Probes) == 0 {
		t.Error("probe sample was discarded while paused")
	}
}

func TestFooterAdvertisesAgentsWithFlag(t *testing.T) {
	m := New(Config{Version: "t", Agents: true}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
	}
	m.w, m.h, m.ready = 110, 36, true
	out := strip(m.renderFooter())
	if !strings.Contains(out, "a agents") {
		t.Errorf("footer does not advertise 'a agents' when --agents is enabled:\n%s", out)
	}
}

// The footer hides the keys with nothing to act on; the help screen is the
// other half of the same reference. A key the footer hid but help still
// documented sent first-timers to the empty setup card pressing p, t and a
// and reading three refusals, so both surfaces read one set of conditions.
func TestHelpListsExactlyTheKeysTheFooterAdvertises(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		snap core.Snapshot
		want string // keys expected in both the footer and help
	}{
		{
			name: "no engines, no agents (the setup card)",
			cfg:  Config{Version: "t", Prober: func() {}},
			want: "",
		},
		{
			name: "engines only",
			cfg:  Config{Version: "t", Prober: func() {}},
			snap: core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}},
			want: "pt",
		},
		{
			name: "engines and agents",
			cfg:  Config{Version: "t", Prober: func() {}},
			snap: core.Snapshot{
				Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
				Agents:    []core.AgentEvent{{Agent: "claude"}},
			},
			want: "pta",
		},
		{
			name: "agents flag, engines not up yet",
			cfg:  Config{Version: "t", Prober: func() {}, Agents: true},
			snap: core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama"}}},
			want: "pta",
		},
		{
			name: "no prober in this run",
			cfg:  Config{Version: "t"},
			snap: core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}},
			want: "t",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.cfg, nil)
			m.snap = tc.snap
			m.w, m.h, m.ready = 110, 36, true
			// Only the key section: the flag list below it is names, not keys.
			listed := map[string]bool{}
			for _, r := range m.helpRows() {
				if r[0] == "" {
					break
				}
				listed[r[0]] = true
			}
			foot := map[string]bool{}
			for _, tok := range strings.Fields(strip(m.renderFooter())) {
				foot[tok] = true
			}
			for _, k := range []string{"p", "t", "a"} {
				inHelp := listed[k]
				inFoot := foot[k]
				want := strings.Contains(tc.want, k)
				if inHelp != want {
					t.Errorf("help %q = %v, want %v (footer: %v)", k, inHelp, want, inFoot)
				}
				if inFoot != want {
					t.Errorf("footer %q = %v, want %v", k, inFoot, want)
				}
			}
		})
	}
}

// The compact strip prints the probe outcome (renderMinimal calls
// probeReadout), so p acts on this layout and both the key line and the
// compact help have to name it. They advertise it under the same condition
// the full dashboard uses, so the two never disagree about a key that is
// live in one and silent in the other.
func TestCompactViewAdvertisesProbeKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		snap core.Snapshot
		want bool
	}{
		{
			name: "prober and engines",
			cfg:  Config{Version: "t", Prober: func() {}},
			snap: core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}},
			want: true,
		},
		{
			name: "no prober in this run",
			cfg:  Config{Version: "t"},
			snap: core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}},
			want: false,
		},
		{
			name: "prober, no engines yet",
			cfg:  Config{Version: "t", Prober: func() {}},
			snap: core.Snapshot{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.cfg, nil)
			m.snap = tc.snap
			m.w, m.h, m.ready = 40, 12, true // below minDashW/minDashH: the strip
			listed := false
			for _, r := range m.helpRows() {
				if r[0] == "p" {
					listed = true
				}
			}
			if listed != tc.want {
				t.Errorf("compact help lists p = %v, want %v", listed, tc.want)
			}
			foot := strings.Contains(strip(m.renderMinimal()), "p probe")
			if foot != tc.want {
				t.Errorf("compact key line has p = %v, want %v", foot, tc.want)
			}
			assertFitsPane(t, "compact frame", m.View(), m.w, m.h)
		})
	}
}

// esc quits the dashboard from every view but the agents view, and the footer
// advertises q alone. The in-app reference is the only place inside the
// product that says what esc does on the dashboard, so it has to say it: a
// reader who knows the key from another tool otherwise learns it by quitting.
func TestHelpDocumentsEscapeFromDashboard(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.help, m.w, m.h, m.ready = true, 110, 36, true
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	var desc string
	for _, r := range m.helpRows() {
		if r[0] == "esc" {
			desc = r[1]
		}
	}
	if !strings.Contains(desc, "close") || !strings.Contains(desc, "quit") {
		t.Errorf("esc row = %q, want both what it closes and what it quits", desc)
	}
}

func TestPanelTitlesShowHiddenCount(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	ps := make([]core.ProviderSnapshot, 6)
	for i := range ps {
		ps[i] = core.ProviderSnapshot{
			Label: fmt.Sprintf("engine-%d", i),
			OK:    true,
		}
	}
	m.snap = core.Snapshot{Providers: ps}
	m.w, m.h, m.ready = 110, 32, true
	out := strip(m.View())
	// Anchor on a panel title line: a bare "+" and a bare "more" are both
	// satisfied by unrelated text anywhere in the frame.
	var bad []string
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "ENGINES  ")
		if !ok {
			continue
		}
		if n, _, ok := strings.Cut(rest, " more (enlarge window)"); !ok || n == "" || strings.TrimLeft(n, "0123456789") != "" {
			bad = append(bad, line)
		}
	}
	if len(bad) != 1 {
		t.Fatalf("panel titles with overflow = %v, want exactly one ENGINES title carrying a hidden count:\n%s", bad, out)
	}
}

// A key the footer cannot advertise (nothing to probe, nothing to plot, no
// engines to swap to) used to be swallowed. A user who remembers it from
// another run gets a dropped-keystroke read instead, so each one answers on
// the footer line and then times itself out.
func TestInertKeysExplainThemselves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := New(Config{Version: "t", Prober: func() {}}, nil)
		m.w, m.h, m.ready = 110, 36, true

		for _, k := range []string{"p", "t", "a"} {
			m.notice, m.noticeAt = "", time.Time{}
			nm, _ := m.Update(keyMsg(k))
			m = nm.(Model)
			if m.notice == "" {
				t.Errorf("%s with nothing to act on set no notice", k)
			}
			if !strings.Contains(strip(m.renderFooter()), strip(m.notice)) {
				t.Errorf("footer hides the notice for %s:\n%s", k, strip(m.renderFooter()))
			}
		}

		// The notice is a beat, not a label: it clears on its own.
		nm, _ := m.Update(tickMsg(m.clock.Add(noticeTTL)))
		m = nm.(Model)
		if m.notice != "" {
			t.Errorf("notice %q outlived %s", m.notice, noticeTTL)
		}
	})
}

// The same explanation has to fit where the compact view prints it: the foot
// there is one clipped line wide, so the notice goes in the body.
func TestInertKeyNoticeInCompactStrip(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.w, m.h, m.ready = 50, 24, true
	nm, _ := m.Update(keyMsg("p"))
	m = nm.(Model)
	out := strip(m.renderMinimal())
	if !strings.Contains(out, "no engines to probe") {
		t.Errorf("compact strip does not explain an inert p:\n%s", out)
	}
}

// The notice is a beat measured in wall time, not on the display clock: a
// pause freezes clock, so a notice raised while paused used to age out on the
// very next tick and read as a dropped keystroke, which is what it exists to
// prevent.
func TestInertKeyNoticeSurvivesAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := New(Config{Version: "t", Prober: func() {}}, nil)
		m.w, m.h, m.ready = 110, 36, true
		nm, _ := m.Update(keyMsg(" ")) // pause: clock stops advancing
		m = nm.(Model)

		nm, _ = m.Update(keyMsg("p")) // nothing to probe
		m = nm.(Model)
		if m.notice == "" {
			t.Fatal("p with nothing to act on set no notice")
		}

		// A second of wall time, with clock still where the pause left it.
		nm, _ = m.Update(tickMsg(m.tickAt.Add(time.Second)))
		m = nm.(Model)
		if m.notice == "" {
			t.Fatalf("notice expired 1s after the key press while paused: %q", m.notice)
		}
		if !strings.Contains(strip(m.renderFooter()), strip(m.notice)) {
			t.Errorf("paused footer hides the notice:\n%s", strip(m.renderFooter()))
		}

		nm, _ = m.Update(tickMsg(m.tickAt.Add(noticeTTL)))
		m = nm.(Model)
		if m.notice != "" {
			t.Errorf("notice %q outlived %s", m.notice, noticeTTL)
		}
	})
}

// t flips the throughput chart, which the compact strip does not draw, so on a
// pane too small for the dashboard the key would otherwise change nothing.
func TestTimescaleKeyExplainsItselfInCompactStrip(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	m.w, m.h, m.ready = 50, 24, true
	nm, _ := m.Update(keyMsg("t"))
	m = nm.(Model)
	if !m.chartCompressed {
		t.Error("t flipped the timescale on a pane with no chart")
	}
	out := strip(m.renderMinimal())
	if !strings.Contains(out, "enlarge window") {
		t.Errorf("compact strip does not explain an inert t:\n%s", out)
	}
}

// With an engine attached these are not inert: t must still flip the
// timescale and a must still reach the agents view.
func TestLiveKeysStillWorkWithoutNotice(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	m.w, m.h, m.ready = 110, 36, true
	nm, _ := m.Update(keyMsg("t"))
	m = nm.(Model)
	if m.chartCompressed {
		t.Error("t did not toggle the timescale with an engine attached")
	}
	if m.notice != "" {
		t.Errorf("t raised a notice with an engine attached: %q", m.notice)
	}
	nm, _ = m.Update(keyMsg("a"))
	m = nm.(Model)
	if !m.focusAgents {
		t.Error("a did not focus agents with an engine attached")
	}
}

// The overlay replaces the whole frame, so it titles itself the way every
// panel does; an untitled box reads as a fragment with no anchor.
func TestHelpOverlayIsTitled(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.help, m.w, m.h, m.ready = true, 110, 36, true
	if out := strip(m.View()); !strings.Contains(out, "KEYS") {
		t.Errorf("help overlay has no title:\n%s", out)
	}
	small := New(Config{Version: "t"}, nil)
	small.help, small.w, small.h, small.ready = true, 40, 10, true
	if out := strip(small.View()); !strings.Contains(out, "KEYS") {
		t.Errorf("compact help overlay has no title:\n%s", out)
	}
}

// The PROBES panel is 31% of the pane, so on the narrowest dashboard it has
// 16 cells. The two hints it prints while no probe has run are the only
// instruction that panel ever gives, and at the longer spellings both ended in
// an ellipsis exactly where the dashboard is smallest.
func TestProbeEmptyHintsFitTheNarrowestProbesColumn(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}}}
	inner := minDashW - minDashW*38/100 - minDashW*31/100 - 4
	for _, line := range strings.Split(strip(m.probesBody(inner, 8)), "\n") {
		if line == "" {
			continue
		}
		if w := lipgloss.Width(line); w > inner {
			t.Errorf("probe hint is %d cells in a %d-cell column: %q", w, inner, line)
		}
	}
}

// A demo tag plus every key hint overruns a narrow pane. The row used to be
// clipped at the pane edge, which took "? help" with it: the one key that
// leads to the reference listing the others.
func TestFooterKeepsHelpOnANarrowPane(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}, Demo: true, DemoSeed: 42}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{Label: "ollama", OK: true}},
		Agents:    []core.AgentEvent{{Agent: "claude"}},
	}
	m.w, m.h, m.ready = minDashW, 32, true
	got := strip(m.renderFooter())
	if !strings.Contains(got, "? help") {
		t.Errorf("footer lost the help hint on a %d-column pane: %q", minDashW, got)
	}
	if !strings.Contains(got, "q quit") || !strings.Contains(got, "DEMO seed 42") {
		t.Errorf("footer lost a key that fits: %q", got)
	}
	if w := lipgloss.Width(got); w > minDashW {
		t.Errorf("footer is %d cells on a %d-column pane: %q", w, minDashW, got)
	}
}

// The agents view is fed by the local session-log watch under --agents and by
// harness events POSTed to the ingest endpoint without it. The title named the
// first on both, so a run that never read a session log claimed to have.
func TestAgentsTitleNamesWhereTheTokensCameFrom(t *testing.T) {
	events := []core.AgentEvent{{Agent: "claude", OutputTokens: 10, At: time.Now()}}
	local := New(Config{Version: "t", Agents: true}, nil)
	local.snap = core.Snapshot{Agents: events}
	local.w, local.h, local.ready = 110, 36, true
	if out := strip(local.View()); !strings.Contains(out, "session logs") {
		t.Errorf("agents view does not name the local session-log source:\n%s", out)
	}
	fed := New(Config{Version: "t", IngestAddr: "127.0.0.1:8420"}, nil)
	fed.snap = core.Snapshot{Agents: events}
	fed.w, fed.h, fed.ready = 110, 36, true
	out := strip(fed.View())
	if !strings.Contains(out, "ingest endpoint") {
		t.Errorf("agents view does not name the ingest source:\n%s", out)
	}
	if strings.Contains(out, "session logs") {
		t.Errorf("agents view claims a session-log watch this run never made:\n%s", out)
	}
}

// ENGINES draws as many blocks as the row budget holds and names the rest only
// as a count, so the engine at the end of the collector's order is the one
// that disappears on a short pane. A failed engine is the one row that carries
// the reason, so the panel draws the down engines first; the healthy ones keep
// their order and still line up with ENGINE STATE beside them.
func TestEnginesPanelDrawsFailuresFirst(t *testing.T) {
	provs := make([]core.ProviderSnapshot, 5)
	for i := range provs {
		provs[i] = core.ProviderSnapshot{Label: fmt.Sprintf("engine-%d", i), OK: true,
			Models: []core.ModelInfo{{Name: "m"}}}
	}
	provs[4] = core.ProviderSnapshot{Label: "engine-4", Err: "connection refused"}
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: provs}
	// Three blocks is all this budget holds.
	body, shown := m.providersBody(60, 6)
	if shown != 3 {
		t.Fatalf("drew %d engines, want 3", shown)
	}
	if !strings.Contains(strip(body), "engine-4") {
		t.Errorf("the failed engine was the block that got cut:\n%s", strip(body))
	}
	if !strings.Contains(strip(body), "connection refused") {
		t.Errorf("the failed engine's reason did not reach the panel:\n%s", strip(body))
	}
	// The healthy pair keeps the collector's order, so the panel still reads
	// across to ENGINE STATE.
	if first, second, _ := strings.Cut(strip(body), "engine-0"); !strings.Contains(second, "engine-1") || first == "" {
		t.Errorf("healthy engines reordered:\n%s", strip(body))
	}
}

// The kind badge names a category two engines share ("vllm" on two ports), so
// the engine's own label is what the block needs; a label that only repeats
// the badge is noise.
func TestEngineBlockNamesTheEngine(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{Providers: []core.ProviderSnapshot{
		{Label: "vllm-a100", Kind: core.KindVLLM, OK: true, Models: []core.ModelInfo{{Name: "Qwen"}}},
		{Label: "vllm-b200", Kind: core.KindVLLM, OK: true, Models: []core.ModelInfo{{Name: "Llama"}}},
		{Label: "ollama", Kind: core.KindOllama, OK: true, Models: []core.ModelInfo{{Name: "gemma"}}},
	}}
	body, _ := m.providersBody(60, 8)
	for _, want := range []string{"vllm-a100", "vllm-b200", "gemma"} {
		if !strings.Contains(strip(body), want) {
			t.Errorf("ENGINES block does not carry %q:\n%s", want, strip(body))
		}
	}
	// The ollama row names the kind once, not twice.
	row, _, _ := strings.Cut(strip(body), "gemma")
	if n := strings.Count(row, "ollama"); n != 1 {
		t.Errorf("label repeating the kind badge printed %d times:\n%s", n, row)
	}
}

// The queue counts and the probe measurement are drawn from one row each and
// appear in no other panel. The kv bar used to be sized from the pane alone, so
// at the narrowest legal dashboard the "wait" half of a backing-up queue and
// the ttft of a probe were cut off the right edge, and the number the panel
// exists to show was the number that went missing.
func TestNarrowColumnsKeepTheMeasurements(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.snap = core.Snapshot{
		Providers: []core.ProviderSnapshot{{
			Label: "ollama", Kind: core.KindOllama, OK: true, KVPct: 14,
			OutTokPS: 114, InTokPS: 416, Running: 2, Waiting: 3,
		}},
		Probes: []core.ProbeSample{{
			At: time.Now(), Model: "llama3.1:8b-instruct-q4_K_M", OK: true, TokPS: 41.2, TTFTms: 104,
		}},
	}
	// The two columns on the smallest legal dashboard: ENGINES at 38% of the
	// pane, PROBES at what is left of it.
	engines := minDashW*38/100 - 4
	probes := minDashW - minDashW*38/100 - minDashW*31/100 - 4
	engineBody, _ := m.providersBody(engines, 2)
	engineRow := strip(engineBody)
	for _, want := range []string{"run 2", "wait 3"} {
		if !strings.Contains(engineRow, want) {
			t.Errorf("engine row in a %d-cell column lost %q:\n%s", engines, want, engineRow)
		}
	}
	probeRow := ""
	for _, ln := range strings.Split(strip(m.probesBody(probes, 8)), "\n") {
		if strings.Contains(ln, "✓") {
			probeRow = ln
		}
	}
	if probeRow == "" {
		t.Fatalf("PROBES drew no successful probe row in a %d-cell column", probes)
	}
	for _, want := range []string{"41.2", "104ms"} {
		if !strings.Contains(probeRow, want) {
			t.Errorf("probe row in a %d-cell column lost %q:\n%s", probes, want, probeRow)
		}
	}
}

// The compact view's orientation line is the only place it says why it is
// compact. It was clipped to the pane, which cut the sentence exactly where
// the minimum it names sat ("min 62×" with the number gone), so the pane has
// to pick a form that fits whole.
func TestMinimalHintFitsThePane(t *testing.T) {
	for _, w := range []int{20, 30, 40, 60, 80} {
		m := New(Config{Version: "t"}, nil)
		m.w, m.h, m.ready = w, 12, true
		hint := m.minimalHint()
		if got := lipgloss.Width(hint); got > w {
			t.Errorf("hint is %d cells in a %d-cell pane: %q", got, w, hint)
		}
		if !strings.Contains(hint, "62×30") {
			t.Errorf("hint in a %d-cell pane lost the minimum: %q", w, hint)
		}
	}
}

// The braille plots are the one thing on the frame that no assistive
// technology can read, so the one number that gives the curve its shape is
// spelled out in the title. A title is not clipped by panel(), so a peak that
// does not fit is dropped whole rather than stretched across the pane.
func TestThroughputTitleNamesChartPeak(t *testing.T) {
	m := New(Config{Version: "t"}, nil)
	m.aggLast = 12
	if got := strip(m.throughputTitle(120, 45)); !strings.Contains(got, "peak ▲45.0 tok/s") {
		t.Errorf("title = %q, want the chart's peak as text", got)
	}
	if got := strip(m.throughputTitle(120, 0)); strings.Contains(got, "peak") {
		t.Errorf("title = %q, want no peak for a window that never rose", got)
	}
	// A pane too narrow for the base title plus the peak gets the base alone:
	// panel() does not clip a title, so an over-wide one stretches every row
	// below it past the pane.
	if got := strip(m.throughputTitle(60, 45000)); strings.Contains(got, "peak") {
		t.Errorf("title = %q, want the peak dropped rather than the pane stretched", got)
	}
}

// The same alternative in the linear report: peaks and the window they were
// measured over, since the rates on the status line only say "now".
func TestPlainFrameNamesChartPeak(t *testing.T) {
	now := time.Now()
	s := core.Snapshot{
		At: now,
		Providers: []core.ProviderSnapshot{{
			Label: "vllm", Kind: "vllm", OK: true,
			OutTokPS: 20, InTokPS: 5,
			// One sample per cadence bucket, so the peak is the tallest column
			// rather than every sample landing on the same one.
			OutHist:   []float64{10, 20, 30},
			OutStamps: []time.Time{now.Add(-2 * time.Second), now.Add(-time.Second), now},
			InHist:    []float64{1, 2, 3},
			InStamps:  []time.Time{now.Add(-2 * time.Second), now.Add(-time.Second), now},
		}},
	}
	out := PlainTextFrame(Config{Version: "t"}, s)
	if !strings.Contains(out, "THROUGHPUT") || !strings.Contains(out, "30.0 tok/s out") ||
		!strings.Contains(out, "3.0 tok/s in") {
		t.Errorf("plain frame missing the chart peaks:\n%s", out)
	}
	if strings.Contains(PlainTextFrame(Config{Version: "t"}, core.Snapshot{At: now}),
		"THROUGHPUT") {
		t.Error("plain frame printed a peak section for a frame with no rate at all")
	}
}

// A pane too short for the whole key reference used to clip the tail, which
// held every flag, with no key that could reach them. It scrolls instead, and
// says which way there is more.
func TestHelpScrollsToTheLastRow(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.help, m.w, m.h, m.ready = true, 40, 8, true
	rows := m.helpRows()
	if len(rows) <= m.helpWindow() {
		t.Fatalf("test needs a list taller than the window: %d rows, window %d",
			len(rows), m.helpWindow())
	}
	first := strip(m.View())
	if !strings.Contains(first, "more") {
		t.Errorf("truncated help says nothing about the rows it hides:\n%s", first)
	}
	for pressKey(&m, "down"); m.helpScroll < m.helpScrollMax(); {
		pressKey(&m, "down")
	}
	last := rows[len(rows)-1]
	if out := strip(m.View()); !strings.Contains(out, last[0]) || !strings.Contains(out, last[1]) {
		t.Errorf("scrolled to the end and %q is still off screen:\n%s", last[0], out)
	}
	assertFitsPane(t, "scrolled help", m.View(), 40, 8)
	// A pane that fits the list never grows a scroll affordance.
	m.helpScroll = 0
	m.w, m.h = 80, 40
	if out := strip(m.View()); strings.Contains(out, "more") {
		t.Errorf("help in a full-size pane advertises scrolling:\n%s", out)
	}
}

// Up and down are muted with every other action key, so the one that scrolls
// has to be back inside the box when it is reopened.
func TestHelpScrollResetsOnReopen(t *testing.T) {
	m := New(Config{Version: "t", Prober: func() {}}, nil)
	m.w, m.h, m.ready = 40, 8, true
	pressKey(&m, "?")
	for pressKey(&m, "down"); m.helpScroll > 0; pressKey(&m, "up") {
		pressKey(&m, "up")
	}
	pressKey(&m, "down")
	pressKey(&m, "esc")
	pressKey(&m, "?")
	if m.helpScroll != 0 {
		t.Errorf("reopened help at row %d, want the top of the list", m.helpScroll)
	}
}

// "Not recent" and "no timeline to judge against" are different claims, so an
// undated agent must not leave a blank cell a reader takes for the first.
func TestAgentRowNamesUnknownRecency(t *testing.T) {
	rows := agentRows([]core.AgentRate{{Agent: "claude", Tokens: 10}}, time.Now())
	if !strings.Contains(strip(rows[0]), "time unknown") {
		t.Errorf("agent row = %q, want the undated third state spelled out", strip(rows[0]))
	}
	out := PlainTextFrame(Config{Version: "t", Agents: true}, core.Snapshot{
		Agents: []core.AgentEvent{{Agent: "claude", OutputTokens: 5}},
	})
	if !strings.Contains(out, "time unknown") {
		t.Errorf("plain report drops the undated recency:\n%s", out)
	}
}

// The header clock shows the launch time until the first tick lands, so a
// demo run has to launch at its pinned origin: the frame a replay draws
// before the first collector frame must read the same simulated time as every
// frame after it.
func TestNewSeedsHeaderClockFromDemoOrigin(t *testing.T) {
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := New(Config{Version: "t", Demo: true, DemoSeed: 7, DemoOrigin: origin}, nil)
	if !m.clock.Equal(origin) {
		t.Errorf("header clock = %v, want the pinned demo origin %v", m.clock, origin)
	}
	if !m.tickAt.Equal(origin) {
		t.Errorf("tick clock = %v, want the pinned demo origin %v", m.tickAt, origin)
	}

	// A live run has no origin to pin to and still starts on the wall clock.
	live := New(Config{Version: "t"}, nil)
	if live.clock.IsZero() {
		t.Error("live header clock is zero")
	}
}
