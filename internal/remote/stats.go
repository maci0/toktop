package remote

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/gpu"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/sysmon"
)

// Stats samples host vitals from a remote every few seconds and merges them
// into snapshots, tagged with the host so the UI can show origin. Load and
// memory come from /proc and stay empty on non-Linux remotes; CPU model, OS
// name, kernel and uptime have Darwin fallbacks.
type Stats struct {
	Client *Client

	mu   sync.Mutex
	last core.SysSample
	at   time.Time
	// loadsValid records whether the latest dump carried usable load
	// readings. Remotes without /proc/loadavg (macOS, hardened kernels)
	// still poll successfully, and merging their absent loads would zero
	// the local readout every frame.
	loadsValid bool
	// now stamps sample freshness and ages it in Merge. nil means time.Now.
	// A stepped clock keeps the staleness window on the caller's timeline
	// instead of the wall clock's, so a replayed run merges exactly what a
	// live one would.
	now func() time.Time
	// err is the last poll failure, kept while it stays the reason the remote
	// is not answering. A dropped connection would otherwise look like a host
	// with no load, no memory and no GPU.
	err string
	// failedPolls counts consecutive failures, so the audit log records the
	// outage once when it starts and once when it ends instead of one line per
	// poll. A run lasting days would otherwise write a line every few seconds.
	failedPolls int
	// failedSince is when the current run of failures started, so the recovery
	// line can say how long the host was dark.
	failedSince time.Time
}

// SetNow overrides the clock used to stamp and age remote samples. Safe to
// call while polling is under way: instant is read by the poll goroutine under
// s.mu, so the write is taken under the same lock. Call before Run to keep the
// samples it stamps on one timeline.
func (s *Stats) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = fn
}

// instant reads s.now, falling back to the wall clock for a Stats built as a
// literal (tests, and any caller that never called SetNow). Callers hold s.mu.
func (s *Stats) instant() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// sectionMark separates the vitals dump into ordered sections. Chosen to be
// unlikely to appear in any of the read files.
const sectionMark = "%toktop%"

// gpuSection is the position of the GPU section in that dump: loadavg,
// meminfo, uptime, CPU model, OS name, kernel, then the vendor CLI's output.
// Its presence is the script's end-of-run signal, which is how parseVitals
// tells "the remote reported no GPU" from "the dump stopped early".
const gpuSection = 6

// stalenessWindow is how long a remote sample still counts as fresh. Past it
// the overlay is dropped rather than merged: a remote that stopped answering
// must not freeze its last known vitals on screen indefinitely.
const stalenessWindow = 20 * time.Second

// vitalsScript dumps load, memory, uptime, CPU model, OS name, kernel and GPU
// telemetry (NVIDIA via nvidia-smi, AMD via rocm-smi; whichever is present) in
// one round trip, sections separated by sectionMark lines. Load and memory
// come from /proc and degrade to empty on non-Linux remotes; CPU model, OS
// name, kernel and uptime have Darwin fallbacks. Darwin uptime is
// kern.boottime vs date(1): the remote is a shell one-liner, so CLOCK_MONOTONIC
// is not available the way the local Darwin sampler uses it.
func vitalsScript() string {
	return `
cat /proc/loadavg 2>/dev/null
echo ` + sectionMark + `
cat /proc/meminfo 2>/dev/null
echo ` + sectionMark + `
cut -d' ' -f1 /proc/uptime 2>/dev/null
boot=$(sysctl -n kern.boottime 2>/dev/null)
sec=${boot#*sec = }
sec=${sec%%,*}
case $sec in
  ''|*[!0-9]*) ;;
  *) echo $(($(date +%s) - sec));;
esac
echo ` + sectionMark + `
cpu=$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo 2>/dev/null | sed -n 1p)
[ -n "$cpu" ] || cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null)
echo "$cpu"
echo ` + sectionMark + `
os=""
[ -r /etc/os-release ] && . /etc/os-release && os="$PRETTY_NAME"
if [ -z "$os" ] && command -v sw_vers >/dev/null 2>&1; then
  os="macOS $(sw_vers -productVersion 2>/dev/null)"
fi
echo "$os"
echo ` + sectionMark + `
uname -r 2>/dev/null
echo ` + sectionMark + `
if command -v nvidia-smi >/dev/null 2>&1; then
  nvidia-smi --query-gpu=index,name,temperature.gpu,memory.used,memory.total,utilization.gpu,power.draw,driver_version --format=csv,noheader,nounits 2>/dev/null
elif command -v rocm-smi >/dev/null 2>&1; then
  rocm-smi --showtemp --showusemem --showmeminfo vram --showuse --json 2>/dev/null
fi
true`
}

// DefaultPollEvery is the sampling period Run uses when the caller passes a
// non-positive one, and what callers pass when they want that period. A zero
// or negative period would otherwise make time.NewTicker panic.
const DefaultPollEvery = 5 * time.Second

// Run polls until ctx is done or the connection it samples dies: past a drop
// every poll fails, so continuing would only burn a round trip apiece on a
// corpse for the rest of the process.
func (s *Stats) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultPollEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	s.poll(ctx)
	var lost <-chan struct{}
	if s.Client != nil {
		lost = s.Client.Done()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-lost:
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

func (s *Stats) poll(ctx context.Context) {
	if s.Client == nil {
		return
	}
	out, err := s.Client.Run(ctx, vitalsScript())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// Keep the last good sample; the reason rides along so the UI can
		// name the target that stopped answering instead of dropping the
		// ssh readings and leaving local numbers to pass for the remote's.
		// Snippet collapses it to one sanitized line; the home fold is the
		// one the audit log already applies, so a remote whose error names a
		// path under the operator's home does not put that account name into
		// a dashboard or a report the operator is told to paste into issues.
		s.err = core.RedactHome(core.Snippet([]byte(err.Error())))
		s.failedPolls++
		// The UI shows the reason on the frame it happens to be drawn on and
		// the frame is replaced a second later. The audit log gets the start of
		// the outage, the first failure only: every poll after it repeats a
		// reason the recovery line will end.
		if s.failedPolls == 1 {
			s.failedSince = s.instant()
			audit().Warn("toktop: remote vitals poll failed",
				"target", logcfg.RedactedField(s.Client.Target.UserHost(), 256),
				"error", logcfg.RedactedField(err.Error(), 256))
		}
		return
	}
	if s.failedPolls > 0 {
		audit().Info("toktop: remote vitals poll recovered",
			"target", logcfg.RedactedField(s.Client.Target.UserHost(), 256),
			"failed_polls", s.failedPolls,
			"outage", s.instant().Sub(s.failedSince).Round(time.Second))
	}
	s.failedPolls = 0
	s.err = ""
	s.loadsValid = parseVitals(out, &s.last)
	s.last.RemoteHost = s.Client.Target.Host
	s.at = s.instant()
}

// Merge overlays fresh remote stats onto a local sample. Stale data (>20s)
// is not merged, but a recorded poll failure names the target and its reason,
// so the frame says which host is missing rather than passing local readings
// off as the whole picture. A target that has not failed and has no sample
// yet leaves the sample alone. Where several targets overlay one sample, only
// the one holding the failure names the sample (see the switch below).
func (s *Stats) Merge(into *core.SysSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() || core.Age(s.instant(), s.at) > stalenessWindow {
		if s.err != "" {
			into.RemoteHost, into.RemoteErr = s.host(), s.err
		}
		return
	}
	if s.loadsValid {
		into.Load1, into.Load5, into.Load15 = s.last.Load1, s.last.Load5, s.last.Load15
	}
	if s.last.MemTotal > 0 {
		into.MemTotal, into.MemUsed = s.last.MemTotal, s.last.MemUsed
		into.SwapTotal, into.SwapUsed = s.last.SwapTotal, s.last.SwapUsed
	}
	if s.last.CPUModel != "" {
		into.CPUModel = s.last.CPUModel
	}
	if s.last.OsName != "" {
		into.OsName = s.last.OsName
	}
	if s.last.Kernel != "" {
		into.Kernel = s.last.Kernel
	}
	if s.last.HostUptime > 0 {
		into.HostUptime = s.last.HostUptime
	}
	if len(s.last.GPUs) > 0 {
		// Copy: s.last is rewritten on the next poll, and the merged
		// sample is published to the UI goroutine.
		into.GPUs = slices.Clone(s.last.GPUs)
		// Drivers follow the devices they were read from. parseVitals
		// rebuilds the map every poll so a vendor that went away does not
		// linger, and merging it as a union would keep the local host's
		// driver chips beside the remote's devices, or beside none at all
		// when the remote's vendors publish no driver string. A target with
		// no GPUs leaves both fields alone, so the pair stays coherent.
		into.Drivers = maps.Clone(s.last.Drivers)
	}
	// The dump carries no temperatures and no NPUs, so what the local
	// sampler left there is this machine's, not the remote's. It is
	// dropped rather than shown: the strip labels the whole row with the
	// host, and an operator reading a remote's CPU model beside their own
	// box's package temperature has been told a lie.
	into.Temps, into.NPUs = nil, nil
	// RemoteHost labels RemoteErr, so the two have to name one target.
	// --target is repeatable and each target's Merge overlays the same
	// sample, so the pair is a slot with an owner, not a field every target
	// writes: a healthy target that cleared it would take the banner off a
	// different target that is still down and leave a dead host with no
	// symptom at all. A target claims the slot while it is failing and
	// releases it on its own recovery; a healthy target that owns nothing
	// names itself, and one that does not own the slot says nothing.
	switch {
	case s.err != "":
		into.RemoteHost, into.RemoteErr = s.host(), s.err
	case into.RemoteHost == s.host():
		into.RemoteErr = ""
	default:
		if into.RemoteErr == "" {
			into.RemoteHost = s.last.RemoteHost
		}
	}
}

// host is the target label for a sample that has gone stale: the host is
// stamped on a successful poll, so a target that never answered once has none
// to show and its connection error stands alone.
func (s *Stats) host() string {
	if s.last.RemoteHost != "" {
		return s.last.RemoteHost
	}
	if s.Client != nil {
		return s.Client.Target.Host
	}
	return ""
}

// parseVitals reads the vitalsScript dump: loadavg, meminfo, uptime seconds,
// CPU model, OS name, kernel release and GPU telemetry (nvidia-smi CSV or
// rocm-smi JSON), separated by sectionMark lines. Missing sections leave the
// corresponding fields alone.
// It reports whether usable load readings were present; a remote whose
// loadavg is missing or all-zero leaves the local readings in place instead
// of zeroing them. The GPU section, which the script always emits, is the one
// exception: it replaces the previous readings even when it is empty.
func parseVitals(out string, s *core.SysSample) (loadsOK bool) {
	sections := splitSections(out)
	section := func(i int) string {
		if i >= len(sections) {
			return ""
		}
		return sections[i]
	}

	if load := firstLine(section(0)); load != "" {
		if l1, l5, l15 := sysmon.ParseLoadavg(load); l1 > 0 || l5 > 0 || l15 > 0 {
			s.Load1, s.Load5, s.Load15 = l1, l5, l15
			loadsOK = true
		}
	}
	if mem := section(1); strings.TrimSpace(mem) != "" {
		sysmon.ParseMeminfo([]byte(mem), s)
	}
	if up := firstLine(section(2)); up != "" {
		if f := strings.Fields(up); len(f) > 0 {
			s.HostUptime = sysmon.ParseUptimeSecs(f[0])
		}
	}
	if cpu := firstLine(section(3)); cpu != "" {
		s.CPUModel = vitalsField(cpu)
	}
	if osName := trimQuotes(firstLine(section(4))); osName != "" {
		s.OsName = vitalsField(osName)
	}
	if kern := firstLine(section(5)); kern != "" {
		s.Kernel = vitalsField(kern)
	}
	// The GPU section is the one a dump can legitimately deliver empty: the
	// script emits its marker whatever the vendor CLI does, and the CLI
	// answers nothing once the driver is unloaded, the tool is uninstalled,
	// or the machine is a VM. An empty answer therefore replaces what the
	// previous poll recorded instead of leaving the card and its driver on
	// the dashboard for the rest of the run. A section that never arrived
	// (a dump cut short before the last marker) leaves the last reading
	// alone, like every other section.
	if gpuSection < len(sections) {
		devs := parseGPUs(sections[gpuSection])
		s.GPUs = devs
		s.Drivers = nil
		if len(devs) > 0 {
			s.Drivers = make(map[string]string, len(devs))
			for _, dev := range devs {
				if d := dev.Driver; d != "" {
					s.Drivers[dev.Vendor] = d
				}
			}
		}
	}
	return loadsOK
}

// parseGPUs accepts either the nvidia-smi CSV or the rocm-smi JSON flavor of
// the GPU section, whichever the remote produced.
func parseGPUs(section string) []core.GPUDevice {
	if devs := gpu.ParseNvidiaSMI([]byte(section)); len(devs) > 0 {
		return devs
	}
	return gpu.ParseRocmSMI([]byte(strings.TrimSpace(section)))
}

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// splitSections cuts a vitals dump on standalone sectionMark lines. Line-wise
// matching is essential: substring splitting mis-nests consecutive empty
// sections because adjacent markers share their newline.
func splitSections(out string) []string {
	var secs []string
	var cur strings.Builder
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == sectionMark {
			secs = append(secs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	return append(secs, cur.String())
}

// trimQuotes strips one layer of matching quotes (PRETTY_NAME style). The
// local producer of the same field, /etc/os-release's PRETTY_NAME, uses
// strings.Trim(v, `"`) at internal/sysmon/sysmon_linux.go, which is a cutset
// trim: PRETTY_NAME=""" reads locally as no OS name at all and over ssh as a
// one-character name. strconv.Unquote is the library call for this job and
// makes both paths agree, including on a value that carries the escapes a
// real os-release does.
func trimQuotes(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// vitalsFieldMax bounds a host-identity string the remote peer chooses. The
// values come from the peer's /proc/cpuinfo, /etc/os-release and uname -r,
// and firstLine bounds them by nothing but the line the peer sent: a 5 MB
// PRETTY_NAME is a 5 MB field in the snapshot, re-sanitized by every renderer
// on every frame and written whole into --json. ModelNameMax is the same
// bound the rest of the tree puts on a server-chosen string.
const vitalsFieldMax = core.ModelNameMax

// vitalsField is the shape a peer-supplied host-identity string takes in this
// program: one line, terminal-sanitized, and capped at vitalsFieldMax grapheme
// clusters. The cap counts clusters, so an accented letter or an emoji in a
// model name is never cut in half.
func vitalsField(s string) string {
	return core.SingleLine(core.TruncateClusters(s, vitalsFieldMax))
}
