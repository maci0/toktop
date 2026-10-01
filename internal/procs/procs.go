// Package procs discovers local inference engines by inspecting running
// processes rather than guessing ports. Everything comes from procfs,
// ps(1) or Win32 CIM - no vendor libraries.
package procs

import (
	"bytes"
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
	annotateClipped(r, ClipArgs(r.args), nil)
}

// annotateClipped is annotate for a lister that has already clipped the
// command line (see splitCmdline), so the line is not split and clipped
// twice on its way to the same two derivations. scratch, when the lister has
// one, carries the fold buffers across processes; a lister that has none
// (one call, not a sweep) folds into fresh buffers.
func annotateClipped(r *raw, clipped []string, scratch *foldScratch) {
	r.args = clipped
	r.port = ExtractPort(r.args)
	if eng, defPort, ok := matchEngine(scratch, Info{Name: r.name, Args: r.args}); ok {
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

// clkTck is the jiffies-per-second constant the CPU delta math divides by.
// USER_HZ is fixed at 100 by the Linux ABI; there is no runtime probe, and
// the windows path scales its 100ns units into the same jiffies.
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

// packageSampler is the process-wide engine sampler Discovery lists through.
// The collector keeps its own, because it lists on its own clock. OnceValue
// so NewSampler runs after platform init has set defaultSamplerRefresh.
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
				"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
		} else if !s.listFailed {
			// A snapshot on hand is why the panel still shows something, and
			// that is exactly why the outage is invisible: every provider
			// discovery port and every process match keeps being answered
			// from the cache, so the engine list goes quietly stale. Latched
			// like the transcript read failures, because the same sweep
			// repeats every refreshMin for as long as the table is gone.
			audit().Warn("toktop: process listing failed; the displayed processes are a stale snapshot",
				"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
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
		// One probe of the tick ledger per process, read before anything here
		// writes it: the derived percentage below and the baseline update under
		// it are the same pid's two questions, asked once.
		pticks, tracked := s.prev[r.pid]
		switch {
		case r.cpuPercent > 0:
			info.CPUPct = r.cpuPercent
		case dt > 0:
			if tracked && r.ticks >= pticks {
				info.CPUPct = clampPct(float64(r.ticks-pticks) / clkTck / dt * 100)
			}
		}
		if !tracked || r.ticks != 0 {
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
	return strings.TrimSuffix(core.FoldASCII(n), exeSuffix)
}

// exeSuffix is what baseName strips from a Windows executable name, named so
// baseNameEqual can recognise the same suffix without a folded copy to hold it.
const exeSuffix = ".exe"

// anyArgContains reports whether any argument holds one of subs. The
// arguments are read through the cmdline's memoized fold, which draws on the
// matchJoinBytes budget, so the matchers read no further into a command line
// than CmdlinePrefix allows. Clipping per argument matters on its own: a
// Chrome --disable-features blob is tens of kilobytes and never an engine
// module path.
func (c *cmdline) anyArgContains(subs ...[]byte) bool {
	for _, la := range c.argsFolded() {
		for _, sub := range subs {
			if bytes.Contains(la, sub) {
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

// splitCmdline is ClipArgs over a command line in the NUL-separated form
// /proc/PID/cmdline holds, producing the same arguments it would and bounding
// them the same way. It exists for the /proc walk, which reads every process
// on the host on every poll: splitting each of those command lines allocated
// a string header per argument for the whole host, and ClipArgs then
// allocated a second slice sized by that argument count to keep the handful
// of bytes the bound allows. A browser or an Electron app carries thousands
// of arguments and is dropped again a few lines later, so the walk built
// two large slices per process per poll to keep none of them. Here the
// arguments are windows onto one copy of the line, cut where the budget runs
// out rather than one at a time, and the walk's scratch carries the headers.
//
// line must already be trimmed of the trailing NUL a command line ends with.
// The strings appended to dst alias it and are only valid while it is; a
// lister that keeps them calls cloneArgs first, which is the detachment
// ClipArgs does as it goes.
func splitCmdline(line string, dst []string) []string {
	dst = dst[:0]
	spent := 0
	for i := 0; line != ""; i++ {
		var arg string
		if n := strings.IndexByte(line, 0); n >= 0 {
			arg, line = line[:n], line[n+1:]
		} else {
			arg, line = line, ""
		}
		if i > 0 {
			spent++ // the separator a join writes between two arguments
		}
		if spent >= CmdlinePrefix {
			break
		}
		room := CmdlinePrefix - spent
		if len(arg) > room {
			dst = append(dst, clipUTF8Prefix(arg, room))
			break
		}
		spent += len(arg)
		dst = append(dst, arg)
	}
	return dst
}

// cloneArgs detaches a command line split out of a read buffer from that
// buffer, so a retained listing pins the arguments it keeps and not the whole
// /proc/PID/cmdline read they were windows onto.
func cloneArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.Clone(a)
	}
	return out
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
	return string(appendJoined(nil, args))
}

// appendJoined writes the lowercased, budget-capped join of args into dst and
// returns the grown slice. Callers that fold one command line per process on a
// /proc poll pass the previous result back in as dst, so the join reuses one
// buffer for the whole sweep instead of allocating a fresh multi-kilobyte
// string per process. The returned bytes are only valid until the next call
// with the same dst, which is why nothing here retains them: the matchers read
// them for a substring test and the engine name they return is a constant.
func appendJoined(dst []byte, args []string) []byte {
	b := dst[:0]
	n := min(len(args), matchJoinArgs)
	for i := 0; i < n; i++ {
		if len(b) >= matchJoinBytes {
			break
		}
		if i > 0 {
			b = append(b, ' ')
		}
		a := args[i]
		remain := matchJoinBytes - len(b)
		if len(a) > remain {
			a = clipUTF8Prefix(a, remain)
		}
		b = appendFoldASCII(b, a)
	}
	return b
}

// appendFoldASCII appends s to dst with its ASCII A-Z folded to a-z, without
// the intermediate []byte/string pair core.FoldASCII allocates for a string
// that does have uppercase in it. The fold is byte-wise and never touches a
// byte past an ASCII letter, so a multi-byte UTF-8 sequence is copied whole,
// exactly as FoldASCII leaves it.
func appendFoldASCII(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
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
//
// buf is scratch the folds are built in, owned by the sweep rather than by the
// cmdline: a listing folds one command line per process per poll, and a walk
// that allocated its own buffer per process created (and collected) a
// multi-kilobyte string for every process on the host, every interval, only to
// answer a substring test and drop it. One buffer per sweep serves all of
// them. Its contents are only valid until the next cmdline is folded, which
// nothing retains: joined and argsFolded are read for a match, and the engine
// a matcher returns is a constant.
type cmdline struct {
	name       string
	args       []string
	once       sync.Once
	folded     []byte
	argsOnce   sync.Once
	foldedArgs [][]byte
	heads      [][]byte
	buf        []byte
	argBuf     []byte
}

// joined is the lowercased command line, folded once per cmdline. The grown
// buffer is kept on the cmdline: the scratch hands the capacity back only
// what this fold left on the line, and the line outlives it, so a fold that
// grew the buffer (a long argv, the first process of a sweep) would otherwise
// have to grow it again for the next one.
func (c *cmdline) joined() []byte {
	c.once.Do(func() {
		c.folded = appendJoined(c.buf, c.args)
		c.buf = c.folded
	})
	return c.folded
}

// argsFolded is the command line's arguments lowercased, folded once per
// cmdline and drawn on the same matchJoinBytes budget anyArgContains spends,
// so the matchers read no further into a command line than CmdlinePrefix
// allows whichever of them runs first. The separator between two arguments
// costs a byte of that budget here as it does in lowerJoinedArgs and in
// ClipArgs: a budget one copy ignores is bytes a matcher reads past the
// retained prefix the bound exists to keep.
//
// The arguments are windows onto one folded copy of the line in argBuf, which
// the sweep reuses. Copying each fold out separately (what core.FoldASCII
// returns) allocated a string per argument holding uppercase, once per process
// per poll. argBuf and the joined line's buffer are kept apart because the two
// reads may be taken in either order and a name matcher can read both, but each
// is written once and reused for the whole sweep.
func (c *cmdline) argsFolded() [][]byte {
	c.argsOnce.Do(func() {
		budget := matchJoinBytes
		b := c.argBuf[:0]
		out := c.heads[:0]
		for i, a := range c.args {
			if i > 0 {
				budget--
			}
			if budget <= 0 {
				break
			}
			a = clipUTF8Prefix(a, budget)
			start := len(b)
			b = appendFoldASCII(b, a)
			budget -= len(b) - start
			out = append(out, b[start:])
		}
		c.argBuf = b
		c.heads = out
		c.foldedArgs = out
	})
	return c.foldedArgs
}

// has reports whether the folded command line contains s, which must already
// be in the folded (lowercase) spelling the line is folded into. The needles
// are package-level []byte rather than string literals so the search does not
// convert one per matcher per process: this runs for every process on the host
// on every /proc poll, and a []byte(s) conversion would allocate for the whole
// sweep for a constant.
func (c *cmdline) has(s []byte) bool { return bytes.Contains(c.joined(), s) }

var engineMatchers = []engineMatcher{
	{"ollama", 11434, func(c *cmdline) bool {
		return c.name == "ollama" || c.has(needOllamaServe)
	}},
	{"llama.cpp", 8080, func(c *cmdline) bool {
		return strings.Contains(c.name, "llama-server") || c.has(needLlamaServer) ||
			strings.Contains(c.name, "llamafile")
	}},
	{"koboldcpp", 5001, func(c *cmdline) bool {
		return c.has(needKoboldcpp)
	}},
	{"vllm", 8000, func(c *cmdline) bool {
		return c.anyArgContains(needVllmEntrypoints, needVllmSlash) ||
			baseNameEq(c.args, "vllm")
	}},
	{"sglang", 30000, func(c *cmdline) bool {
		// python -m sglang.launch_server / sglang.srt.*, and the
		// `sglang serve` CLI (same shape as `vllm serve`).
		return c.anyArgContains(needSglangLaunch, needSglangSrt) ||
			baseNameEq(c.args, "sglang")
	}},
	{"triton", 8000, func(c *cmdline) bool { return c.name == "tritonserver" }},
	{"tgi", 8080, func(c *cmdline) bool {
		return c.has(needTextGenLauncher)
	}},
	{"tabbyapi", 5000, func(c *cmdline) bool { return c.name == "tabbyapi" }},
	{"oobabooga", 7860, func(c *cmdline) bool {
		return c.has(needTextGenWebui) || c.has(needOobabooga)
	}},
	{"localai", 8080, func(c *cmdline) bool {
		return c.name == "localai" || c.name == "local-ai"
	}},
	{"litellm", 4000, func(c *cmdline) bool {
		return baseNameEq(c.args, "litellm") || c.anyArgContains(needLiteLLMProxy)
	}},
	{"mlx", 8080, func(c *cmdline) bool {
		return c.has(needMlxServer) || c.has(needMlxDash)
	}},
	{"lmstudio", 1234, func(c *cmdline) bool {
		return strings.Contains(c.name, "lm-studio") || strings.Contains(c.name, "lmstudio") ||
			strings.Contains(c.name, "lm studio")
	}},
	{"gpustack", 80, func(c *cmdline) bool {
		return c.anyArgContains(needGpustackStart)
	}},
	{"lemonade", 8000, func(c *cmdline) bool { return c.name == "lemonade-server" || c.name == "lemond" }},
	{"gpt4all", 4891, func(c *cmdline) bool { return c.name == "gpt4all" }},
	{"jan", 1337, func(c *cmdline) bool { return c.name == "jan" }},
	{"ramalama", 8080, func(c *cmdline) bool {
		return c.has(needRamalama)
	}},
}

// The substrings the name-line matchers search the folded command line for,
// as bytes so the search needs no conversion. Each is written in the folded
// spelling the line carries (lowercase), which is what has folds it into.
var (
	needOllamaServe     = []byte("ollama serve")
	needLlamaServer     = []byte("llama-server")
	needKoboldcpp       = []byte("koboldcpp")
	needTextGenLauncher = []byte("text-generation-launcher")
	needTextGenWebui    = []byte("text-generation-webui")
	needOobabooga       = []byte("oobabooga")
	needMlxServer       = []byte("mlx_lm.server")
	needMlxDash         = []byte("mlx-lm")
	needRamalama        = []byte("ramalama")
	needVllmEntrypoints = []byte("vllm.entrypoints")
	needVllmSlash       = []byte("/vllm")
	needSglangLaunch    = []byte("sglang.launch_server")
	needSglangSrt       = []byte("sglang.srt")
	needLiteLLMProxy    = []byte("litellm.proxy")
	needGpustackStart   = []byte("gpustack.start")
)

// baseNameEq reports whether any argument is, by base name, want (which every
// caller passes already folded). It compares without building the folded name.
//
// Three name-only matchers call this, and each folded the base name of every
// argument on the host to do it: a name containing uppercase costs a copy of
// it, so an argv like "-m vllm.entrypoints.openai.api_server" was copied once
// per matcher per process on every /proc poll, for a comparison that folding
// does not change. baseNameEqual folds a byte at a time against want instead,
// so no name is copied at all.
func baseNameEq(args []string, want string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return baseNameEqual(a, want) })
}

// baseNameEqual is baseName(a) == want for a want already folded, without the
// allocation. The name is reduced to its base first, and the ".exe" suffix
// TrimSuffix would drop is recognised in either case, because baseName folds
// the name before it strips the suffix and so drops it off "LITELLM.EXE" as
// readily as off "litellm.exe". What is compared is therefore precisely what
// baseName produces, and not what strings.ToLower would have over-folded (see
// baseName).
func baseNameEqual(a, want string) bool {
	if i := strings.LastIndexByte(a, '/'); i >= 0 {
		a = a[i+1:]
	}
	if i := strings.LastIndexByte(a, '\\'); i >= 0 {
		a = a[i+1:]
	}
	if n := len(a) - len(exeSuffix); n >= 0 && equalFoldedASCII(a[n:], exeSuffix) {
		a = a[:n]
	}
	if len(a) != len(want) {
		return false
	}
	return equalFoldedASCII(a, want)
}

// equalFoldedASCII reports whether a folded to b's spelling, which must be
// the name baseName folds to. It is FoldASCII's fold (ASCII A-Z only, and not
// ToLower's, so it never rewrites a multi-byte rune) without the copy that
// makes one. b carries no uppercase of its own, so a is the only side that
// ever needs folding.
func equalFoldedASCII(a, b string) bool {
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}

// MatchEngine finds the well-known engine behind a process, if any. The
// name is basename'd internally, so raw argv[0] works. Exported for the
// remote ssh path, which matches command lines gathered from another host.
func MatchEngine(i Info) (engine string, defPort int, ok bool) {
	return matchEngine(nil, i)
}

// matchEngine is MatchEngine over a reusable fold buffer, which a lister that
// folds one command line per process per poll passes so the folds reuse its
// capacity instead of allocating per process. The engine name and port it
// returns are constants from engineMatchers, so nothing about the match
// depends on the buffer outliving the call.
func matchEngine(scratch *foldScratch, i Info) (engine string, defPort int, ok bool) {
	if scratch == nil {
		c := cmdline{name: baseName(i.Name), args: i.Args}
		return runMatchers(&c)
	}
	// The cmdline itself is part of the scratch. Its address reaches the
	// matcher closures, which is enough for the compiler to give it the heap,
	// so a sweep that allocated one per process paid that once per process on
	// the host on every poll for a struct it overwrites immediately.
	c := &scratch.cmd
	c.name, c.args = baseName(i.Name), i.Args
	c.buf, c.argBuf, c.heads = scratch.buf, scratch.argBuf, scratch.heads
	// A new process is a new command line, so the previous process's folds
	// must not answer for it: reset the memos rather than let a warm cmdline
	// report the last one.
	c.once, c.argsOnce, c.folded, c.foldedArgs = sync.Once{}, sync.Once{}, nil, nil
	engine, defPort, ok = runMatchers(c)
	// Keep whatever capacity the folds grew to for the next process, and drop
	// their contents: the bytes the matchers read are windows onto them and
	// are not read again.
	scratch.buf, scratch.argBuf, scratch.heads = c.buf[:0], c.argBuf[:0], c.heads[:0]
	return engine, defPort, ok
}

// runMatchers walks the matchers against one command line. The first that
// claims it names the engine.
func runMatchers(c *cmdline) (engine string, defPort int, ok bool) {
	for _, m := range engineMatchers {
		if m.match(c) {
			return m.engine, m.defPort, true
		}
	}
	return "", 0, false
}

// foldScratch holds everything one lister reuses across the processes it
// matches: the fold buffers, the slice of argument windows onto them, and the
// cmdline itself. It is emptied at the end of every match: the buffers keep
// their capacity and nothing outside a match reads their contents.
type foldScratch struct {
	cmd         cmdline
	buf, argBuf []byte
	heads       [][]byte
}

// defaultSamplerRefresh is set by platform files when OS tooling needs
// throttling (windows). Zero means every Snapshot call re-lists.
var defaultSamplerRefresh time.Duration

// sweepWait bounds how long a caller parks on another caller's in-flight
// listing. It is shorter than a Windows CIM sweep, which the sweep comment
// above says outlasts the refresh window, so this gives up on a peer that
// slow rather than duplicating its work; a lister wedged in the kernel is
// released rather than left pinning the panel.
const sweepWait = 2 * time.Second

// procText decodes one process-listing file into text at the boundary that
// reads it. None of those files is required to hold valid UTF-8: the comm file
// is fixed at TASK_COMM_LEN-1 (15) bytes, so the kernel cuts a name written in
// a non-ASCII script mid-rune (an executable whose name is 21 bytes lands on
// half a character), the command-line file holds whatever bytes the process's
// argv did, and a ps(1) command column carries whatever the process wrote as
// its first argument.
//
// An ill-formed byte cannot be left in place. core.FoldASCII only touches
// ASCII, and a matcher or a label renders the name as it stands, so the half a
// rune reaches the engine label, the JSON report and the matcher key as a
// character no other reader of the same process can produce. Dropping the byte
// is the safe direction: a name that lost a byte matches no engine, which is
// the right answer for a process this sweep cannot name.
//
// Valid input takes the plain conversion, so a sweep over every process on the
// host pays only for the strings it was already turning into text.
func procText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "")
}
