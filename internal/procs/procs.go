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
	"github.com/maci0/toktop/internal/logcfg"
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
	// sweep is non-nil for the duration of the unlocked listing, and closed
	// when it lands. The refresh window alone cannot hold a second sweep off: a
	// Windows CIM enumeration outlasts refreshMin, so every caller arriving past
	// the window started its own sweep of one process table, and whichever
	// finished last published the older listing and a lastSample stamped
	// before a newer one.
	sweep chan struct{}

	// refreshMin throttles expensive OS tooling (PowerShell CIM on Windows
	// takes seconds); within the window the previous snapshot is returned.
	refreshMin time.Duration
	cached     []Info
	// listFailed latches a listing failure so a host whose process table
	// stays unreadable is reported once per outage instead of once per sweep.
	// The cached snapshot is returned in the meantime, so without the latch
	// nothing distinguishes a stale-but-working table from one that stopped
	// answering entirely.
	listFailed bool
}

// audit builds the process logger for the lines this package writes. A var so
// a test can point it at a handler it can read; the only call sites are the
// two listing-failure lines below, both on the throttled sweep, so building
// it per call costs nothing.
var audit = logcfg.Logger

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
	// instead, together with the in-flight channel, so a caller arriving
	// mid-sweep waits for that sweep rather than starting a second one over the
	// same process table, and the tick math below still runs as one critical
	// section. A caller that waited is answered by the sweep it waited for and
	// does not re-decide: re-deciding would sweep again wherever the refresh
	// window is zero, which is every platform but Windows.
	s.mu.Lock()
	if s.sweep != nil {
		// The wait is bounded. A lister blocked in the kernel on a stalled mount
		// cannot be cancelled from here, and an unbounded wait pins every caller
		// of this sampler for as long as it stalls. A caller that gives up reads
		// the cache the sweep has published so far, which is what a caller
		// arriving mid-sweep read before, so the bound costs a stale frame
		// rather than a stalled panel.
		sweep := s.sweep
		s.mu.Unlock()
		timer := time.NewTimer(sweepWait)
		select {
		case <-sweep:
			timer.Stop()
		case <-timer.C:
		}
		s.mu.Lock()
		out := slices.Clone(s.cached)
		s.mu.Unlock()
		return out
	}
	if s.refreshMin > 0 && !s.last.IsZero() && core.Age(now, s.last) < s.refreshMin {
		out := slices.Clone(s.cached)
		s.mu.Unlock()
		return out
	}
	s.last = now
	done := make(chan struct{})
	s.sweep = done
	s.mu.Unlock()
	// Released by defer, not on the normal path: the claim is what every later
	// caller parks on, so a panic inside the platform lister would otherwise
	// pin all of them for the life of the process, with nothing to say why.
	// Registered before the s.mu defer below, so it runs after that unlock and
	// can take the lock itself. The channel is closed with the lock released,
	// so a woken caller never blocks acquiring it.
	defer func() {
		s.mu.Lock()
		s.sweep = nil
		s.mu.Unlock()
		close(done)
	}()

	list, err := platformList()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// Last good snapshot; a transient listing error is not "no
		// processes". With nothing cached yet the returned slice is empty,
		// which renders as a permanently blank process panel. A host whose
		// process table cannot be read at all (no PowerShell on Windows, a
		// /proc mount that is not there) then looks exactly like a host with
		// no engines running, so that case is recorded. The listing is
		// already throttled to one sweep per refreshMin, which bounds this
		// line the same way the sweep itself is bounded.
		if len(s.cached) == 0 {
			audit().Warn("toktop: process listing failed, no snapshot to fall back on",
				"error", logcfg.RedactedField(err.Error(), 256))
		} else if !s.listFailed {
			// A snapshot on hand is why the panel still shows something, and
			// that is exactly why the outage is invisible: every provider
			// discovery port and every process match keeps being answered
			// from the cache, so the engine list goes quietly stale. Latched
			// like the transcript read failures, because the same sweep
			// repeats every refreshMin for as long as the table is gone.
			audit().Warn("toktop: process listing failed; the displayed processes are a stale snapshot",
				"error", logcfg.RedactedField(err.Error(), 256))
		}
		s.listFailed = true
		return slices.Clone(s.cached)
	}
	s.listFailed = false

	// core.Age, like every other interval here: a backward step makes a raw
	// subtraction negative, and the jiffies read in that frame are dropped
	// rather than counted against a shorter interval.
	var dt float64
	if !s.lastSample.IsZero() {
		dt = core.Age(now, s.lastSample).Seconds()
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

// clampPctMax is the ceiling on a derived CPU percentage. Many-core boxes
// legitimately exceed 100% of one core, so the cap is far above it; it exists
// to bound a runaway multiplier, not to describe a real load.
const clampPctMax = 100 * 1024

// clampPct bounds a derived CPU percentage. Counter resets must not read as
// negative load, and a runaway multiplier must stay bounded; many-core boxes
// legitimately exceed 100% of one core.
func clampPct(v float64) float64 {
	if !(v > 0) { // also catches NaN: every comparison with it is false
		return 0
	}
	return min(v, clampPctMax)
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
	match   func(*cmdline) bool
}

// baseName is a process name reduced to the form the engine matchers below
// compare, folded with core.FoldASCII rather than strings.ToLower. A /proc
// name is arbitrary bytes, and the matchers test ASCII literals: ToLower also
// folds runes whose lowercase form is ASCII, so a binary invoked through a
// U+212A (KELVIN SIGN) reaches the matcher as "kvllm" and is claimed as a
// vLLM. It also rewrites an invalid byte to U+FFFD, which is not the name
// anything else on the machine spells it.
func baseName(n string) string {
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	if i := strings.LastIndexByte(n, '\\'); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSuffix(core.FoldASCII(n), ".exe")
}

// anyArgContains reports whether any argument holds one of subs. The
// arguments are read through the cmdline's memoized fold, which draws on the
// matchJoinBytes budget, so the matchers read no further into a command line
// than CmdlinePrefix allows. Clipping per argument matters on its own: a
// Chrome --disable-features blob is tens of kilobytes and never an engine
// module path.
func (c *cmdline) anyArgContains(subs ...string) bool {
	for _, la := range c.argsFolded() {
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
// The kept arguments are cloned rather than shared. A local listing builds
// them with strings.Split over one whole /proc/PID/cmdline buffer and a sweep
// with strings.Fields over one whole ps line, so every element is a window
// onto the same bytes: returning a subslice, however short, would pin the
// entire command line for the life of the sampler, which is the retention the
// bound exists to prevent.
//
// Exported for the sweep's own reader, which splits a line that was cut in
// characters, so the Info it builds is bounded in bytes the same way.
func ClipArgs(args []string) []string {
	kept := make([]string, 0, len(args))
	spent := 0
	for i, a := range args {
		if i > 0 {
			spent++ // the separator a join writes between two arguments
		}
		if spent >= CmdlinePrefix {
			break
		}
		room := CmdlinePrefix - spent
		if len(a) > room {
			kept = append(kept, strings.Clone(clipUTF8Prefix(a, room)))
			break
		}
		spent += len(a)
		kept = append(kept, strings.Clone(a))
	}
	return kept
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
		b.WriteString(core.FoldASCII(a))
	}
	return b.String()
}

// cmdline is what an engine matcher reads: the process name, its argv, and
// the argv folded into one lowercased line.
//
// The folds are built on first use. Most matchers decide on the name alone,
// and folding the line for every process on every /proc poll was a per-process
// allocation paid for a match that never reads it. The per-argument fold is
// separate from the joined one and memoized the same way: four matchers test
// arguments by substring, and folding the same argv once per matcher was four
// times the fold for one answer.
type cmdline struct {
	name       string
	args       []string
	once       sync.Once
	folded     string
	argsOnce   sync.Once
	foldedArgs []string
}

// joined is the lowercased command line, folded once per cmdline.
func (c *cmdline) joined() string {
	c.once.Do(func() { c.folded = lowerJoinedArgs(c.args) })
	return c.folded
}

// argsFolded is the command line's arguments lowercased, folded once per
// cmdline and drawn on the same matchJoinBytes budget anyArgContains spends,
// so the matchers read no further into a command line than CmdlinePrefix
// allows whichever of them runs first.
func (c *cmdline) argsFolded() []string {
	c.argsOnce.Do(func() {
		budget := matchJoinBytes
		out := make([]string, 0, len(c.args))
		for _, a := range c.args {
			if budget <= 0 {
				break
			}
			la := core.FoldASCII(clipUTF8Prefix(a, budget))
			budget -= len(la)
			out = append(out, la)
		}
		c.foldedArgs = out
	})
	return c.foldedArgs
}

// has reports whether the folded command line contains s.
func (c *cmdline) has(s string) bool { return strings.Contains(c.joined(), s) }

var engineMatchers = []engineMatcher{
	{"ollama", 11434, func(c *cmdline) bool {
		return c.name == "ollama" || c.has("ollama serve")
	}},
	{"llama.cpp", 8080, func(c *cmdline) bool {
		return strings.Contains(c.name, "llama-server") || c.has("llama-server") ||
			strings.Contains(c.name, "llamafile")
	}},
	{"koboldcpp", 5001, func(c *cmdline) bool {
		return c.has("koboldcpp")
	}},
	{"vllm", 8000, func(c *cmdline) bool {
		return c.anyArgContains("vllm.entrypoints", "/vllm") ||
			baseNameEq(c.args, "vllm")
	}},
	{"sglang", 30000, func(c *cmdline) bool {
		// python -m sglang.launch_server / sglang.srt.*, and the
		// `sglang serve` CLI (same shape as `vllm serve`).
		return c.anyArgContains("sglang.launch_server", "sglang.srt") ||
			baseNameEq(c.args, "sglang")
	}},
	{"triton", 8000, func(c *cmdline) bool { return c.name == "tritonserver" }},
	{"tgi", 8080, func(c *cmdline) bool {
		return c.has("text-generation-launcher")
	}},
	{"tabbyapi", 5000, func(c *cmdline) bool { return c.name == "tabbyapi" }},
	{"oobabooga", 7860, func(c *cmdline) bool {
		return c.has("text-generation-webui") || c.has("oobabooga")
	}},
	{"localai", 8080, func(c *cmdline) bool {
		return c.name == "localai" || c.name == "local-ai"
	}},
	{"litellm", 4000, func(c *cmdline) bool {
		return baseNameEq(c.args, "litellm") || c.anyArgContains("litellm.proxy")
	}},
	{"mlx", 8080, func(c *cmdline) bool {
		return c.has("mlx_lm.server") || c.has("mlx-lm")
	}},
	{"lmstudio", 1234, func(c *cmdline) bool {
		return strings.Contains(c.name, "lm-studio") || strings.Contains(c.name, "lmstudio") ||
			strings.Contains(c.name, "lm studio")
	}},
	{"gpustack", 80, func(c *cmdline) bool {
		return c.anyArgContains("gpustack.start")
	}},
	{"lemonade", 8000, func(c *cmdline) bool { return c.name == "lemonade-server" || c.name == "lemond" }},
	{"gpt4all", 4891, func(c *cmdline) bool { return c.name == "gpt4all" }},
	{"jan", 1337, func(c *cmdline) bool { return c.name == "jan" }},
	{"ramalama", 8080, func(c *cmdline) bool {
		return c.has("ramalama")
	}},
}

func baseNameEq(args []string, want string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return baseName(a) == want })
}

// MatchEngine finds the well-known engine behind a process, if any. The
// name is basename'd internally, so raw argv[0] works. Exported for the
// remote ssh path, which matches command lines gathered from another host.
func MatchEngine(i Info) (engine string, defPort int, ok bool) {
	c := cmdline{name: baseName(i.Name), args: i.Args}
	for _, m := range engineMatchers {
		if m.match(&c) {
			return m.engine, m.defPort, true
		}
	}
	return "", 0, false
}

// defaultSamplerRefresh is set by platform files when OS tooling needs
// throttling (windows). Zero means every Snapshot call re-lists.
var defaultSamplerRefresh time.Duration

// sweepWait bounds how long a caller parks on another caller's in-flight
// listing. Long enough that a normal Windows CIM sweep is waited out rather
// than duplicated, short enough that a lister wedged in the kernel does not
// pin the panel.
const sweepWait = 2 * time.Second
