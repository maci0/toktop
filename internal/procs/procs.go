// Package procs discovers local inference engines by inspecting running
// processes rather than guessing ports. Everything comes from procfs,
// ps(1) or Win32 CIM - no vendor libraries.
package procs

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Info is one sampled process relevant to engine discovery or accounting.
type Info struct {
	PID      int
	Name     string // executable / comm
	Args     []string
	RSS      uint64  // resident memory, bytes
	CPUPct   float64 // percent of one core
	PortHint int     // --port found on the command line
	Engine   string  // matched well-known engine id
	DefPort  int     // the matched engine's default port
}

// raw is the platform-sampled record before delta math.
type raw struct {
	pid        int
	name       string
	args       []string
	rss        uint64
	cpuPercent float64 // provided directly by the OS tooling when available
	ticks      uint64  // cumulative CPU jiffies (linux path)
}

// platformList is implemented per GOOS.
var platformList func() ([]raw, error)

// clkTck is the jiffies-per-second constant on the linux path. USER_HZ is
// fixed at 100 by the Linux ABI; there is no runtime probe.
const clkTck = 100

// Sampler turns raw process listings into Infos, deriving CPU percentage on
// linux from tick deltas between samples.
type Sampler struct {
	mu         sync.Mutex
	prev       map[int]uint64
	last       time.Time // last poll attempt (for minRefresh throttling)
	lastSample time.Time // last successful poll (for CPU tick delta dt)

	// minRefresh throttles expensive OS tooling (PowerShell CIM on Windows
	// takes seconds); within the window the previous snapshot is returned.
	minRefresh time.Duration
	cached     []Info
}

func NewSampler() *Sampler {
	return &Sampler{prev: map[int]uint64{}, minRefresh: defaultSamplerRefresh}
}

// packageSampler is the process-wide engine sampler. Discovery and the
// collector both list through it so Windows CIM listings and CPU tick
// deltas are not taken twice. OnceValue so NewSampler runs after platform
// init has set defaultSamplerRefresh.
var packageSampler = sync.OnceValue(NewSampler)

// Snapshot lists engine processes using the process-wide sampler.
func Snapshot() []Info {
	return packageSampler().Snapshot()
}

// Snapshot lists processes, best effort. Returns nil on unsupported/erroring
// platforms so callers can degrade silently.
func (s *Sampler) Snapshot() []Info {
	return s.SnapshotAt(time.Now())
}

// SnapshotAt is Snapshot with the caller's clock. CPU tick deltas and the
// minRefresh window use now, so a collector that injects time does not pick
// up a second wall-clock read inside the sampler.
func (s *Sampler) SnapshotAt(now time.Time) []Info {
	if platformList == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.minRefresh > 0 && !s.last.IsZero() && now.Sub(s.last) < s.minRefresh {
		return slices.Clone(s.cached)
	}
	list, err := platformList()
	s.last = now
	if err != nil {
		return slices.Clone(s.cached) // last good snapshot; a transient listing error is not "no processes"
	}
	var dt float64
	if !s.lastSample.IsZero() {
		dt = now.Sub(s.lastSample).Seconds()
	}
	s.lastSample = now

	self := os.Getpid()
	out := make([]Info, 0, len(list))
	for _, r := range list {
		if r.pid == self {
			continue
		}
		info := Info{
			PID:      r.pid,
			Name:     r.name,
			Args:     r.args,
			RSS:      r.rss,
			PortHint: ExtractPort(r.args),
		}
		if eng, defPort, ok := MatchEngine(info); ok {
			info.Engine, info.DefPort = eng, defPort
		} else if info.PortHint == 0 {
			continue // not an engine and no listen-port flag: drop it
		}
		switch {
		case r.cpuPercent > 0:
			info.CPUPct = r.cpuPercent
		case dt > 0:
			pticks, tracked := s.prev[r.pid]
			if tracked && r.ticks >= pticks {
				info.CPUPct = clampPct(float64(r.ticks-pticks) / clkTck / dt * 100)
			}
		}
		if _, tracked := s.prev[r.pid]; !tracked || r.ticks != 0 {
			s.prev[r.pid] = r.ticks
		}
		out = append(out, info)
	}
	// prune dead pids so the map cannot grow forever
	live := make(map[int]struct{}, len(out))
	for _, p := range out {
		live[p.PID] = struct{}{}
	}
	for pid := range s.prev {
		if _, ok := live[pid]; !ok {
			delete(s.prev, pid)
		}
	}
	s.cached = out
	return slices.Clone(out)
}

// clampPct bounds a derived CPU percentage. Counter resets must not read as
// negative load, and a runaway multiplier must stay bounded; many-core boxes
// legitimately exceed 100% of one core.
func clampPct(v float64) float64 {
	if !(v > 0) { // also catches NaN: every comparison with it is false
		return 0
	}
	if v > 100*1024 {
		return 100 * 1024
	}
	return v
}

// ExtractPort scans argv for explicit listen-port flags. Exported so the
// remote ssh path can reuse the same convention for command lines gathered
// from another host. Anything outside the TCP port range reads as absent: a
// garbage or hostile --port must never become a tunnel target.
func ExtractPort(args []string) int {
	for i, a := range args {
		for _, flag := range []string{"--port", "--http-port", "--listen-port"} {
			if a == flag && i+1 < len(args) {
				if p, err := strconv.Atoi(strings.TrimSpace(args[i+1])); err == nil && isPort(p) {
					return p
				}
			}
			if after, ok := strings.CutPrefix(a, flag+"="); ok {
				if p, err := strconv.Atoi(after); err == nil && isPort(p) {
					return p
				}
			}
		}
	}
	return 0
}

// isPort reports whether p is a number a process can listen on.
func isPort(p int) bool { return p >= 1 && p <= 65535 }

// ListenPort returns the process's effective listen port: an explicit --port
// flag on the command line when present, else the matched engine's default.
// Zero when neither applies (DefPort is only set for engine matches).
func (i Info) ListenPort() int {
	if i.PortHint != 0 {
		return i.PortHint
	}
	return i.DefPort
}

// engineMatcher identifies well-known serving processes. Matching is
// deliberately conservative to avoid grabbing unrelated processes.
type engineMatcher struct {
	engine  string
	defPort int
	match   func(name string, lowerCmd string, args []string) bool
}

func baseName(n string) string {
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	if i := strings.LastIndexByte(n, '\\'); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(n), ".exe")
}

func anyArgContains(args []string, subs ...string) bool {
	for _, a := range args {
		la := strings.ToLower(clipArg(a))
		for _, sub := range subs {
			if strings.Contains(la, sub) {
				return true
			}
		}
	}
	return false
}

// matchJoinArgs / matchJoinBytes bound the command-line string MatchEngine
// builds. Engine names sit in the first arguments; browsers and Electron
// apps trail tens to hundreds of kilobytes of flags, and joining those on
// every /proc poll was a large alloc per process per interval.
const (
	matchJoinArgs  = 12
	matchJoinBytes = 4096
)

// clipArg keeps the prefix of a single argument that engine matchers look
// at. A Chrome --disable-features blob is tens of kilobytes and never an
// engine module path.
func clipArg(a string) string {
	return clipUTF8Prefix(a, matchJoinBytes)
}

// clipUTF8Prefix keeps at most n bytes of s, ending on a code-point
// boundary so a cut cannot leave a dangling lead byte (é as 0xC3).
func clipUTF8Prefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// lowerJoinedArgs is the command line engine matchers search, lowercased,
// capped so a process with a huge argv cannot force a huge allocation.
func lowerJoinedArgs(args []string) string {
	var b strings.Builder
	n := min(len(args), matchJoinArgs)
	for i := 0; i < n; i++ {
		if b.Len() >= matchJoinBytes {
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		a := args[i]
		remain := matchJoinBytes - b.Len()
		if len(a) > remain {
			a = clipUTF8Prefix(a, remain)
		}
		b.WriteString(strings.ToLower(a))
	}
	return b.String()
}

var engineMatchers = []engineMatcher{
	{"ollama", 11434, func(n, c string, _ []string) bool {
		return n == "ollama" || strings.Contains(c, "ollama serve")
	}},
	{"llama.cpp", 8080, func(n, c string, _ []string) bool {
		return strings.Contains(n, "llama-server") || strings.Contains(c, "llama-server") ||
			strings.Contains(n, "llamafile")
	}},
	{"koboldcpp", 5001, func(_, c string, _ []string) bool {
		return strings.Contains(c, "koboldcpp")
	}},
	{"vllm", 8000, func(_, _ string, args []string) bool {
		return anyArgContains(args, "vllm.entrypoints", "/vllm") ||
			baseNameEq(args, "vllm")
	}},
	{"sglang", 30000, func(_, _ string, args []string) bool {
		// python -m sglang.launch_server / sglang.srt.*, and the
		// `sglang serve` CLI (same shape as `vllm serve`).
		return anyArgContains(args, "sglang.launch_server", "sglang.srt") ||
			baseNameEq(args, "sglang")
	}},
	{"triton", 8000, func(n, _ string, _ []string) bool { return n == "tritonserver" }},
	{"tgi", 8080, func(_, c string, _ []string) bool {
		return strings.Contains(c, "text-generation-launcher")
	}},
	{"tabbyapi", 5000, func(n, _ string, _ []string) bool { return n == "tabbyapi" }},
	{"oobabooga", 7860, func(_, c string, _ []string) bool {
		return strings.Contains(c, "text-generation-webui") || strings.Contains(c, "oobabooga")
	}},
	{"localai", 8080, func(n, _ string, _ []string) bool {
		return n == "localai" || n == "local-ai"
	}},
	{"litellm", 4000, func(_, _ string, args []string) bool {
		return baseNameEq(args, "litellm") || anyArgContains(args, "litellm.proxy")
	}},
	{"mlx", 8080, func(_, c string, _ []string) bool {
		return strings.Contains(c, "mlx_lm.server") || strings.Contains(c, "mlx-lm")
	}},
	{"lmstudio", 1234, func(n, _ string, _ []string) bool {
		return strings.Contains(n, "lm-studio") || strings.Contains(n, "lmstudio") ||
			strings.Contains(n, "lm studio")
	}},
	{"gpustack", 80, func(_, _ string, args []string) bool {
		return anyArgContains(args, "gpustack.start")
	}},
	{"lemonade", 8000, func(n, _ string, _ []string) bool { return n == "lemonade-server" || n == "lemond" }},
	{"gpt4all", 4891, func(n, _ string, _ []string) bool { return n == "gpt4all" }},
	{"jan", 1337, func(n, _ string, _ []string) bool { return n == "jan" }},
	{"ramalama", 8080, func(_, c string, _ []string) bool {
		return strings.Contains(c, "ramalama")
	}},
}

func baseNameEq(args []string, want string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return baseName(a) == want })
}

// MatchEngine finds the well-known engine behind a process, if any. The
// name is basename'd internally, so raw argv[0] works. Exported for the
// remote ssh path, which matches command lines gathered from another host.
func MatchEngine(i Info) (engine string, defPort int, ok bool) {
	name := baseName(i.Name)
	lowerCmd := lowerJoinedArgs(i.Args)
	for _, m := range engineMatchers {
		if m.match(name, lowerCmd, i.Args) {
			return m.engine, m.defPort, true
		}
	}
	return "", 0, false
}

// defaultSamplerRefresh is set by platform files when OS tooling needs
// throttling (windows). Zero means every Snapshot call re-lists.
var defaultSamplerRefresh time.Duration
