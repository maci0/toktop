package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// maxFeedEvents bounds the agent feed rows the plain report prints. A module
// constant beside maxSummaryAgents, which caps the rate summary two sections
// up, so both row caps read as one policy.
const maxFeedEvents = 8

// maxProbeRows bounds the probe results the plain report prints. A new enough
// outcome beats an older one, and two rows carry "it works" and "it does not"
// without burying either.
const maxProbeRows = 2

// PlainTextFrame renders one snapshot as a linear, text-only report: the
// non-visual counterpart to the dashboard frame. That frame draws charts as
// braille dot-matrix rows, panels as box-drawing borders and meters as bar
// glyphs; a screen reader meets those as floods of "braille pattern dots-…"
// announcements or skips them silently, and the side-by-side mid-row scrambles
// into interleaved column fragments when read line by line. This frame carries
// every number as words in reading order instead (WCAG 1.1.1): no braille, no
// borders, no bars, no multi-column layout. It is what `--once --plain` prints.
func PlainTextFrame(cfg Config, s core.Snapshot) string {
	var b strings.Builder
	if cfg.Demo {
		fmt.Fprintf(&b, "[demo seed %d] ", cfg.DemoSeed)
	}
	b.WriteString("toktop v" + cfg.Version)
	if s.Sys != nil {
		switch {
		case s.Sys.RemoteErr != "":
			target := s.Sys.RemoteHost
			if target == "" {
				target = "remote"
			}
			b.WriteString(" via ssh:" + core.SingleLine(target) +
				" (not answering: " + core.SingleLine(s.Sys.RemoteErr) + ")")
		case s.Sys.RemoteHost != "":
			b.WriteString(" via ssh:" + core.SingleLine(s.Sys.RemoteHost))
		}
	}
	b.WriteString("\n\n")

	if len(s.Providers) == 0 {
		if len(s.Agents) > 0 {
			writeAgentsPlain(&b, s, cfg)
			return b.String()
		}
		b.WriteString("no inference engines detected\n")
		b.WriteString("attach an engine with --add URL")
		switch {
		case cfg.Agents:
			b.WriteString("; watching local agents")
		default:
			b.WriteString(", watch agents with --agents")
		}
		if cfg.IngestAddr != "" {
			b.WriteString(fmt.Sprintf(", or POST events to http://%s/v1/events",
				core.SanitizeText(cfg.IngestAddr)))
		}
		b.WriteString(", or preview with --demo\n")
		return b.String()
	}

	up, tot := upCount(s.Providers)
	state := fmt.Sprintf("%d/%d engines up", up, tot)
	switch {
	case up == 0:
		state += " (all down)"
	case up < tot:
		state += " (partial)"
	}
	now := frameNow(s, time.Time{})
	// One accounting of the feed serves both the aggregate line and the
	// per-agent list below it.
	sum := core.Summarize(s.Agents, now)
	outAgg, inAgg := aggBoth(s, sum)
	fmt.Fprintf(&b, "%s · out %s tok/s · in %s tok/s",
		state, fmtRate(outAgg), fmtRate(inAgg))
	rates := sum.Rates
	if n := len(rates); n > 0 {
		b.WriteString(" · " + agentCountLabel(n))
	}
	if s.Uptime > 0 {
		b.WriteString(" · session " + fmtDur(s.Uptime))
	}
	b.WriteString("\n")

	writeThroughputPlain(&b, s, cfg)
	writeEnginesPlain(&b, s)
	writeSystemPlain(&b, s.Sys)
	writeProbesPlain(&b, s, cfg)
	writeFeedPlain(&b, s, cfg, rates)
	return b.String()
}

// writeThroughputPlain is the text alternative for the dashboard's two braille
// charts: the status line above already carries the current output and input
// rates, and this names the peaks those plots are scaled against and the window
// they span (WCAG 1.1.1). The chart body is braille dot patterns, which a screen
// reader announces as pattern glyphs and a monochrome terminal cannot read as
// height at all, so without these two lines the throughput history has no
// account of itself anywhere in the product.
//
// A run with no rate anywhere prints nothing: a peak of 0 on an empty window
// is a measurement of nothing.
func writeThroughputPlain(b *strings.Builder, s core.Snapshot, cfg Config) {
	cad := cadenceOf(cfg.PollEvery)
	outPeak := seriesPeak(aggHist(s, true, core.HistoryLen, cad))
	inPeak := seriesPeak(aggHist(s, false, core.HistoryLen, cad))
	if outPeak <= 0 && inPeak <= 0 {
		return
	}
	b.WriteString("\nTHROUGHPUT\n")
	fmt.Fprintf(b, "peak %s tok/s out, %s tok/s in, over the last %s\n",
		fmtRate(outPeak), fmtRate(inPeak), fmtDur(historyWindow(s, cad)))
}

// historyWindow is the span the plotted samples actually cover, oldest sample
// to newest across every engine.
func historyWindow(s core.Snapshot, cad time.Duration) time.Duration {
	var stamps []time.Time
	plotted := 0
	for i := range s.Providers {
		for _, out := range [2]bool{true, false} {
			vals, series := historyOf(s.Providers[i], out)
			plotted = max(plotted, len(vals))
			for j := range vals {
				if at := sampleTime(series, j); !at.IsZero() {
					stamps = append(stamps, at)
				}
			}
		}
	}
	return spanOf(stamps, plotted, cad)
}

// probeWindow is historyWindow over the probe samples: the span the PROBES
// plot covers, on the same terms.
func probeWindow(s core.Snapshot, cad time.Duration) time.Duration {
	stamps := make([]time.Time, 0, len(s.Probes))
	for _, p := range s.Probes {
		stamps = append(stamps, p.At)
	}
	return spanOf(stamps, len(stamps), cad)
}

// spanOf is what a plotted window spans, oldest stamp to newest. The sample
// buffers are sized for a full run, so naming their capacity here would claim a
// three-minute window for a five-frame `--once` render that measured four
// seconds. The stamps that travel with the values are the real answer; plotted,
// the number of samples on the collector cadence, stands in only for a caller
// that built a snapshot without them.
func spanOf(stamps []time.Time, plotted int, cad time.Duration) time.Duration {
	var oldest, newest time.Time
	for _, at := range stamps {
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		if at.After(newest) {
			newest = at
		}
	}
	if !oldest.IsZero() && newest.After(oldest) {
		return newest.Sub(oldest)
	}
	if plotted >= 2 {
		return time.Duration(plotted-1) * cad
	}
	return cad
}

// writeEnginesPlain lists every backend as its own block of lines, healthy or
// not: the TUI hides down engines' telemetry behind an error line, and both
// halves must survive here.
func writeEnginesPlain(b *strings.Builder, s core.Snapshot) {
	b.WriteString("\nENGINES\n")
	for _, p := range s.Providers {
		head := core.SanitizeText(p.Label)
		if head == "" {
			head = core.SanitizeText(p.Addr)
		}
		if k := core.SanitizeText(p.Kind); k != "" {
			head += " (" + k + ")"
		}
		if p.OK {
			b.WriteString("up   " + head + "\n")
		} else {
			b.WriteString("down " + head + "\n")
		}
		var detail []string
		if model := core.SanitizeText(primaryModel(p)); model != "" && model != "-" {
			detail = append(detail, model)
		}
		if p.Version != "" {
			detail = append(detail, "version "+core.SingleLine(p.Version))
		}
		if len(detail) > 0 {
			b.WriteString("       " + strings.Join(detail, " · ") + "\n")
		}
		if !p.OK {
			if msg := strings.TrimSpace(core.SingleLine(p.Err)); msg != "" {
				b.WriteString("       error: " + shorten(msg, 120) + "\n")
			}
			continue
		}
		var stats []string
		stats = append(stats,
			"out "+fmtRate(p.OutTokPS)+" tok/s",
			"in "+fmtRate(p.InTokPS)+" tok/s",
			fmt.Sprintf("kv cache %.0f%%", clamp01(p.KVPct/100)*100))
		stats = append(stats,
			fmt.Sprintf("running %d", p.Running),
			fmt.Sprintf("waiting %d", p.Waiting))
		if p.TTFTms > 0 {
			stats = append(stats, "ttft "+fmtMs(p.TTFTms))
		}
		if p.ProcRSS > 0 {
			rss := "rss " + humanBytesShort(p.ProcRSS)
			if p.ProcCPU > 0 {
				rss += fmt.Sprintf(" %.0f%% cpu", p.ProcCPU)
			}
			stats = append(stats, rss)
		}
		b.WriteString("       " + strings.Join(stats, " · ") + "\n")
	}
}

// writeSystemPlain mirrors the SYS strip: memory, swap, load, accelerators,
// identity and temperatures, each as its own labelled line.
func writeSystemPlain(b *strings.Builder, sy *core.SysSample) {
	if sy == nil {
		return
	}
	b.WriteString("\nSYSTEM\n")
	if sy.MemTotal == 0 {
		b.WriteString("memory n/a\n")
	} else {
		memPct := float64(sy.MemUsed) / float64(sy.MemTotal) * 100
		line := fmt.Sprintf("memory %.0f%% (%s/%s)", memPct,
			humanBytesShort(sy.MemUsed), humanBytesShort(sy.MemTotal))
		if sy.SwapTotal > 0 {
			swPct := float64(sy.SwapUsed) / float64(sy.SwapTotal) * 100
			line += fmt.Sprintf(" · swap %.0f%%", swPct)
		}
		if sy.Load1 > 0 || sy.Load5 > 0 {
			line += fmt.Sprintf(" · load %.2f", sy.Load1)
		}
		b.WriteString(line + "\n")
	}
	for _, g := range sy.GPUs {
		line := fmt.Sprintf("gpu %s%d", shortVendor(g.Vendor), g.Index)
		if g.Name != "" {
			line += " " + shorten(core.SanitizeText(g.Name), 30)
		}
		if g.MilliC > 0 {
			line += " " + fmtTempC(g.MilliC)
		}
		if g.UtilPct > 0 {
			line += fmt.Sprintf(" %.0f%% util", g.UtilPct)
		}
		if g.MemTotal > 0 {
			line += " vram " + humanBytesShort(g.MemUsed) + "/" + humanBytesShort(g.MemTotal)
		}
		if g.PowerW > 0 {
			line += fmt.Sprintf(" %.0fW", g.PowerW)
		}
		b.WriteString(line + "\n")
	}
	// Same segments as the TUI strip (hostSegments), uncapped: this report
	// wraps to the terminal's width, so a cut CPU model would be a fact the
	// reader has no way to get back. They carry the strip's separator, not a
	// space: a CPU model ending in a word and the OS name after it run
	// together into one indistinguishable string, and this report has no row
	// borders to tell the fields apart.
	if ident := hostSegments(sy, hostSegmentLimits{}); len(ident) > 0 {
		b.WriteString(strings.Join(ident, " · ") + "\n")
	}
	shown := 0
	for _, t := range sysCPUTemps(sy) {
		if shown >= shownCPUTemps {
			break
		}
		b.WriteString(fmt.Sprintf("temp %s %s\n",
			core.SanitizeText(cpuTempLabel(t.Label)),
			fmtTempC(t.MilliC)))
		shown++
	}
}

// writeProbesPlain lists the most recent probe results newest-first, with the
// ok/failed verdict spelled out and failure reasons attached.
//
// The window peak is the text alternative for the PROBES panel's braille plot,
// the same job writeThroughputPlain's peak does for the two throughput charts
// (WCAG 1.1.1). The rows below carry the newest measurements; only the peak
// says how the window got, and the panel title is clipped to a column too
// narrow to hold it, so without this line the probe history has no account of
// itself anywhere. A peak of 0 prints nothing: that is a measurement of
// nothing.
func writeProbesPlain(b *strings.Builder, s core.Snapshot, cfg Config) {
	if len(s.Probes) == 0 {
		// Same rule as the empty agent feed: the panel tells the reader which
		// knob fills it, and this report is the only surface a screen-reader
		// user has, so it carries the instruction. No key, unlike the panel:
		// this frame is not interactive.
		b.WriteString("\nPROBES\n")
		b.WriteString("none yet: quit, re-run with --probe N\n")
		return
	}
	b.WriteString("\nPROBES\n")
	cad := cadenceOf(cfg.PollEvery)
	if peak := seriesPeak(probeSeries(s, core.ProbeHistoryLen, cad)); peak > 0 {
		fmt.Fprintf(b, "peak %s tok/s over the last %s\n", fmtRate(peak), fmtDur(probeWindow(s, cad)))
	}
	shown := 0
	for i := len(s.Probes) - 1; i >= 0 && shown < maxProbeRows; i-- {
		p := s.Probes[i]
		if !p.OK {
			line := fmt.Sprintf("failed %s", shorten(core.SingleLine(p.Model), 40))
			if msg := strings.TrimSpace(core.SingleLine(p.Err)); msg != "" {
				line += " error: " + shorten(msg, 100)
			}
			b.WriteString(line + "\n")
			shown++
			continue
		}
		fmt.Fprintf(b, "ok %s ttft %s %s tok/s\n",
			shorten(core.SingleLine(p.Model), 40), fmtMs(p.TTFTms),
			fmtRate(p.TokPS))
		shown++
	}
}

// writeFeedPlain tails the agent feed oldest-first like the panel does, with
// per-kind words instead of icons. An empty feed keeps the panel's setup
// guidance: which knob feeds this panel is invisible from a bare "empty".
func writeFeedPlain(b *strings.Builder, s core.Snapshot, cfg Config, rates []core.AgentRate) {
	b.WriteString("\nAGENT FEED\n")
	if len(rates) > 0 {
		var parts []string
		for i, r := range rates {
			if i == maxSummaryAgents {
				parts = append(parts, fmt.Sprintf("+%d more", len(rates)-maxSummaryAgents))
				break
			}
			name := core.SingleLine(r.Agent)
			// Engine-routed tokens are already counted by the engine, so the
			// agent row must not present them as its own rate.
			switch {
			case r.ViaEngine != "":
				parts = append(parts, name+" via "+core.SingleLine(r.ViaEngine))
			case r.TokPS > 0:
				parts = append(parts, fmt.Sprintf("%s %s tok/s", name, fmtRate(r.TokPS)))
			default:
				parts = append(parts, fmt.Sprintf("%s %s tok", name, fmtCount(r.Tokens)))
			}
		}
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	if len(s.Agents) == 0 {
		// No panel title above this line to point at, so the endpoint is spelled
		// out; the dashboard's clause for the same run points at its own title.
		where := ""
		if cfg.IngestAddr != "" {
			where = "POST events to http://" + core.SanitizeText(cfg.IngestAddr) + "/v1/events"
		}
		b.WriteString(feedEmptyHint(cfg, where) + "\n")
		return
	}
	start := max(len(s.Agents)-maxFeedEvents, 0)
	for _, ev := range s.Agents[start:] {
		model := strings.TrimSpace(core.SingleLine(ev.Model))
		if model == "" {
			model = "-"
		}
		kind := core.SingleLine(ev.Kind)
		if kind == "" {
			kind = "event"
		}
		fmt.Fprintf(b, "%s %s %s model %s prompt %s output %s",
			ev.At.Local().Format("15:04:05"), kind,
			core.SingleLine(ev.Agent),
			model,
			fmtCount(ev.PromptTokens), fmtCount(ev.OutputTokens))
		if ev.ThinkingTokens > 0 {
			b.WriteString(" thinking " + fmtCount(ev.ThinkingTokens))
		}
		if ev.ViaEngine != "" {
			b.WriteString(" via " + core.SingleLine(ev.ViaEngine))
		}
		if ev.Note != "" {
			b.WriteString(" note " + shorten(core.SingleLine(ev.Note), 60))
		}
		b.WriteString("\n")
	}
}

// writeAgentsPlain is the plain counterpart of renderAgentsOnly: agents but
// no engines.
func writeAgentsPlain(b *strings.Builder, s core.Snapshot, cfg Config) {
	now := frameNow(s, time.Time{})
	sum := core.Summarize(s.Agents, now)
	outPS, inPS := sumOwn(sum.Own)
	b.WriteString("no inference engines detected; --add URL attaches one\n")
	fmt.Fprintf(b, "out %s tok/s · in %s tok/s\n", fmtRate(outPS), fmtRate(inPS))
	writeThroughputPlain(b, s, cfg)
	writeSystemPlain(b, s.Sys)
	b.WriteString("\nAGENTS\n")
	rates := sum.Rates
	rows := 0
	for _, r := range rates {
		name := core.SingleLine(r.Agent)
		recency := ""
		switch d, how := agentIdle(now, r.Last); how {
		case recencyLive:
			recency = "live"
		case recencyIdle:
			recency = "idle " + fmtDur(d)
		default:
			recency = "time unknown"
		}
		// The engine is named once, where the rate it replaces would go, so
		// the recency word keeps its own job. This mirrors the AGENT FEED
		// summary line above, which prints the same attribution.
		line := name
		switch {
		case r.ViaEngine != "":
			line += " via " + core.SingleLine(r.ViaEngine)
		case r.TokPS > 0:
			line += " " + fmtRate(r.TokPS) + " tok/s"
		default:
			line += " no rate yet"
		}
		line += " output " + fmtCount(r.Tokens)
		if r.Prompt > 0 {
			line += " prompt " + fmtCount(r.Prompt)
		}
		if r.Thinking > 0 {
			line += " thinking " + fmtCount(r.Thinking)
		}
		line += " " + recency
		b.WriteString(line + "\n")
		rows++
	}
	if rows == 0 {
		b.WriteString("waiting for an agent to report tokens\n")
	}
	writeFeedPlain(b, s, cfg, rates)
}
