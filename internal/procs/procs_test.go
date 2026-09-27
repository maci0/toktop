package procs

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// annotateRaw applies the derivation a platform lister performs, so a
// stubbed listing hands SnapshotAt the same shape a real one does.
func annotateRaw(r raw) raw {
	annotate(&r)
	return r
}

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
		{[]string{"x", "--listen-port=7001"}, 7001},
		// An unusable value on one spelling must not hide a good one later.
		{[]string{"x", "--http-port=abc", "--port=4242"}, 4242},
		{[]string{"x", "--port=abc", "--http-port", "5002"}, 5002},
		// A bare flag with no following argument is not a port.
		{[]string{"x", "--port"}, 0},
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

// A listing keeps only the prefix a consumer reads, so the tail of a long
// command line (a browser's flags, an inline prompt, a credential) is not
// retained for the life of the sampler, while everything inside the prefix
// still decides the match and the port.
func TestClipArgsKeepsOnlyTheReadablePrefix(t *testing.T) {
	args := append([]string{"ollama", "serve", "--port", "11434"}, hugeArgs(64, 1024)...)
	got := ClipArgs(args)

	n := 0
	for i, a := range got {
		if i > 0 {
			n++ // the separator a join writes between two arguments
		}
		n += len(a)
	}
	if n > CmdlinePrefix {
		t.Fatalf("retained %d bytes of command line, bound is %d", n, CmdlinePrefix)
	}
	if len(got) >= len(args) {
		t.Fatalf("kept %d of %d arguments: the tail past the prefix was retained", len(got), len(args))
	}
	if !slices.Equal(got[:4], args[:4]) {
		t.Fatalf("clipped argv lost a leading argument: %q", got[:min(4, len(got))])
	}
	if !utf8.ValidString(got[len(got)-1]) {
		t.Fatalf("ClipArgs split a character: %q", got[len(got)-1])
	}
	eng, def, ok := MatchEngine(Info{Name: "ollama", Args: got, PortHint: ExtractPort(got)})
	if !ok || eng != "ollama" || def != 11434 {
		t.Fatalf("match over the clipped command line = %q/%d/%v, want ollama/11434/true", eng, def, ok)
	}
	if p := ExtractPort(got); p != 11434 {
		t.Fatalf("ExtractPort over the clipped command line = %d, want 11434", p)
	}
}

// A command line inside the bound is handed back with every argument intact.
// The elements are cloned, because a listing builds them as windows onto one
// whole /proc/PID/cmdline buffer: sharing them would pin the whole line, and
// with it the prompt or path past the bound, for the life of the sampler.
func TestClipArgsLeavesAShortCommandLineAlone(t *testing.T) {
	args := []string{"vllm", "serve", "--port", "8001"}
	got := ClipArgs(args)
	if len(got) != len(args) {
		t.Fatalf("ClipArgs = %q, want %q", got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("ClipArgs = %q, want %q", got, args)
		}
		if &got[i] == &args[i] {
			t.Fatalf("ClipArgs kept argument %d aliasing the caller's slice, so the whole command line stays retained", i)
		}
	}
	if got := ClipArgs(nil); len(got) != 0 {
		t.Fatalf("ClipArgs(nil) = %q, want empty", got)
	}
}

// The bound is a property of the listing, not of the matchers alone: a lister
// that hands the whole line to annotate must come back holding the prefix.
func TestAnnotateClipsTheRetainedCommandLine(t *testing.T) {
	r := raw{pid: 1, name: "ollama", args: append([]string{"ollama", "serve"}, hugeArgs(8, 4096)...)}
	annotate(&r)

	total := 0
	for i, a := range r.args {
		if i > 0 {
			total++
		}
		total += len(a)
	}
	if total > CmdlinePrefix {
		t.Fatalf("annotate retained %d bytes, bound is %d", total, CmdlinePrefix)
	}
	if r.engine != "ollama" || r.defPort != 11434 {
		t.Fatalf("annotate over the clipped line = %q/%d, want ollama/11434", r.engine, r.defPort)
	}
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

func TestClipUTF8PrefixDoesNotSplitUTF8(t *testing.T) {
	// 4095 ASCII bytes plus é (U+00E9, two UTF-8 bytes). A raw s[:4096]
	// keeps 0xC3 and drops 0xA9, so ToLower would run on invalid UTF-8.
	a := strings.Repeat("x", matchJoinBytes-1) + "é"
	got := clipUTF8Prefix(a, matchJoinBytes)
	if !utf8.ValidString(got) {
		t.Fatalf("clipUTF8Prefix split a character: %q is not valid UTF-8", got)
	}
	if strings.HasSuffix(got, "é") {
		t.Fatal("clipUTF8Prefix kept a character that does not fit in the byte cap")
	}
	if len(got) != matchJoinBytes-1 {
		t.Fatalf("clipUTF8Prefix length = %d, want %d (ASCII prefix only)", len(got), matchJoinBytes-1)
	}
	joined := lowerJoinedArgs([]string{a})
	if !utf8.ValidString(joined) {
		t.Fatalf("lowerJoinedArgs split a character: %q", joined)
	}
	if len(joined) > matchJoinBytes {
		t.Fatalf("joined command is %d bytes, cap is %d", len(joined), matchJoinBytes)
	}
}

func TestAnyArgContainsSharesByteBudget(t *testing.T) {
	// A match must never depend on anything past CmdlinePrefix bytes, so the
	// budget is spent across arguments, not reset per argument.
	args := []string{strings.Repeat("x", matchJoinBytes), "vllm.entrypoints"}
	if anyArgContains(args, "vllm.entrypoints") {
		t.Fatal("anyArgContains matched past the shared byte budget")
	}
	if !anyArgContains([]string{"python", "-m", "vllm.entrypoints"}, "vllm.entrypoints") {
		t.Fatal("anyArgContains missed a match inside the budget")
	}
}

// toktop must never count itself as an engine. The lister is stubbed to
// return this very process under an engine-looking name and command line, so
// the self-skip is what the assertion is about; reading the host's real
// process table would leave the entry absent for two other reasons and pass
// even with the skip removed.
func TestSelfIsSkipped(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })
	platformList = func() ([]raw, error) {
		list := []raw{
			{pid: os.Getpid(), name: "ollama", args: []string{"ollama", "serve"}},
			{pid: os.Getpid() + 1, name: "ollama", args: []string{"ollama", "serve"}},
		}
		for i := range list {
			annotate(&list[i]) // every real lister derives engine/port this way
		}
		return list, nil
	}

	for _, list := range [][]Info{NewSampler().Snapshot(), Snapshot()} {
		for _, p := range list {
			if p.PID == os.Getpid() {
				t.Errorf("toktop's own test process leaked in: %+v", p)
			}
		}
		// The sibling is otherwise a perfect match, so its presence is what
		// shows the stub was actually in force.
		found := false
		for _, p := range list {
			if p.PID == os.Getpid()+1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("stubbed listing did not reach the snapshot: %+v", list)
		}
	}
}

func TestSnapshotDropsUnrelatedProcesses(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	platformList = func() ([]raw, error) {
		list := []raw{
			{pid: 1, name: "ollama", args: []string{"ollama", "serve"}},
			{pid: 2, name: "firefox", args: []string{"firefox"}},
			{pid: 3, name: "python3", args: []string{"python3", "--port", "9000"}},
		}
		for i := range list {
			annotate(&list[i])
		}
		return list, nil
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
		return []raw{annotateRaw(raw{pid: 1, name: "ollama", args: []string{"ollama", "serve"}, ticks: ticks})}, nil
	}
	s := NewSampler()
	s.refreshMin = time.Second
	t0 := time.Unix(1_000, 0)

	first := s.SnapshotAt(t0)
	if len(first) != 1 || first[0].Name != "ollama" {
		t.Fatalf("first snapshot = %+v", first)
	}
	_ = s.SnapshotAt(t0.Add(100 * time.Millisecond))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 inside refreshMin", calls)
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
		return []raw{annotateRaw(raw{pid: 1, name: "ollama", args: []string{"ollama", "serve"}, ticks: ticks})}, nil
	}
	s := NewSampler()
	s.refreshMin = 0
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
		return []raw{annotateRaw(raw{pid: 1, name: "ollama", args: []string{"ollama", "serve"}})}, nil
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

func TestSnapshotDetachedFromCache(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	platformList = func() ([]raw, error) {
		return []raw{annotateRaw(raw{pid: 1, name: "ollama", args: []string{"ollama", "serve"}})}, nil
	}
	s := NewSampler()
	first := s.Snapshot()
	first[0].Name = "mutated"
	second := s.Snapshot()
	if second[0].Name != "ollama" {
		t.Fatalf("snapshot aliased the cache: %+v", second)
	}
}

func TestSnapshotErrorThrottled(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	calls := 0
	platformList = func() ([]raw, error) {
		calls++
		return nil, errors.New("timeout")
	}
	s := NewSampler()
	s.refreshMin = 10 * time.Second
	now := time.Now()
	_ = s.SnapshotAt(now)
	_ = s.SnapshotAt(now.Add(time.Second))
	if calls != 1 {
		t.Fatalf("error was not throttled by refreshMin: calls = %d, want 1", calls)
	}
}

// A listing error with nothing cached returns an empty slice, which the UI
// renders as a blank process panel. A host that can never list (no PowerShell
// on Windows, no /proc) is indistinguishable from a host running no engines
// unless the failure is recorded, so the first such failure is audited and a
// later one that has a snapshot to fall back on is not.
func TestSnapshotErrorAuditedWhenNoCache(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	buf := &syncBuffer{}
	oldAudit := audit
	audit = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	t.Cleanup(func() { audit = oldAudit })

	fail := errors.New("no PowerShell implementation present")
	platformList = func() ([]raw, error) { return nil, fail }
	s := NewSampler()
	s.refreshMin = 0
	if got := s.SnapshotAt(time.Now()); len(got) != 0 {
		t.Fatalf("snapshot with no cache = %+v, want empty", got)
	}
	line := buf.String()
	if !strings.Contains(line, "process listing failed") {
		t.Fatalf("no audit line for a listing with no fallback: %s", line)
	}
	if !strings.Contains(line, "no PowerShell") {
		t.Fatalf("audit line lost the cause: %s", line)
	}

	// With a snapshot to fall back on, the failure is a transient blip and
	// the throttled sweep that returns stale processes must stay quiet.
	buf.Reset()
	platformList = func() ([]raw, error) {
		return []raw{annotateRaw(raw{pid: 42, name: "ollama", args: []string{"ollama", "serve"}})}, nil
	}
	_ = s.SnapshotAt(time.Now())
	platformList = func() ([]raw, error) { return nil, fail }
	if got := s.SnapshotAt(time.Now()); len(got) != 1 {
		t.Fatalf("snapshot after a failure = %+v, want the last good one", got)
	}
	if line := buf.String(); strings.Contains(line, "process listing failed") {
		t.Fatalf("a failure with a snapshot to return was audited: %s", line)
	}
}

// syncBuffer is the bytes.Buffer the audit logger writes through, with the
// lock a plain one lacks. A sweep runs on the calling goroutine, so nothing
// writes concurrently here, but the handler is shared with whatever the
// package's own goroutines log.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// The Windows lister names a PowerShell that must exist on the host. Which of
// the two implementations an image carries is not fixed, so a name that is
// missing there silently costs the whole process listing. Pin the preference
// order and the miss.
func TestPickShell(t *testing.T) {
	present := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, p := range names {
				if p == n {
					return "/usr/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	for _, tc := range []struct {
		name string
		have []string
		want string
	}{
		{"both installed", []string{"pwsh", "powershell"}, "pwsh"},
		{"only the legacy shell", []string{"powershell"}, "powershell"},
		{"only the supported shell", []string{"pwsh"}, "pwsh"},
		{"neither", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickShell(present(tc.have...), "pwsh", "powershell"); got != tc.want {
				t.Errorf("pickShell = %q, want %q", got, tc.want)
			}
		})
	}
}

// A listing slow enough to outlast the refresh window must not be joined by a
// second one. The window is a rate limit, not a lock: on Windows the CIM
// enumeration costs more than refreshMin, so every caller arriving while it
// ran used to start its own sweep of the same process table, and whichever
// finished last published the older listing over the newer one. The stub
// blocks the first call so the overlap is certain rather than timed.
func TestSnapshotDoesNotSweepTwiceAtOnce(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	platformList = func() ([]raw, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		list := []raw{{pid: 1, name: "ollama", args: []string{"ollama", "serve"}}}
		for i := range list {
			annotate(&list[i])
		}
		return list, nil
	}

	s := NewSampler()
	base := time.Unix(1700000000, 0)
	done := make(chan []Info, 1)
	go func() { done <- s.SnapshotAt(base) }()
	<-entered

	// An hour later, so the refresh window cannot be what holds this caller
	// off: the in-flight claim is the only thing standing between one sweep
	// and two.
	if got := s.SnapshotAt(base.Add(time.Hour)); len(got) != 0 {
		t.Errorf("mid-sweep caller got %d entries, want the previous (empty) snapshot", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("platformList called %d times, want 1 while a sweep is in flight", n)
	}

	close(release)
	list := <-done
	if len(list) != 1 || list[0].Engine != "ollama" {
		t.Fatalf("snapshot = %+v, want the one ollama process", list)
	}
	// The claim is released on both the success and the error path, so the
	// sampler keeps listing after a sweep that ran long.
	if after := s.SnapshotAt(base.Add(2 * time.Hour)); len(after) != 1 {
		t.Errorf("snapshot after the sweep = %+v, want the sweep released", after)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("platformList called %d times, want 2", n)
	}
}

// A failed listing releases the in-flight claim too, or every later caller
// reads the last good snapshot forever.
func TestSnapshotFailedSweepReleasesClaim(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	var calls atomic.Int32
	platformList = func() ([]raw, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("listing failed")
		}
		list := []raw{{pid: 1, name: "ollama", args: []string{"ollama", "serve"}}}
		for i := range list {
			annotate(&list[i])
		}
		return list, nil
	}

	s := NewSampler()
	// An explicit clock, an hour apart: the refresh window is a rate limit on
	// the listing, and on Windows it is wide enough that two Snapshot calls
	// back to back would answer from the cache and never reach the retry this
	// pins. Stepping past the window leaves the in-flight claim as the only
	// thing that could hold the second call off.
	base := time.Unix(1700000000, 0)
	if got := s.SnapshotAt(base); len(got) != 0 {
		t.Errorf("failed sweep returned %+v, want no processes", got)
	}
	if got := s.SnapshotAt(base.Add(time.Hour)); len(got) != 1 {
		t.Errorf("snapshot after a failed sweep = %+v, want the listing retried", got)
	}
}

// Engine matching folds a process name and its command line with
// core.FoldASCII, not strings.ToLower, because the matchers test ASCII
// literals against bytes a /proc listing hands over verbatim. ToLower also
// folds runes whose lowercase form is ASCII, so a binary invoked through a
// U+212A (KELVIN SIGN) reaches a matcher as "kvllm" and is claimed as a vLLM:
// an engine identity assigned to whatever the operator happened to launch.
func TestEngineMatchRejectsUnicodeFoldedNames(t *testing.T) {
	for _, info := range []Info{
		// The needle is what carries the rune here: vllm matches on
		// "vllm.entrypoints" appearing in an argument, and a U+212A in place
		// of the k folds to "k" under ToLower and satisfies a substring
		// match on a path no vLLM install has.
		{Name: "node", Args: []string{"node", "/srv/vll\u212A.entrypoints.openai.api_server"}},
		{Name: "node", Args: []string{"node", "-m", "sglan\u212A.srt.entrypoints.http_server"}},
		{Name: "node", Args: []string{"node", "/opt/litell\u212A.proxy.proxy_server"}},
		{Name: "node", Args: []string{"node", "/opt/gpustac\u212A.start"}},
		{Name: "node", Args: []string{"node", "-m", "vll\u0130m.entrypoints.openai.api_server"}},
		{Name: "ollama-run", Args: []string{"ollama-run", "ollama \u212Aserve"}},
	} {
		if engine, _, ok := MatchEngine(info); ok {
			t.Errorf("MatchEngine(%+v) = %q, want no engine", info, engine)
		}
	}
	// The ASCII spellings still match, which is what the fold is for.
	for _, info := range []Info{
		{Name: "tritonserver"},
		{Name: "vllm", Args: []string{"vllm", "serve"}},
	} {
		if _, _, ok := MatchEngine(info); !ok {
			t.Errorf("MatchEngine(%+v) matched no engine, want the ASCII spelling to match", info)
		}
	}
}

// A name carrying a byte that is not valid UTF-8 is a legal /proc name. The
// fold must leave it alone: strings.ToLower rewrote it to U+FFFD, which is
// not the name the process is running under.
func TestBaseNameKeepsInvalidBytes(t *testing.T) {
	if got := baseName("vllm\xff"); got != "vllm\xff" {
		t.Errorf("baseName(%q) = %q, want the invalid byte preserved", "vllm\xff", got)
	}
}
