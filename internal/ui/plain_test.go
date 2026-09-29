package ui

import (
	"cmp"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The plain frame is the screen-reader path: it must carry no braille chart
// glyphs, no box-drawing borders, no bar meters and no ANSI styling at all.
// Those are exactly the characters that read as noise (or silence) through a
// screen reader in the dashboard frame.
func TestPlainFrameHasNoNonTextGlyphs(t *testing.T) {
	out := PlainTextFrame(Config{Version: "t"}, busySnap())
	for name, re := range map[string]*regexp.Regexp{
		"braille":       regexp.MustCompile(`[\x{2800}-\x{28FF}]`),
		"box drawing":   regexp.MustCompile(`[\x{2500}-\x{257F}]`),
		"geometric":     regexp.MustCompile(`[\x{25A0}-\x{25FF}]`),
		"ANSI escapes":  regexp.MustCompile(`\x1b`),
		"dots/controls": regexp.MustCompile(`[\x{00}\x{07}\x{08}\x{0b}\x{0c}]`),
	} {
		if re.MatchString(out) {
			t.Errorf("plain frame contains %s glyph(s):\n%s", name, out)
		}
	}
}

func TestPlainFrameCarriesTheData(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		At:     now,
		Uptime: 2 * time.Minute,
		Providers: []core.ProviderSnapshot{
			{
				Label: "ollama", Kind: core.KindOllama,
				Addr: "http://127.0.0.1:11434", OK: true,
				Models: []core.ModelInfo{{Name: "llama3"}}, Version: "0.5",
				OutTokPS: 109, InTokPS: 401, KVPct: 7, Running: 2, Waiting: 3,
			},
			{
				Label: "vllm", Kind: core.KindVLLM,
				Addr: "http://127.0.0.1:8000", OK: false,
				Err: "connection refused",
			},
		},
		Sys: &core.SysSample{
			MemTotal: 32 << 30, MemUsed: 16 << 30,
			SwapTotal: 8 << 30, SwapUsed: 1 << 30,
			Load1: 1.52, CPUModel: "Test CPU", OsName: "TestOS", Kernel: "9.9",
			Temps: []core.TempReading{{Label: "package", MilliC: 64000}},
			GPUs: []core.GPUDevice{{Vendor: "nvidia", Index: 0, Name: "A100",
				MilliC: 71000, MemTotal: 80 << 30, MemUsed: 20 << 30,
				UtilPct: 42, PowerW: 310}},
		},
		Probes: []core.ProbeSample{
			{At: now.Add(-time.Second), Model: "llama3", OK: true, TTFTms: 97, TokPS: 340},
			{At: now, Model: "gone", OK: false, Err: "timeout"},
		},
		Agents: []core.AgentEvent{
			{At: now.Add(-2 * time.Second), Agent: "claude", Kind: "turn",
				Model: "sonnet", PromptTokens: 4200, OutputTokens: 310},
			{At: now.Add(-time.Second), Agent: "codex", Kind: "tool",
				PromptTokens: 10, OutputTokens: 5, Note: "shell(git status)"},
		},
	}
	out := PlainTextFrame(Config{Version: "t"}, snap)
	for _, want := range []string{
		// header: count spelled out, partial state named
		"toktop vt", "1/2 engines up (partial)",
		// engines: status as a word, error text attached to the down one
		"up   ollama", "down vllm", "connection refused", "kv cache 7%",
		"running 2", "waiting 3",
		// system strip content survives as text, and a reading on the warn or
		// crit band names its severity: this report has no color, so a word is
		// the only channel a hot reading has (WCAG 1.4.1)
		"memory 50%", "swap 12%", "load 1.52", "gpu nv0 A100", "71° high",
		"vram 20G/80G", "310W", "Test CPU", "temp package 64° high",
		// probes: verdict words, failure reason kept, no empty metrics
		"failed gone", "error: timeout", "ok llama3", "ttft 97ms", "340 tok/s",
		// feed: kind words and token counts instead of icons
		"turn claude", "prompt 4.2k", "output 310", "tool codex", "note shell(git status)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain frame missing %q in:\n%s", want, out)
		}
	}
}

// The plain report has no color to spend on a reading, so a reading over a
// ramp's warn or crit threshold has to say so in words. Both steps, on a
// temperature, a memory percentage and the KV-cache meter the dashboard draws
// as a colored gauge, and an OK-band reading says nothing: a healthy machine
// reads the same as it always did.
func TestPlainFrameNamesSeverityInWords(t *testing.T) {
	snap := core.Snapshot{
		At: time.Now(),
		Providers: []core.ProviderSnapshot{
			{Label: "x", Kind: core.KindOllama, OK: true},
			{Label: "kv-ok", Kind: core.KindOllama, OK: true, KVPct: 40},
			{Label: "kv-warn", Kind: core.KindOllama, OK: true, KVPct: 70},
			{Label: "kv-crit", Kind: core.KindOllama, OK: true, KVPct: 95},
		},
		Sys: &core.SysSample{
			MemTotal: 1000, MemUsed: 500, SwapTotal: 1000, SwapUsed: 800,
			Temps: []core.TempReading{
				{Label: "cool", MilliC: 40000}, // ok
				{Label: "warm", MilliC: 65000}, // warn
				{Label: "hot", MilliC: 95000},  // crit
			},
			GPUs: []core.GPUDevice{{Vendor: "nvidia", Index: 0, MilliC: 85000}},
		},
	}
	out := PlainTextFrame(Config{Version: "t"}, snap)
	for _, want := range []string{
		"temp cool 40°\n",
		"temp warm 65° high",
		"temp hot 95° critical",
		"swap 80% high",
		"gpu nv0 85° critical",
		// The gauge's band is the one reading the report used to drop: the
		// drawn meter spends it on a bar color, and a bar's length reads the
		// same to a monochrome terminal at 95% as at 40%.
		"kv-ok (ollama)\n       out 0.0 tok/s · in 0.0 tok/s · kv cache 40% · running 0",
		"kv cache 70% high",
		"kv cache 95% critical",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain frame missing %q in:\n%s", want, out)
		}
	}
	// memory is at 50%, on the OK band, so it carries no word
	if !strings.Contains(out, "memory 50% (") {
		t.Errorf("OK-band memory should read bare, got:\n%s", out)
	}
}

// An engine with no models (the normal state of a down one) names no model
// anywhere: a "-" placeholder in the dashboard row read as a model called "-",
// and the plain report already dropped it.
func TestEngineModelLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []core.ModelInfo
		label  string
	}{
		{name: "nil"},
		{name: "empty", models: []core.ModelInfo{}},
		{name: "first", models: []core.ModelInfo{{Name: "first"}, {Name: "second"}}, label: "first"},
		{name: "empty first", models: []core.ModelInfo{{}, {Name: "second"}}},
		{name: "sanitized", models: []core.ModelInfo{{Name: "\x1b[31mfirst\x1b[0m"}}, label: "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := core.Snapshot{Providers: []core.ProviderSnapshot{{
				Label: "engine", Kind: core.KindOllama, Models: tc.models,
			}}}
			m := Model{snap: snap}
			row, _, _ := strings.Cut(strip(func() string { b, _ := m.providersBody(80, 4); return b }()), "\n")
			// The row carries the engine's own label as well as its model, so
			// the model is looked for by name rather than as the last cell.
			named := false
			for _, f := range strings.Fields(row) {
				if f == tc.label {
					named = true
				}
			}
			if named != (tc.label != "") {
				t.Errorf("dashboard model row = %q, want model %q present=%v", row, tc.label, tc.label != "")
			}
			if !strings.Contains(row, "engine") {
				t.Errorf("dashboard row does not name the engine: %q", row)
			}
			var plain strings.Builder
			writeEnginesPlain(&plain, snap)
			want := "\nENGINES\ndown engine (ollama)\n"
			if tc.label != "" && tc.label != "-" {
				want += "       " + tc.label + "\n"
			}
			if got := plain.String(); got != want {
				t.Errorf("plain engines = %q, want %q", got, want)
			}
		})
	}
}

func TestPlainFrameAllDown(t *testing.T) {
	snap := core.Snapshot{
		Providers: []core.ProviderSnapshot{{
			Label: "box", Kind: core.KindOpenAI, OK: false, Err: "no route",
		}},
	}
	out := PlainTextFrame(Config{Version: "t"}, snap)
	for _, want := range []string{"0/1 engines up (all down)", "down box", "error: no route"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain frame missing %q in:\n%s", want, out)
		}
	}
}

func TestPlainFrameEmptyState(t *testing.T) {
	out := PlainTextFrame(Config{Version: "t"}, core.Snapshot{})
	for _, want := range []string{"no inference engines detected", "--add URL", "--agents"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty plain frame missing %q in:\n%s", want, out)
		}
	}
	on := PlainTextFrame(Config{Version: "t", Agents: true}, core.Snapshot{})
	if !strings.Contains(on, "watching local agents") {
		t.Errorf("--agents on, but plain empty does not say so:\n%s", on)
	}
	if strings.Contains(on, "--agents") {
		t.Errorf("plain empty still tells an --agents run to pass --agents:\n%s", on)
	}
	ingest := PlainTextFrame(Config{Version: "t", IngestAddr: "127.0.0.1:8420"}, core.Snapshot{})
	if !strings.Contains(ingest, "http://127.0.0.1:8420/v1/events") {
		t.Errorf("plain empty lost the live ingest endpoint:\n%s", ingest)
	}
}

// An empty feed must still say how to feed it, like the dashboard panel does.
func TestPlainFrameEmptyFeedNamesTheEndpoint(t *testing.T) {
	out := PlainTextFrame(Config{Version: "t", IngestAddr: "127.0.0.1:8420"},
		core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "x", OK: true}}})
	if !strings.Contains(out, "POST events to http://127.0.0.1:8420/v1/events") {
		t.Errorf("empty plain feed missing endpoint hint:\n%s", out)
	}
}

// Agents without engines get the agents-only report: per-agent rows with
// rates (or an explicit no-rate), recency as a word, then the feed tail.
func TestPlainFrameAgentsOnly(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		At:        now,
		Providers: nil,
		Agents: []core.AgentEvent{
			{At: now.Add(-4 * time.Second), Agent: "codex", Kind: "turn",
				PromptTokens: 100, OutputTokens: 40},
			{At: now.Add(-time.Second), Agent: "codex", Kind: "tool",
				PromptTokens: 100, OutputTokens: 60},
		},
	}
	out := PlainTextFrame(Config{Version: "t"}, snap)
	for _, want := range []string{
		"AGENTS", "codex", "live", "AGENT FEED", "--add URL attaches one",
		"out ", "tok/s", "output ", "prompt ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agents-only plain frame missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ENGINES\n") {
		t.Errorf("agents-only plain frame invented an ENGINES section:\n%s", out)
	}
}

// --once --plain must score agent rates against the snapshot stamp, not
// wall time, or an hour-old sample would drop agents that were live then.
func TestPlainFrameUsesSnapshotTime(t *testing.T) {
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
	out := PlainTextFrame(Config{Version: "t"}, snap)
	// The count is spelled by agentCountLabel, so one in-window agent reads
	// "1 agent" here exactly as it does in the header and the agents view.
	if !strings.Contains(out, "1 agent") {
		t.Fatalf("hour-old snapshot dropped in-window agents:\n%s", out)
	}
}

// Demo runs must not pass themselves off as real telemetry in the linear
// frame either.
func TestPlainFrameMarksDemo(t *testing.T) {
	out := PlainTextFrame(Config{Version: "t", Demo: true, DemoSeed: 7}, core.Snapshot{})
	if !strings.HasPrefix(out, "[demo seed 7] ") {
		t.Errorf("demo plain frame lacks its seed marker:\n%s", out)
	}
}

// A sensor label is whatever the driver wrote into hwmon, and --plain is the
// path a screen reader or a log file consumes. It goes out sanitized like
// every other externally sourced field, so a driver cannot put a live escape
// sequence on a terminal that trusts this output.
func TestPlainTempLabelIsSanitized(t *testing.T) {
	for _, tc := range []struct {
		name, label, want string
	}{
		{"plain", "package", "temp package 64"},
		{"title set", "\x1b]0;spoofed\x07core", "temp core 64"},
		{"csi", "\x1b[31mcore\x1b[0m", "temp core 64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := busySnap()
			snap.Sys = &core.SysSample{
				MemTotal: 32 << 30, MemUsed: 16 << 30,
				Temps: []core.TempReading{{Label: tc.label, MilliC: 64000}},
			}
			out := PlainTextFrame(Config{Version: "t"}, snap)
			if strings.ContainsRune(out, 0x1b) {
				t.Fatalf("escape sequence reached the plain frame:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("plain frame missing %q in:\n%s", tc.want, out)
			}
		})
	}
}

// The SYS strip cuts a long CPU model to keep one row readable; the plain
// report wraps to the terminal's width and has no such row to fit, so it must
// print the model whole. A cut name there is a fact the reader cannot get
// back from anywhere else in the output.
func TestPlainFramePrintsTheFullCPUModel(t *testing.T) {
	const cpu = "AMD Ryzen Threadripper PRO 5995WX 64-Cores"
	snap := busySnap()
	snap.Sys = &core.SysSample{
		MemTotal: 32 << 30, MemUsed: 16 << 30,
		CPUModel: cpu,
		Drivers:  map[string]string{"nvidia": "550.54.14"},
	}
	out := PlainTextFrame(Config{Version: "t"}, snap)
	if !strings.Contains(out, cpu) {
		t.Errorf("plain frame truncated the CPU model:\n%s", out)
	}
	if !strings.Contains(out, "nvidia 550.54.14") {
		t.Errorf("plain frame truncated the driver version:\n%s", out)
	}
	// The strip is a fixed row and still cuts, so the two stay different.
	m := New(Config{Version: "t"}, nil)
	m.snap = snap
	if stripSegs := m.renderSystem(); strings.Contains(strip(stripSegs), cpu) {
		t.Errorf("SYS strip let a %d-cell model past its budget", len(cpu))
	}
}

// The dashboard panel and the linear report answer the same question about the
// same empty panel, so they must give the same advice. Only the ingest clause
// differs, and only because the panel title above it already carries the
// endpoint; the two used to drift apart entirely.
func TestEmptyFeedAdviceMatchesAcrossViews(t *testing.T) {
	snap := core.Snapshot{Providers: []core.ProviderSnapshot{{Label: "x", OK: true}}}
	for _, tc := range []struct {
		name  string
		cfg   Config
		want  string
		panel string // the dashboard clause, where it cannot be want verbatim
	}{
		{"agents", Config{Version: "t", Agents: true},
			"no agent activity yet: agents running locally are picked up automatically", ""},
		{"agents+ingest", Config{Version: "t", Agents: true, IngestAddr: "127.0.0.1:8420"},
			"no agent activity yet: agents running locally are picked up automatically", ""},
		{"ingest", Config{Version: "t", IngestAddr: "127.0.0.1:8420"},
			"no agent activity yet: POST events to http://127.0.0.1:8420/v1/events",
			"no agent activity yet: point your harness at the endpoint above"},
		{"bare", Config{Version: "t"},
			"no agent activity yet: run with --agents to watch coding agents on this machine", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if plain := PlainTextFrame(tc.cfg, snap); !strings.Contains(plain, tc.want) {
				t.Errorf("plain report = %q, want %q", plain, tc.want)
			}
			m := New(tc.cfg, nil)
			m.w, m.h, m.ready = 120, 40, true
			m.snap = snap
			got := strip(strings.Join(m.feedEmptyLines(100), "\n"))
			want := cmp.Or(tc.panel, tc.want)
			if got != want {
				t.Errorf("feed panel = %q, want %q", got, want)
			}
		})
	}
}

// The PROBES panel plots a braille history of measured rates, and the rows the
// plain report lists below are the newest of them: nothing else names how the
// window got. The peak is the same text alternative writeThroughputPlain gives
// the two throughput charts (WCAG 1.1.1), and the panel title is clipped to a
// column too narrow to carry it.
func TestPlainProbePeakNamesThePlot(t *testing.T) {
	now := time.Now()
	engines := []core.ProviderSnapshot{{Label: "box", Kind: core.KindOpenAI, OK: true}}
	out := PlainTextFrame(Config{Version: "t", PollEvery: time.Second}, core.Snapshot{
		At:        now,
		Providers: engines,
		Probes: []core.ProbeSample{
			{At: now.Add(-30 * time.Second), Model: "a", OK: true, TokPS: 12},
			{At: now.Add(-20 * time.Second), Model: "a", OK: true, TokPS: 480},
			{At: now, Model: "a", OK: true, TokPS: 30},
		},
	})
	if want := "peak 480 tok/s over the last 30s"; !strings.Contains(out, want) {
		t.Errorf("plain frame missing %q in:\n%s", want, out)
	}

	// A window that measured nothing has no peak to name, and a line reading
	// "peak 0 tok/s" would be a measurement of nothing.
	none := PlainTextFrame(Config{Version: "t", PollEvery: time.Second}, core.Snapshot{
		At:        now,
		Providers: engines,
		Probes:    []core.ProbeSample{{At: now, Model: "a", Err: "timeout"}},
	})
	if strings.Contains(none, "peak") {
		t.Errorf("plain frame names a peak for a window that measured nothing:\n%s", none)
	}

	// A run with no probes at all still says which knob produces them: the
	// empty panel says it, and this report is the only surface a screen-reader
	// user has.
	empty := PlainTextFrame(Config{Version: "t"}, core.Snapshot{At: now, Providers: engines})
	if want := "none yet: quit, re-run with --probe N"; !strings.Contains(empty, want) {
		t.Errorf("plain frame missing %q in:\n%s", want, empty)
	}
}

// The live plain view is the screen-reader path into the running dashboard,
// not only into a --once snapshot. It has to differ from the drawn frame in
// the same two ways the --once report does (no chart glyphs, no column
// layout) and it has to keep the key line, or the keys that made the live
// view worth having are documented nowhere on it.
func TestLivePlainViewIsTheReportPlusTheKeys(t *testing.T) {
	cfg := Config{Version: "t", Plain: true}
	m := New(cfg, nil)
	m.snap = busySnap()
	m.w, m.h = 100, 40
	m.ready = true

	out := strip(m.View())
	report := strip(PlainTextFrame(cfg, m.snap))
	if !strings.Contains(out, report) {
		t.Errorf("live plain view is not the plain report:\n%s", out)
	}
	for _, want := range []string{"ENGINES", "q", "quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("live plain view missing %q:\n%s", want, out)
		}
	}
	for name, re := range map[string]*regexp.Regexp{
		"braille":      regexp.MustCompile(`[\x{2800}-\x{28FF}]`),
		"box drawing":  regexp.MustCompile(`[\x{2500}-\x{257F}]`),
		"ANSI escapes": regexp.MustCompile(`\x1b`),
	} {
		if re.MatchString(out) {
			t.Errorf("live plain view contains %s glyph(s):\n%s", name, out)
		}
	}

	// A frozen report with nothing on it saying so is a feed that stalled.
	m.paused = true
	if want := "PAUSED"; !strings.Contains(strip(m.View()), want) {
		t.Errorf("paused live plain view carries no %q badge:\n%s", want, strip(m.View()))
	}

	// A degraded feed is announced in a panel of the drawn frame. The report
	// has no panel titles to point at the message, so it names the subsystem.
	m.paused = false
	m.feedDown = "ingest endpoint closed"
	if want := "feed: ingest endpoint closed"; !strings.Contains(strip(m.View()), want) {
		t.Errorf("live plain view missing the degraded-feed line %q:\n%s", want, strip(m.View()))
	}
}
