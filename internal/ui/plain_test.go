package ui

import (
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
		// system strip content survives as text
		"memory 50%", "swap 12%", "load 1.52", "gpu nv0 A100", "71°",
		"vram 20G/80G", "310W", "Test CPU", "temp package 64°",
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
