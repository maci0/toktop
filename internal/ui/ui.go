// Package ui renders the toktop dashboard.
package ui

import (
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// Config wires the dashboard to its data source.
type Config struct {
	Version string
	Demo    bool
	// DemoSeed is the seed the demo source draws from, shown next to the
	// DEMO tag: a demo frame is reproducible from it, so the frame itself
	// has to say which run produced it.
	DemoSeed   int64
	IngestAddr string
	PollEvery  time.Duration // sampling cadence; anchors the chart timescale
	Prober     func()        // nil disables manual probing
	// Agents reports that local agent watching (--agents) is on: only then
	// may empty-feed guidance promise that running agents are picked up.
	Agents bool
	// FeedErr receives a message each time the agent event stream
	// degrades after startup: the ingest endpoint dying, or the agent watch
	// refusing to read a monitored engine's address. Every message names its
	// own subsystem, because it is rendered verbatim in the feed panel.
	// nil (or silent) means it is up: stderr is invisible under the alternate
	// screen, so without this in-band signal the UI would advertise a dead
	// endpoint forever, or silently double-count against a bad address.
	FeedErr <-chan string
}

// Model is the bubbletea dashboard: it holds the newest snapshot, the
// viewport size and the transient state a frame cannot be rebuilt from
// (pause, help, which panel estate is in focus, a pending notice).
type Model struct {
	cfg         Config
	ch          <-chan core.Snapshot
	snap        core.Snapshot
	w, h        int
	ready       bool
	paused      bool
	help        bool
	focusAgents bool // agents get the panel estate; engines keep header, charts, strip
	clock       time.Time
	// tickAt is the newest wall time bubbletea delivered, which keeps
	// advancing while clock is frozen by a pause. Timers that must keep
	// running read it, not clock: a notice timed against clock expires on
	// its very next tick once the frame has been paused for a while.
	tickAt          time.Time
	aggMax          float64
	aggLast         float64 // most recent aggregate output rate across engines
	chartCompressed bool
	probeReq        time.Time // manual probe awaiting its first result
	feedDown        string    // set once the agent event stream has degraded
	notice          string    // one-shot explanation of a key that changed nothing
	noticeAt        time.Time
	// sum is the agent feed accounted once for the frame being drawn. The
	// header, charts, feed, footer and agents view each need a different
	// slice of it, and the feed holds up to AgentHistoryLen events, so
	// walking it per consumer cost five groupings and five NFC-normalized
	// maps a frame. View fills it; agentSum computes on demand for a
	// consumer called outside a frame.
	sum *core.AgentSummary
}

// New builds the dashboard model over a snapshot stream. ch carries the
// collector's frames; a frame that has not arrived yet is what the warm-up
// glyph stands in for, so the first render is never a frame of zeroes.
func New(cfg Config, ch <-chan core.Snapshot) Model {
	// The header clock only advances on ticks, so until the first one lands
	// (~1s in) it must show the launch time rather than a zero-value midnight.
	now := time.Now()
	return Model{cfg: cfg, ch: ch, chartCompressed: chartCompressedDefault, clock: now, tickAt: now}
}

// noticeTTL is how long a "that key does nothing here" explanation stays on
// the footer line. Long enough to read: the notice answers a key that changed
// nothing on screen, so the reader has to look away from where they were
// looking and find the footer first. Short enough not to become the footer.
const noticeTTL = 6 * time.Second

// probeTimeout is how long the "probing…" marker waits for its first result
// before giving up, measured on the display clock so a paused frame holds the
// marker until it is resumed. Generous enough to cover a slow first token on a
// cold model.
const probeTimeout = 15 * time.Second

// setNotice sets the transient footer explanation for a key press that had no
// effect. The footer hides keys with nothing to act on, but a user who
// remembers them from another run still presses them; silence reads as a
// dropped keystroke, and the reason is what they are missing. It is stamped
// on the tick clock, not the display clock, so a pause does not age it out
// before it has been read.
func (m *Model) setNotice(s string) {
	at := m.tickAt
	if at.IsZero() {
		at = m.clock
	}
	m.notice, m.noticeAt = s, at
}

// StaticFrame renders one snapshot for non-interactive output (--once).
func StaticFrame(cfg Config, s core.Snapshot, w, h int) string {
	m := New(cfg, nil)
	m.snap = s
	m.w, m.h = w, h
	m.ready = true
	m.clock = frameNow(s, time.Time{})
	if agg := aggOutAt(s, m.clock); agg > 0 {
		m.aggLast = agg
		m.aggMax = agg
	}
	return m.View()
}

// --- messages -------------------------------------------------------------

type snapMsg core.Snapshot
type tickMsg time.Time

// feedDownMsg reports the agent event stream degraded after startup; the
// payload is the producer's own subsystem-prefixed message, rendered
// verbatim.
type feedDownMsg string

func waitSnap(ch <-chan core.Snapshot) tea.Cmd {
	return func() tea.Msg {
		snap, ok := <-ch
		if !ok {
			return nil
		}
		return snapMsg(snap)
	}
}

// waitFeedErr blocks until the feed dies; re-issued after each delivery so a
// restart-and-resignal cycle is still observed. A nil channel never fires.
func waitFeedErr(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		err, ok := <-ch
		if !ok {
			return nil
		}
		return feedDownMsg(err)
	}
}

func tickClock() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- model ----------------------------------------------------------------

// Init starts the header clock, the snapshot wait and, when the feed can
// degrade, the channel that reports it. Each waits in a goroutine bubbletea
// owns, so none of them blocks the first render.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickClock(), waitSnap(m.ch)}
	if m.cfg.FeedErr != nil {
		cmds = append(cmds, waitFeedErr(m.cfg.FeedErr))
	}
	return tea.Batch(cmds...)
}

// Update folds one message (keypress, tick, snapshot, resize) into the model.
// A snapshot taken while the frame is paused updates the probe results and
// nothing else, so pausing freezes the whole frame rather than only the
// header clock.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case tickMsg:
		m.tickAt = time.Time(msg)
		// A paused frame must be genuinely still: the header clock is the
		// only element that kept changing every second, churning the screen
		// for anyone pausing to read it with a screen reader or magnifier.
		if !m.paused {
			m.clock = time.Time(msg)
		}
		// Engines that never answer (no known model yet, all down) would
		// leave the "probing…" marker up forever without this bail-out.
		// On the display clock, not wall time: a paused frame is meant to be
		// still, so the marker holds until the frame is resumed.
		if !m.probeReq.IsZero() && m.clock.Sub(m.probeReq) > probeTimeout {
			m.probeReq = time.Time{}
		}
		if !m.noticeAt.IsZero() && time.Time(msg).Sub(m.noticeAt) >= noticeTTL {
			m.notice, m.noticeAt = "", time.Time{}
		}
		return m, tickClock()

	case feedDownMsg:
		m.feedDown = string(msg)
		return m, waitFeedErr(m.cfg.FeedErr)

	case snapMsg:
		in := core.Snapshot(msg)
		if n := len(in.Probes); n > 0 && in.Probes[n-1].At.After(m.probeReq) {
			m.probeReq = time.Time{} // first result landed: hand over to it
			if m.paused {
				m.snap.Probes = in.Probes
			}
		}
		if !m.paused {
			agg := aggOutAt(in, frameNow(in, m.clock))
			m.aggLast = agg
			if agg > m.aggMax {
				m.aggMax = agg
			}
			m.snap = in
		}
		return m, waitSnap(m.ch)

	case tea.KeyMsg:
		key := msg.String()
		// Help is a full-screen replacement view: action keys must not act
		// blind on the dashboard it covers (space silently paused mid-read,
		// p fired real probe generations). Only the dismiss and toggle keys
		// stay live while help is up.
		if m.help {
			switch key {
			case "q", "Q", "ctrl+c", "esc", "?", "h", "H", "enter":
				m.help = false
				return m, nil
			default:
				return m, nil
			}
		}
		switch key {
		case "q", "Q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.focusAgents {
				m.focusAgents = false
				return m, nil
			}
			return m, tea.Quit
		case " ", "space":
			m.paused = !m.paused
			return m, nil
		case "p", "P":
			// No engines: ProbeAll is a silent no-op and the PROBES panel
			// (where "probing…" lives) is absent. Say why rather than look dead.
			switch {
			case m.cfg.Prober == nil:
				m.setNotice("p: probing is not available in this run")
			case len(m.snap.Providers) == 0:
				m.setNotice("p: no engines to probe")
			default:
				// A command, not a bare goroutine: the program owns this
				// task's lifetime, so a held key (terminals repeat key
				// events) queues dispatches the program can account for
				// instead of spawning an untracked goroutine per press.
				prober := m.cfg.Prober
				m.probeReq = m.clock
				return m, func() tea.Msg {
					prober()
					return nil // no result to fold back into the model
				}
			}
			return m, nil
		case "t", "T":
			if len(m.snap.Providers) == 0 && len(m.snap.Agents) == 0 {
				m.setNotice("t: no throughput to plot yet")
				return m, nil
			}
			// The compact strip draws no chart, so flipping the timescale there
			// changes nothing on screen. Same rule as the hidden keys in
			// renderMinimal: say so rather than read as a dropped keystroke.
			if m.w < minDashW || m.h < minDashH {
				m.setNotice("t: enlarge window, the timescale has no chart here")
				return m, nil
			}
			m.chartCompressed = !m.chartCompressed
			return m, nil
		case "a", "A":
			// Which side gets the panel estate. The other side keeps the
			// header, the shared throughput chart and the host strip, so the
			// frame never hides half the machine to show the other half.
			// Without engines the agents view is already the view.
			if len(m.snap.Providers) == 0 && !m.focusAgents {
				m.setNotice("a: no engines to switch to")
				return m, nil
			}
			// The compact strip draws no panels to swap, so the focus flip
			// would change nothing on screen. Same rule as the hidden keys
			// in renderMinimal: say so rather than read as a dropped key.
			if m.w < minDashW || m.h < minDashH {
				m.setNotice("a: enlarge window, there are no panels to swap here")
				return m, nil
			}
			m.focusAgents = !m.focusAgents
			return m, nil
		case "?", "h", "H":
			m.help = !m.help
			return m, nil
		}
	}
	return m, nil
}

// --- view ------------------------------------------------------------------

// View draws one frame at the model's current size. It fills the agent
// summary first, since the header, charts, feed and agents view each read a
// different slice of it and the retained feed is long enough that walking it
// per consumer shows up in a frame.
func (m Model) View() string {
	if !m.ready {
		// Same glyph the probe panel uses for work in progress: the status
		// vocabulary stays monochrome terminal glyphs, no color emoji.
		return "\n  " + styleWarn.Render("● toktop is warming up…")
	}
	// Account the agent feed once, before any consumer reads it.
	m.sum = new(core.AgentSummary)
	if len(m.snap.Agents) > 0 {
		s := core.Summarize(m.snap.Agents, m.snapNow())
		m.sum = &s
	}
	if m.help {
		return m.renderHelp()
	}
	if m.w < minDashW || m.h < minDashH {
		return m.renderMinimal()
	}
	if len(m.snap.Providers) == 0 {
		// Agents reporting over the ingest endpoint are real activity even
		// without --agents, and a run that asked for --agents gets the agents
		// dashboard before the first event lands: the setup card complains
		// about engines nobody asked for, while the waiting panel says so
		// itself.
		if len(m.snap.Agents) > 0 || m.cfg.Agents {
			return m.renderAgentsOnly()
		}
		return m.renderEmpty()
	}
	if m.focusAgents {
		return m.renderAgentsOnly()
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		"",
		m.renderCharts(),
		m.renderSystem(),
		m.renderMidRow(),
		m.renderFeed(),
	)
	return composeFrame(body, m.renderFooter(), m.w, m.h)
}

// --- helpers ---------------------------------------------------------------

// composeFrame pads body so the footer sits on the last row, then clips every
// line to the pane: bubbletea wraps an over-wide line and drags every row
// below it out of alignment. Concatenating the footer onto unpadded body
// made it ride the last content line.
func composeFrame(body, footer string, w, h int) string {
	if gap := h - lipgloss.Height(body) - lipgloss.Height(footer) - 1; gap > 0 {
		body += strings.Repeat("\n", gap)
	}
	return clipBlock(body+"\n"+footer, w, -1)
}

// clipBlock clips s to at most w visible columns and, when h >= 0, at most
// h rows. h < 0 means no row cap. Extra rows are dropped from
// the bottom so a header or title already on screen stays put.
func clipBlock(s string, w, h int) string {
	if h == 0 || w < 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if h > 0 && len(lines) > h {
		lines = lines[:h]
	}
	for i, ln := range lines {
		lines[i] = clip(ln, w)
	}
	return strings.Join(lines, "\n")
}

func (m Model) upCount() (up, total int) {
	for _, p := range m.snap.Providers {
		total++
		if p.OK {
			up++
		}
	}
	return up, total
}

func (m Model) lastProbe() (core.ProbeSample, bool) {
	if n := len(m.snap.Probes); n > 0 {
		return m.snap.Probes[n-1], true
	}
	return core.ProbeSample{}, false
}

func kvHeat(v float64) lipgloss.Color {
	if math.IsNaN(v) || v < 60 {
		return cGreen
	}
	switch {
	case v < 85:
		return cYellow
	default:
		return cRed
	}
}

// snapNow is the instant rate windows and idle spans use: the snapshot's
// own stamp when the collector filled one in, otherwise the header clock.
// The header clock itself stays on ticks so it still advances for the
// operator while a slow scrape holds At still.
func (m Model) snapNow() time.Time {
	return frameNow(m.snap, m.clock)
}

// frameNow is the instant a snapshot treats as "now": its own stamp when
// the collector filled one in, otherwise fallback (the UI clock). A zero
// fallback stays zero: wall time would slide --once/--plain agent windows
// against a clock the snapshot does not share.
func frameNow(s core.Snapshot, fallback time.Time) time.Time {
	if !s.At.IsZero() {
		return s.At
	}
	return fallback
}

// agentSum is the frame's agent summary, computed on demand for a consumer
// called outside View (a test, a direct render call).
func (m Model) agentSum() core.AgentSummary {
	if m.sum != nil {
		return *m.sum
	}
	return core.Summarize(m.snap.Agents, m.snapNow())
}

// agentRates is the frame's per-agent list, busiest first.
func (m Model) agentRates() []core.AgentRate { return m.agentSum().Rates }

func aggOutAt(s core.Snapshot, now time.Time) float64 {
	out, _ := aggBothAt(s, now)
	return out
}

// aggInAt is the input-side half of aggBothAt, for a call site needing one
// direction without the output half.
func aggInAt(s core.Snapshot, now time.Time) float64 {
	_, in := aggBothAt(s, now)
	return in
}

// aggBothAt sums provider rates with unattributed agent rates in one pass, so
// the output and input halves of a snapshot cost one walk of the agent feed
// rather than two.
func aggBothAt(s core.Snapshot, now time.Time) (out, in float64) {
	return aggBoth(s, core.Summarize(s.Agents, now))
}

// aggBoth adds a feed summary's unattributed rates to the provider totals.
func aggBoth(s core.Snapshot, sum core.AgentSummary) (out, in float64) {
	for _, p := range s.Providers {
		out += p.OutTokPS
		in += p.InTokPS
	}
	for _, r := range sum.Own {
		out += r.TokPS
		in += r.PromptPS
	}
	return out, in
}

// aggOwn sums the unattributed agent rates a frame already accounted.
func (m Model) aggOwn() (out, in float64) {
	for _, r := range m.agentSum().Own {
		out += r.TokPS
		in += r.PromptPS
	}
	return
}

// aggIn is the header's input total: provider rates plus the feed's
// unattributed share, off the frame's own walk.
func (m Model) aggIn() float64 {
	in := m.aggInProviders()
	_, aIn := m.aggOwn()
	return in + aIn
}

func (m Model) aggInProviders() float64 {
	var in float64
	for _, p := range m.snap.Providers {
		in += p.InTokPS
	}
	return in
}

// uniqueAgents counts distinct agent names. The key is normalized like every
// other identity field in the UI (core.AgentRates groups under NFC), so the
// same agent recorded as "café" both ways is one agent and the header agrees
// with the feed below it.
func uniqueAgents(events []core.AgentEvent) int {
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.Agent != "" {
			seen[core.CanonicalAgent(ev.Agent)] = true
		}
	}
	return len(seen)
}

// aggHist sums every provider's history onto one absolute time grid of w
// columns ending at the newest sample anywhere. Every sample carries the
// instant it was taken, so engines that joined late, dropped out, or were
// scraped slowly cannot stretch or compress the visible window, and an engine
// that fell silent leaves a real gap rather than a fabricated one.
func aggHist(s core.Snapshot, out bool, w int, cadence time.Duration) []float64 {
	type src struct {
		vals []float64
		ts   []time.Time
	}
	var srcs []src
	var end time.Time
	for i := range s.Providers {
		p := &s.Providers[i]
		vals, ts := p.OutHist, p.OutStamps
		if !out {
			vals, ts = p.InHist, p.InStamps
		}
		if len(vals) == 0 {
			continue
		}
		srcs = append(srcs, src{vals, ts})
		if last := lastSampleTime(ts, len(vals)); last.After(end) {
			end = last
		}
	}
	// Timed and uniform paths both end at the newest sample anywhere; the
	// scan is one helper so the two modes cannot disagree about "newest".
	if aend := agentHistEnd(s.Agents); aend.After(end) {
		end = aend
	}
	if end.IsZero() || w <= 0 || cadence <= 0 {
		return nil
	}
	grid := make([]float64, w)
	start := end.Add(-time.Duration(w-1) * cadence)
	for _, sr := range srcs {
		for k, v := range sr.vals {
			st := sampleTime(sr.ts, k)
			if st.IsZero() {
				continue
			}
			// A bucket takes every sample within half a cadence of it, so a
			// source sampled faster than the grid is summed rather than
			// overwritten, and one sampled slower lands on the bucket it
			// belongs to. A sample with no instant is not placed at all.
			//
			// The columns it reaches are derived, not scanned: at a 200
			// column chart over three engines' retention, testing every
			// column per sample was a third of the frame.
			first, last := cadenceSpan(st.Sub(start), cadence)
			for j := max(first, 0); j <= min(last, w-1); j++ {
				grid[j] += v
			}
		}
	}
	for i, v := range agentDenseHist(s.Agents, out, end, w, cadence) {
		grid[i] += v
	}
	return grid
}

// probeSeries turns irregularly timed probe results into a step-hold series
// on a uniform grid ending at the newest probe: each bucket carries the most
// recently measured tok/s.
func probeSeries(s core.Snapshot, w int, cadence time.Duration) []float64 {
	if len(s.Probes) == 0 || w <= 0 || cadence <= 0 {
		return nil
	}
	end := s.Probes[len(s.Probes)-1].At
	grid := make([]float64, w)
	start := end.Add(-time.Duration(w-1) * cadence)
	last := 0.0
	seen := false
	next := 0
	for j := range grid {
		bucketEnd := start.Add(time.Duration(j+1) * cadence)
		for next < len(s.Probes) && s.Probes[next].At.Before(bucketEnd) {
			last, seen = s.Probes[next].TokPS, true // hold even pre-window probes
			next++
		}
		if seen {
			grid[j] = last
		}
	}
	return grid
}
