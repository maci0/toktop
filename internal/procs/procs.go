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

	"github.com/maci0/toktop/internal/core"
)

// Info is one sampled process relevant to engine discovery or accounting.
type Info struct {
	PID      int
	Name     string   // executable / comm
	Args     []string // leading CmdlinePrefix bytes of the command line; the tail is not retained
	RSS      uint64   // resident memory, bytes
	CPUPct   float64  // percent of one core
	PortHint int      // --port found on the command line
	Engine   string   // matched well-known engine id
	DefPort  int      // the matched engine's default port
}

// raw is the platform-sampled record before delta math.
type raw struct {
	pid        int
	name       string
	args       []string
	rss        uint64
	cpuPercent float64 // provided directly by the OS tooling when available
	ticks      uint64  // cumulative CPU jiffies (linux path)

	// port/engine/defPort are derived from name and args by annotate,
	// which every platform lister calls. The /proc walk needs them to
	// filter before reading /proc/PID/stat, and deriving them again in
	// SnapshotAt rebuilt the joined command line and walked every matcher
	// a second time for each process it kept.
	port    int
	engine  string
	defPort int
}

// annotate derives the listen-port hint and engine match from the command
// line the lister already read, and keeps only the prefix of it a consumer
// can read. Both derivations run on the clipped command line, so what a
// listing retains and what it reports are the same bytes.
func annotate(r *raw) {
	r.args = ClipArgs(r.args)
	r.port = ExtractPort(r.args)
	if eng, defPort, ok := MatchEngine(Info{Name: r.name, Args: r.args}); ok {
		r.engine, r.defPort = eng, defPort
	}
}

// platformList is implemented per GOOS.
var platformList func() ([]raw, error)

// pickShell returns the first name look resolves, or "" when none of them are
// installed. Windows ships two PowerShell implementations and which one is
// present varies by image: pwsh (PowerShell 7) is the supported line, and
// Windows PowerShell 5.1 is an optional feature that Server Core and trimmed
// images leave out, while an upgraded workstation may have only pwsh. Hard
// coding either name loses process listing on the other half of the claim.
func pickShell(look func(string) (string, error), names ...string) string {
	for _, n := range names {
		if _, err := look(n); err == nil {
			return n
		}
	}
	return ""
}

// clkTck is the jiffies-per-second constant on the linux path. USER_HZ is
// fixed at 100 by the Linux ABI; there is no runtime probe.
const clkTck = 100

// Sampler turns raw process listings into Infos, deriving CPU percentage on
// linux from tick deltas between samples.
type Sampler struct {
	mu         sync.Mutex
	prev       map[int]uint64
	last       time.Time // last poll attempt (for refreshMin throttling)
	lastSample time.Time // last successful poll (for CPU tick delta dt)
	// sweeping is set for the duration of the unlocked listing. The refresh
	// window alone cannot hold a second sweep off: a Windows CIM enumeration
	// outlasts refreshMin, so every caller arriving past the window started
	// its own sweep of one process table, and whichever finished last
	// published the older listing and a lastSample stamped before a newer one.
	sweeping bool

	// refreshMin throttles expensive OS tooling (PowerShell CIM on Windows
	// takes seconds); within the window the previous snapshot is returned.
	refreshMin time.Duration
	cached     []Info
}

// NewSampler returns a Sampler with the platform's default refresh window.
// A caller that lists processes more than once in a run should share one
// sampler: the underlying listing is throttled, and the CPU tick deltas it
// keeps are only meaningful across successive calls.
func NewSampler() *Sampler {
	return &Sampler{prev: map[int]uint64{}, refreshMin: defaultSamplerRefresh}
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

// Snapshot lists processes, best effort. Returns nil when the platform is
// unsupported or nothing has been listed yet; a listing error returns the
// last good snapshot, so callers see stale processes rather than none.
func (s *Sampler) Snapshot() []Info {
	return s.SnapshotAt(time.Now())
}

// SnapshotAt is Snapshot with the caller's clock. CPU tick deltas and the
// refreshMin window use now, so a collector that injects time does not pick
// up a second wall-clock read inside the sampler.
func (s *Sampler) SnapshotAt(now time.Time) []Info {
	if platformList == nil {
		return nil
	}
	// The listing runs unlocked: platformList spawns the OS process table, and
	// on Windows CIM enumeration takes seconds, so holding s.mu across it
	// pinned every other caller of this sampler for the whole sweep, the
	// cached fast path included. The throttle is claimed under the lock
	// instead, together with the in-flight flag, so a caller arriving
	// mid-sweep gets the previous snapshot rather than a second sweep, and
	// the tick math below still runs as one critical section.
	s.mu.Lock()
	if s.sweeping || (s.refreshMin > 0 && !s.last.IsZero() && core.Age(now, s.last) < s.refreshMin) {
		out := slices.Clone(s.cached)
		s.mu.Unlock()
		return out
	}
	s.last = now
	s.sweeping = true
	s.mu.Unlock()

	list, err := platformList()
	s.mu.Lock()
	s.sweeping = false
	defer s.mu.Unlock()
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
			PortHint: r.port,
			Engine:   r.engine,
			DefPort:  r.defPort,
		}
		if info.Engine == "" && info.PortHint == 0 {
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

// portFlags and portFlagsEq are the argv spellings that carry an explicit
// listen port, as bare flags and as "flag=value". Hoisted to package
// tables because the /proc walk calls ExtractPort for every process on
// every poll: rebuilding these lists per argument allocated on each of the
// thousands of iterations one walk makes.
var (
	portFlags   = []string{"--port", "--http-port", "--listen-port"}
	portFlagsEq = []string{"--port=", "--http-port=", "--listen-port="}
)

// ExtractPort scans argv for explicit listen-port flags. Exported so the
// remote ssh path can reuse the same convention for command lines gathered
// from another host. Anything outside the TCP port range reads as absent: a
// garbage or hostile --port must never become a tunnel target.
func ExtractPort(args []string) int {
	for i, a := range args {
		for _, flag := range portFlagsEq {
			if after, ok := strings.CutPrefix(a, flag); ok {
				if p, err := strconv.Atoi(after); err == nil && isPort(p) {
					return p
				}
			}
		}
		if i+1 < len(args) {
			for _, flag := range portFlags {
				if a == flag {
					if p, err := strconv.Atoi(strings.TrimSpace(args[i+1])); err == nil && isPort(p) {
						return p
					}
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

// anyArgContains reports whether any argument holds one of subs. The
// arguments draw on one matchJoinBytes budget, not one per argument, so the
// matchers read no further into a command line than CmdlinePrefix allows.
// Clipping per argument matters on its own: a Chrome --disable-features blob
// is tens of kilobytes and never an engine module path.
func anyArgContains(args []string, subs ...string) bool {
	budget := matchJoinBytes
	for _, a := range args {
		if budget <= 0 {
			return false
		}
		la := strings.ToLower(clipUTF8Prefix(a, budget))
		budget -= len(la)
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

// CmdlinePrefix is the longest leading slice of a command line any engine
// matcher reads, so a command line longer than this cannot change a match. A
// scan of another host's processes (internal/remote) ships at most this many
// bytes per process, and a local listing keeps at most this many (see
// ClipArgs): the tail is where an agent's inline prompt, a file path or a
// credential sits, and none of it is read on this side either.
const CmdlinePrefix = matchJoinBytes

// ClipArgs keeps the leading CmdlinePrefix bytes of a command line, the same
// bound the ssh sweep ships and the matchers read, and drops what is past
// them. A local listing reads /proc/PID/cmdline whole: a browser, an Electron
// app or an agent started with a long inline script puts hundreds of
// kilobytes on that line, and the tail past the prefix is exactly where a
// home directory, an inline prompt or a credential sits. None of it is read
// by anything here, and the Info a listing produces is retained for the life
// of the sampler, so holding it costs memory and keeps personal data in a
// structure no consumer ever asks for.
//
// The budget is spent across arguments rather than per argument, so it counts
// what a joined command line counts: the separator between two arguments is a
// byte of that line. An argument that straddles the cut is clipped rather
// than dropped whole, the way the sweep's cut -c clips mid-token.
//
// Exported for the sweep's own reader, which splits a line that was cut in
// characters, so the Info it builds is bounded in bytes the same way.
func ClipArgs(args []string) []string {
	spent := 0
	for i, a := range args {
		if i > 0 {
			spent++ // the separator a join writes between two arguments
		}
		if spent >= CmdlinePrefix {
			return args[:i]
		}
		room := CmdlinePrefix - spent
		if len(a) <= room {
			spent += len(a)
			continue
		}
		out := make([]string, i, i+1)
		copy(out, args[:i])
		return append(out, clipUTF8Prefix(a, room))
	}
	return args
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
