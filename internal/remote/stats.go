package remote

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/gpu"
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
}

// sectionMark separates the vitals dump into ordered sections. Chosen to be
// unlikely to appear in any of the read files.
const sectionMark = "%toktop%"

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

// Run polls until ctx is done or the connection it samples dies: past a drop
// every poll fails, so continuing would only burn a round trip apiece on a
// corpse for the rest of the process.
func (s *Stats) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
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
	if err != nil {
		return // keep last good sample; UI shows staleness via age
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadsValid = parseVitals(out, &s.last)
	s.last.RemoteHost = s.Client.Target.Host
	s.at = time.Now()
}

// Merge overlays fresh remote stats onto a local sample. Stale data (>20s)
// is ignored entirely.
func (s *Stats) Merge(into *core.SysSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() || time.Since(s.at) > 20*time.Second {
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
	}
	if len(s.last.Drivers) > 0 {
		if into.Drivers == nil {
			into.Drivers = maps.Clone(s.last.Drivers)
		} else {
			maps.Copy(into.Drivers, s.last.Drivers)
		}
	}
	into.RemoteHost = s.last.RemoteHost
}

// parseVitals reads the vitalsScript dump: loadavg, meminfo, uptime seconds,
// CPU model, OS name, kernel release and GPU telemetry (nvidia-smi CSV or
// rocm-smi JSON), separated by sectionMark lines. Missing sections leave the
// corresponding fields alone.
// It reports whether usable load readings were present; a remote whose
// loadavg is missing or all-zero leaves the local readings in place instead
// of zeroing them.
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
		s.CPUModel = cpu
	}
	if osName := trimQuotes(firstLine(section(4))); osName != "" {
		s.OsName = osName
	}
	if kern := firstLine(section(5)); kern != "" {
		s.Kernel = kern
	}
	if devs := parseGPUs(section(6)); len(devs) > 0 {
		s.GPUs = devs
		if s.Drivers == nil {
			s.Drivers = map[string]string{}
		}
		if d := devs[0].Driver; d != "" {
			s.Drivers[devs[0].Vendor] = d
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

// firstLine returns the first non-blank line of s.
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

// trimQuotes strips one layer of matching quotes (PRETTY_NAME style).
func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
