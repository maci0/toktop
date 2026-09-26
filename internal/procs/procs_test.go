package procs

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExtractPort(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"ollama", "serve"}, 0},
		{[]string{"llama-server", "--port", "8081"}, 8081},
		{[]string{"vllm", "--port=9000"}, 9000},
		{[]string{"x", "--http-port", "5002"}, 5002},
		{[]string{"x", "-p", "1234"}, 0},      // -p is ambiguous, must not match
		{[]string{"x", "--port", "65536"}, 0}, // outside the TCP port range
		{[]string{"x", "--port", "99999999999"}, 0},
		{[]string{"x", "--port", "-1"}, 0},
		{[]string{"x", "--port=65535"}, 65535}, // boundary stays valid
	}
	for _, c := range cases {
		if got := ExtractPort(c.args); got != c.want {
			t.Errorf("ExtractPort(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

// ListenPort prefers an explicit --port flag over the engine's default, and
// is zero when neither is set, so discovery and emit attribute the right
// process (or none) to a backend URL.
func TestListenPort(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want int
	}{
		{"explicit flag wins over default", Info{PortHint: 8081, DefPort: 8080}, 8081},
		{"engine default when no flag", Info{DefPort: 11434}, 11434},
		{"neither is zero", Info{}, 0},
	}
	for _, c := range cases {
		if got := c.info.ListenPort(); got != c.want {
			t.Errorf("%s: ListenPort() = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMatchEngine(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		engine string
		port   int
		ok     bool
	}{
		{"ollama", []string{"ollama", "serve"}, "ollama", 11434, true},
		{"OLLAMA.EXE", []string{`C:\Program Files\Ollama\OLLAMA.EXE`, "serve"}, "ollama", 11434, true},
		{`C:/Apps/JAN.ExE`, nil, "jan", 1337, true},
		{"python.exe", []string{`C:\Python\Scripts\LITELLM.EXE`, "--port", "4000"}, "litellm", 4000, true},
		{"OLLAMA.EXE.BAK", []string{"OLLAMA.EXE.BAK"}, "", 0, false},
		{"llama-server", []string{"/usr/bin/llama-server", "--port", "8081"}, "llama.cpp", 8080, true}, // def; hint overrides separately
		{"llamafile", []string{"llamafile", "--port", "8080"}, "llama.cpp", 8080, true},
		{"python3", []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}, "vllm", 8000, true},
		{"vllm", []string{"vllm", "serve", "--port", "8001"}, "vllm", 8000, true},
		{"python3", []string{"python3", "-m", "sglang.launch_server"}, "sglang", 30000, true},
		{"sglang", []string{"sglang", "serve", "--port", "30001"}, "sglang", 30000, true},
		{"tritonserver", []string{"/opt/tritonserver/bin/tritonserver"}, "triton", 8000, true},
		{"koboldcpp.py", []string{"python3", "koboldcpp.py", "--port", "5001"}, "koboldcpp", 5001, true},
		{"jan", []string{"/opt/Jan/jan"}, "jan", 1337, true},
		{"LM Studio Helper", []string{"/opt/LM Studio Helper"}, "lmstudio", 1234, true},
		{"bash", []string{"bash", "-c", "janitor --clean"}, "", 0, false}, // 'jan' false-positive guard
		{"firefox", []string{"firefox"}, "", 0, false},
		{"ollama behind huge flags", append([]string{"ollama", "serve"}, hugeArgs(64, 1024)...), "ollama", 11434, true},
		{"vllm behind huge flags", append([]string{"python3", "-m", "vllm.entrypoints.openai.api_server"}, hugeArgs(64, 1024)...), "vllm", 8000, true},
	}
	for _, c := range cases {
		i := Info{PID: 1, Name: c.name, Args: c.args,
			PortHint: ExtractPort(c.args)}
		eng, def, ok := MatchEngine(i)
		if ok != c.ok || eng != c.engine || (ok && def != c.port) {
			t.Errorf("match(%s %v) = %q/%d/%v, want %q/%d/%v",
				c.name, c.args, eng, def, ok, c.engine, c.port, c.ok)
		}
	}
}

func hugeArgs(n, each int) []string {
	a := make([]string, n)
	pad := strings.Repeat("x", each)
	for i := range a {
		a[i] = pad
	}
	return a
}

func TestLowerJoinedArgsCapsSize(t *testing.T) {
	got := lowerJoinedArgs(hugeArgs(100, 10_000))
	if len(got) > matchJoinBytes {
		t.Fatalf("joined command is %d bytes, cap is %d", len(got), matchJoinBytes)
	}
	if got := lowerJoinedArgs(nil); got != "" {
		t.Fatalf("empty argv joined to %q", got)
	}
	if got := lowerJoinedArgs([]string{"Ollama", "Serve"}); got != "ollama serve" {
		t.Fatalf("short argv = %q, want lowercased join", got)
	}
}

func TestClipArgDoesNotSplitUTF8(t *testing.T) {
	// 4095 ASCII bytes plus é (U+00E9, two UTF-8 bytes). A raw s[:4096]
	// keeps 0xC3 and drops 0xA9, so ToLower would run on invalid UTF-8.
	a := strings.Repeat("x", matchJoinBytes-1) + "é"
	got := clipArg(a)
	if !utf8.ValidString(got) {
		t.Fatalf("clipArg split a character: %q is not valid UTF-8", got)
	}
	if strings.HasSuffix(got, "é") {
		t.Fatal("clipArg kept a character that does not fit in the byte cap")
	}
	if len(got) != matchJoinBytes-1 {
		t.Fatalf("clipArg length = %d, want %d (ASCII prefix only)", len(got), matchJoinBytes-1)
	}
	joined := lowerJoinedArgs([]string{a})
	if !utf8.ValidString(joined) {
		t.Fatalf("lowerJoinedArgs split a character: %q", joined)
	}
	if len(joined) > matchJoinBytes {
		t.Fatalf("joined command is %d bytes, cap is %d", len(joined), matchJoinBytes)
	}
}

func TestSelfIsSkipped(t *testing.T) {
	for _, list := range [][]Info{NewSampler().Snapshot(), Snapshot()} {
		for _, p := range list {
			if p.PID == os.Getpid() && p.Name == baseName(os.Args[0]) {
				t.Errorf("toktop's own test process leaked in: %+v", p)
			}
		}
	}
}

func TestSnapshotDropsUnrelatedProcesses(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	platformList = func() ([]raw, error) {
		return []raw{
			{pid: 1, name: "ollama", args: []string{"ollama", "serve"}},
			{pid: 2, name: "firefox", args: []string{"firefox"}},
			{pid: 3, name: "python3", args: []string{"python3", "--port", "9000"}},
		}, nil
	}
	list := NewSampler().Snapshot()
	got := map[string]Info{}
	for _, p := range list {
		got[p.Name] = p
	}
	if _, ok := got["firefox"]; ok {
		t.Fatal("unrelated process was kept")
	}
	ollama, ok := got["ollama"]
	if !ok || ollama.Engine != "ollama" || ollama.DefPort != 11434 || ollama.PID != 1 {
		t.Errorf("ollama = %+v, want engine ollama default port 11434 pid 1", ollama)
	}
	py, ok := got["python3"]
	if !ok || py.PortHint != 9000 || py.Engine != "" || py.PID != 3 {
		t.Errorf("python3 = %+v, want --port 9000, no engine match, pid 3", py)
	}
	if len(list) != 2 {
		t.Fatalf("snapshot = %+v, want ollama and the --port process", list)
	}
}

// SnapshotAt uses the caller's clock for the refresh window and CPU dt, so
// a collector that injects time does not pick up a wall-clock read here.
func TestSnapshotAtUsesGivenTime(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	var calls int
	var ticks uint64 = 100
	platformList = func() ([]raw, error) {
		calls++
		return []raw{{pid: 1, name: "ollama", args: []string{"ollama", "serve"}, ticks: ticks}}, nil
	}
	s := NewSampler()
	s.minRefresh = time.Second
	t0 := time.Unix(1_000, 0)

	first := s.SnapshotAt(t0)
	if len(first) != 1 || first[0].Name != "ollama" {
		t.Fatalf("first snapshot = %+v", first)
	}
	_ = s.SnapshotAt(t0.Add(100 * time.Millisecond))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 inside minRefresh", calls)
	}

	ticks = 200
	second := s.SnapshotAt(t0.Add(2 * time.Second))
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 after the window", calls)
	}
	if len(second) != 1 {
		t.Fatalf("second snapshot = %+v", second)
	}
	// 100 ticks over 2s at USER_HZ 100: 100/100/2*100 = 50% of one core.
	if second[0].CPUPct != 50 {
		t.Fatalf("CPUPct = %v, want 50 from injected dt", second[0].CPUPct)
	}
}

func TestSnapshotAtZeroTickBaseline(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	var ticks uint64
	platformList = func() ([]raw, error) {
		return []raw{{pid: 1, name: "ollama", args: []string{"ollama", "serve"}, ticks: ticks}}, nil
	}
	s := NewSampler()
	s.minRefresh = 0
	now := time.Unix(1_000, 0)
	first := s.SnapshotAt(now)
	if len(first) != 1 || first[0].CPUPct != 0 {
		t.Fatalf("first snapshot = %+v, want one process without a rate", first)
	}

	ticks = clkTck
	second := s.SnapshotAt(now.Add(time.Second))
	if len(second) != 1 || second[0].CPUPct != 100 {
		t.Fatalf("second snapshot = %+v, want 100%% CPU from zero-tick baseline", second)
	}
}

func TestSnapshotKeepsLastGoodOnError(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	platformList = func() ([]raw, error) {
		return []raw{{pid: 1, name: "ollama", args: []string{"ollama", "serve"}}}, nil
	}
	s := NewSampler()
	first := s.Snapshot()
	if len(first) != 1 || first[0].Name != "ollama" {
		t.Fatalf("warm snapshot = %+v", first)
	}

	platformList = func() ([]raw, error) { return nil, errors.New("cim timeout") }
	second := s.Snapshot()
	if len(second) != 1 || second[0].Name != "ollama" {
		t.Fatalf("listing error dropped the last good snapshot: %+v", second)
	}
}

// Derived CPU percentages saturate at zero (counter resets must not read as
// negative load) and cap at 100 cores: many-core boxes legitimately exceed
// 100% of one core, but a runaway multiplier must stay bounded.
func TestClampPctBounds(t *testing.T) {
	cases := map[float64]float64{
		-1:            0,
		0:             0,
		55.5:          55.5,
		100 * 1024:    100 * 1024,
		100*1024 + .5: 100 * 1024,
	}
	for in, want := range cases {
		if got := clampPct(in); got != want {
			t.Errorf("clampPct(%v) = %v, want %v", in, got, want)
		}
	}
	if got := clampPct(math.NaN()); got != 0 {
		t.Errorf("clampPct(NaN) = %v, want 0", got)
	}
}
