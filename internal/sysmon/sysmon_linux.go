//go:build linux

package sysmon

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

const (
	procMeminfo = "/proc/meminfo"
	procLoadavg = "/proc/loadavg"
	procUptime  = "/proc/uptime"
	sysHwmon    = "/sys/class/hwmon"
	sysThermal  = "/sys/class/thermal"
)

// audit builds the logger for the host-vitals lines. A var so a test can point
// it at a handler it can read.
var audit = logcfg.Logger

// procRun tracks whether a required procfs read is failing, so the audit log
// records the start of an outage once and its end once rather than a line per
// poll. Sample runs every interval, and a sandbox with no procfs would
// otherwise write one line per second for the life of the run.
//
// An entry is never removed, for the reason gpu's run state is not: the keys
// are the fixed file set readProc is called with, and dropping one on recovery
// loses a failure a concurrent poll had just recorded, so the next failure
// reads as a fresh outage.
var procRuns sync.Map // path -> *procRun

type procRun struct {
	mu     sync.Mutex
	failed bool
	since  time.Time
}

func noteProcFailure(path string, err error) {
	s, _ := procRuns.LoadOrStore(path, &procRun{})
	p := s.(*procRun)
	p.mu.Lock()
	first := !p.failed
	if first {
		p.failed, p.since = true, time.Now()
	}
	p.mu.Unlock()
	if !first {
		return
	}
	audit().Warn("toktop: host vitals source unreadable",
		"source", logcfg.Field(path, 256),
		"error", logcfg.Field(err.Error(), 256))
}

func noteProcOK(path string) {
	s, ok := procRuns.Load(path)
	if !ok {
		return
	}
	p := s.(*procRun)
	p.mu.Lock()
	if !p.failed {
		p.mu.Unlock()
		return
	}
	p.failed = false
	downFor := time.Since(p.since)
	p.mu.Unlock()
	audit().Info("toktop: host vitals source readable again",
		"source", logcfg.Field(path, 256),
		"down_for", downFor.Round(time.Second))
}

// readProc reads one of the /proc files every Linux host has, and latches a
// read failure into the audit log.
//
// These three are not optional the way the driver and sensor files are: a
// container without procfs mounted, a hardened kernel, a revoked permission
// all fail the same way an empty file would, and the host strip then shows
// zero memory, no load and no uptime for the rest of the run. An idle machine
// reads the same on screen, so the line naming the file is the only thing that
// tells the two apart.
func readProc(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		noteProcFailure(path, err)
		return nil, false
	}
	noteProcOK(path)
	return b, true
}

func init() {
	platformMemory = sampleMemoryLinux
	platformLoad = func(s *core.SysSample) {
		if b, ok := readProc(procLoadavg); ok {
			s.Load1, s.Load5, s.Load15 = ParseLoadavg(string(b))
		}
	}
	platformTemps = func() []core.TempReading { return scanTemps(sysHwmon, sysThermal) }
	platformCPUModel = cpuModelCached // brand string is fixed once read
	platformHost = hostInfoLinux
}

// cpuModelRetry spaces out retries of an empty brand string. /proc/cpuinfo
// can be hundreds of kilobytes on many-core hosts, so a successful parse is
// kept for the process lifetime; a miss (procfs not ready yet) is retried
// the way hostStatic retries empty drivers rather than blanking the row
// forever.
const cpuModelRetry = 30 * time.Second

var (
	cpuModelMu    sync.Mutex
	cpuModelVal   string
	cpuModelAt    time.Time
	cpuModelProbe = cpuModelLinux
)

func cpuModelCached() string {
	cpuModelMu.Lock()
	defer cpuModelMu.Unlock()
	if cpuModelVal != "" {
		return cpuModelVal
	}
	if !cpuModelAt.IsZero() && time.Since(cpuModelAt) < cpuModelRetry {
		return cpuModelVal
	}
	cpuModelVal = cpuModelProbe()
	cpuModelAt = time.Now()
	return cpuModelVal
}

func sampleMemoryLinux(s *core.SysSample) {
	if b, ok := readProc(procMeminfo); ok {
		ParseMeminfo(b, s)
	}
}

// hostStatic is identity that is usually fixed for the process lifetime:
// distro name, kernel release, driver versions and the NPU inventory.
// Filled fields are kept; empty optional ones (driver not loaded yet, NPU
// sysfs not mounted at start) are retried on hostStaticRetry, hostStaticTries
// times over, so the first sample cannot blank them for the whole session and
// a host with no optional accelerator to find does not rescan forever.
type hostStatic struct {
	osName    string
	kernel    string
	nvidiaDrv string
	cuda      string
	amdgpu    string
	npus      []string
}

const (
	hostStaticRetry = 30 * time.Second
	// hostStaticTries bounds how many times the probe runs, the first read
	// included. The retry exists so a field that is not there yet (a driver
	// loaded after
	// toktop started, NPU sysfs mounted later) is still picked up, and
	// mergeHostStatic only ever fills an empty field, so a retry can still
	// add something. It is bounded because the alternative is asking whether
	// the host has every optional accelerator, which no host without an NPU
	// and without AMD satisfies: that predicate never holds, so the memo
	// re-read /etc/os-release, uname and the driver files every
	// hostStaticRetry for the life of the process. A driver still absent
	// after the window is not one this dashboard keeps waiting on.
	hostStaticTries = 4
)

var (
	hostStaticMu   sync.Mutex
	hostStaticVal  hostStatic
	hostStaticAt   time.Time
	hostStaticFill int
)

func hostStaticInfo() hostStatic {
	hostStaticMu.Lock()
	defer hostStaticMu.Unlock()
	if hostStaticAt.IsZero() || (hostStaticFill < hostStaticTries && time.Since(hostStaticAt) >= hostStaticRetry) {
		hostStaticVal = mergeHostStatic(hostStaticVal, loadHostStatic())
		hostStaticAt = time.Now()
		hostStaticFill++
	}
	res := hostStaticVal
	res.npus = slices.Clone(hostStaticVal.npus)
	return res
}

func mergeHostStatic(prev, fresh hostStatic) hostStatic {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&prev.osName, fresh.osName)
	fill(&prev.kernel, fresh.kernel)
	fill(&prev.nvidiaDrv, fresh.nvidiaDrv)
	fill(&prev.cuda, fresh.cuda)
	fill(&prev.amdgpu, fresh.amdgpu)
	if len(prev.npus) == 0 {
		prev.npus = fresh.npus
	}
	return prev
}

// loadHostStatic is the probe used by hostStaticInfo; tests swap it.
var loadHostStatic = readHostStatic

func readHostStatic() hostStatic {
	var h hostStatic
	h.osName = prettyOSName()
	var un unix.Utsname
	if err := unix.Uname(&un); err == nil {
		h.kernel = strings.TrimSpace(utsField(un.Release[:]))
		if h.osName == "" {
			h.osName = strings.TrimSpace(utsField(un.Sysname[:]))
		}
	}
	if b, err := os.ReadFile("/proc/driver/nvidia/version"); err == nil {
		h.nvidiaDrv, h.cuda = parseNvidiaVersion(string(b))
	}
	h.amdgpu = sysModuleVersion("amdgpu")
	h.npus = scanAccelDrivers("/sys/class/accel")
	return h
}

func hostInfoLinux(s *core.SysSample) {
	h := hostStaticInfo()
	s.OsName = h.osName
	s.Kernel = h.kernel
	s.HostUptime = linuxUptime()
	if s.Drivers == nil { // Sample initializes late; never write a nil map
		s.Drivers = map[string]string{}
	}
	if h.nvidiaDrv != "" {
		s.Drivers["nvidia"] = h.nvidiaDrv
	}
	if h.cuda != "" {
		s.Drivers["cuda"] = h.cuda
	}
	if h.amdgpu != "" {
		s.Drivers["amdgpu"] = h.amdgpu
	}
	// h.npus is hostStaticInfo's own per-call copy: the cache hands out a
	// clone and keeps the original, and the slice never changes once filled.
	s.NPUs = h.npus
}

// prettyOSName reads PRETTY_NAME from /etc/os-release. The value is unquoted
// with strconv.Unquote, not strings.Trim(v, `"`): a cutset trim eats every
// leading and trailing quote, so PRETTY_NAME=""" reads as no name at all. The
// remote reader of the same file (internal/remote/stats.go trimQuotes) uses
// the same call, so one file answers the same both ways.
func prettyOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == "PRETTY_NAME" {
			v = strings.TrimSpace(v)
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return v
		}
	}
	return ""
}

func linuxUptime() time.Duration {
	b, ok := readProc(procUptime)
	if !ok {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	return ParseUptimeSecs(f[0])
}

// parseNvidiaVersion extracts "Driver Version: 550.54.14" and
// "CUDA Version: 12.4" from /proc/driver/nvidia/version. The two lines are
// read independently: the file is the kernel driver's, not ours, and it puts
// CUDA either before or after the driver line depending on the driver build.
// Gating the CUDA scan on the driver line already seen would drop CUDA
// whenever it comes first, and report a driver with a silently missing CUDA
// row.
func parseNvidiaVersion(text string) (driver, cuda string) {
	first := func(line, prefix string) string {
		if _, after, ok := strings.Cut(line, prefix); ok {
			if fields := strings.Fields(after); len(fields) > 0 {
				return fields[0]
			}
		}
		return ""
	}
	for line := range strings.SplitSeq(text, "\n") {
		if v := first(line, "Driver Version:"); v != "" {
			driver = v
		}
		if v := first(line, "CUDA Version:"); v != "" {
			cuda = v
		}
	}
	if driver == "" {
		return "", ""
	}
	return driver, cuda
}

func sysModuleVersion(module string) string {
	b, err := os.ReadFile(filepath.Join("/sys/module", module, "version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scanAccelDrivers enumerates the accelerator class (/sys/class/accel):
// NPUs like Intel's NPU, AMD XDNA and Qualcomm Cloud AI100. Same-driver
// devices collapse into one entry with a count suffix.
func scanAccelDrivers(root string) []string {
	entries, err := filepath.Glob(filepath.Join(root, "accel*"))
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	var order []string
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(e, "device", "driver"))
		if err != nil {
			continue
		}
		name := npuDisplayName(filepath.Base(link))
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		if counts[name] > 1 {
			name += fmt.Sprintf(" x%d", counts[name])
		}
		out = append(out, name)
	}
	return out
}

// npuDisplayName maps kernel driver names to human-friendly accelerators.
func npuDisplayName(driver string) string {
	switch driver {
	case "intel_vpu", "ivpu":
		return "Intel NPU"
	case "amdxdna":
		return "AMD XDNA NPU"
	case "qaic":
		return "Qualcomm Cloud AI100"
	default:
		return driver
	}
}

func cpuModelLinux() string {
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		if s := parseCPUModel(b); s != "" {
			return s
		}
	}
	// ARM and Apple Silicon often omit "model name"; the board string is
	// in the device tree, NUL-terminated.
	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		return strings.TrimRight(string(b), "\x00\n\r\t ")
	}
	return ""
}

// parseCPUModel picks a brand string out of /proc/cpuinfo. x86 uses
// "model name"; ARM often uses Hardware / Processor / cpu model instead,
// and "processor : 0" is a core index, not a brand. The key is folded with
// core.FoldASCII, the fold for /proc field names: the switch below matches
// ASCII literals, and strings.ToLower would also fold a rune whose lowercase
// is ASCII, so a key spelled with U+0130 or U+212A would reach a branch the
// kernel never wrote that key for.
func parseCPUModel(b []byte) string {
	var modelName, hardware, processor, cpuModel string
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := core.FoldASCII(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		switch key {
		case "model name":
			if modelName == "" {
				modelName = val
			}
		case "hardware":
			if hardware == "" {
				hardware = val
			}
		case "processor":
			if processor == "" {
				if _, err := strconv.Atoi(val); err != nil {
					processor = val // "0" is a core index, not a brand
				}
			}
		case "cpu model":
			if cpuModel == "" {
				cpuModel = val
			}
		}
	}
	for _, s := range []string{modelName, hardware, processor, cpuModel} {
		if s != "" {
			return s
		}
	}
	return ""
}

var gpuChips = []string{"amdgpu", "radeon", "nouveau", "nvidia", "i915", "xe"}

// scanTemps gathers readings, preferring hwmon chips and falling back to
// thermal zones only when hwmon yields nothing (they often duplicate).
func scanTemps(hwmonRoot, thermalRoot string) []core.TempReading {
	temps := readSensors(sensorLayout("hwmon\x00"+hwmonRoot, hwmonRoot, listHwmon))
	if len(temps) == 0 {
		temps = readSensors(sensorLayout("thermal\x00"+thermalRoot, thermalRoot, listThermalZones))
	}
	slices.SortStableFunc(temps, func(a, b core.TempReading) int {
		if a.IsGPU != b.IsGPU {
			if a.IsGPU {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.MilliC, a.MilliC)
	})
	if len(temps) > 16 { // keep the frame cheap on sensor-farm machines
		temps = temps[:16]
	}
	return temps
}

// sensorLayoutTTL bounds how long chip names, labels and paths are reused.
// The values still come from the input files every poll; only the walk of
// hwmon*/temp*_input and name/label files is amortized. A GPU that appears
// after start shows up on the next expiry, the same window hostStatic uses
// for drivers that load late.
const sensorLayoutTTL = 30 * time.Second

type sensorInput struct {
	path  string
	label string
	gpu   bool
}

type cachedSensors struct {
	inputs []sensorInput
	at     time.Time
}

var (
	sensorLayoutMu sync.Mutex
	sensorLayouts  = map[string]cachedSensors{}
)

func sensorLayout(key, root string, build func(string) []sensorInput) []sensorInput {
	sensorLayoutMu.Lock()
	defer sensorLayoutMu.Unlock()
	now := time.Now()
	for k, c := range sensorLayouts {
		if core.Age(now, c.at) >= sensorLayoutTTL {
			delete(sensorLayouts, k)
		}
	}
	if c, ok := sensorLayouts[key]; ok && core.Age(now, c.at) < sensorLayoutTTL {
		return c.inputs
	}
	// Build under the lock so concurrent samples share one walk and a
	// slower empty result cannot overwrite a newer fill.
	inputs := build(root)
	sensorLayouts[key] = cachedSensors{inputs: inputs, at: now}
	return inputs
}

func readSensors(inputs []sensorInput) []core.TempReading {
	var out []core.TempReading
	for _, in := range inputs {
		mc, ok := readMilliC(in.path)
		if !ok {
			continue
		}
		out = append(out, core.TempReading{Label: in.label, MilliC: mc, IsGPU: in.gpu})
	}
	return out
}

// sensorLabel normalizes a hwmon name/label or thermal-zone type. These are
// file contents on a host toktop may not own, and they reach a terminal, so
// escape sequences are stripped before the value is stored.
//
// Folded with core.FoldASCII, not strings.ToLower, because the result is
// matched against ASCII needles (gpuChips, "gpu", "junction"). ToLower also
// folds runes whose lowercase form is ASCII, so a chip named with U+212A
// (KELVIN SIGN) in place of a k satisfies a match its producer never wrote,
// which marks a chip as a GPU the hardware does not report as one.
func sensorLabel(raw string) string {
	return core.FoldASCII(strings.TrimSpace(core.SanitizeText(raw)))
}

func listHwmon(root string) []sensorInput {
	chips, err := filepath.Glob(filepath.Join(root, "hwmon*"))
	if err != nil {
		return nil
	}
	var out []sensorInput
	for _, chip := range chips {
		nameB, err := os.ReadFile(filepath.Join(chip, "name"))
		if err != nil {
			continue
		}
		chipName := sensorLabel(string(nameB))
		isGPUChip := core.ContainsAny(chipName, gpuChips...)
		inputs, _ := filepath.Glob(filepath.Join(chip, "temp*_input"))
		for _, in := range inputs {
			label := chipName
			base := strings.TrimSuffix(filepath.Base(in), "_input")
			if lb, err := os.ReadFile(filepath.Join(chip, base+"_label")); err == nil {
				label = sensorLabel(string(lb))
			}
			gpu := isGPUChip || core.ContainsAny(label, "gpu", "junction", "hotspot", "edge")
			out = append(out, sensorInput{path: in, label: label, gpu: gpu})
		}
	}
	return out
}

func listThermalZones(root string) []sensorInput {
	zones, err := filepath.Glob(filepath.Join(root, "thermal_zone*"))
	if err != nil {
		return nil
	}
	var out []sensorInput
	for _, z := range zones {
		tb, err := os.ReadFile(filepath.Join(z, "type"))
		if err != nil {
			continue
		}
		typ := sensorLabel(string(tb))
		out = append(out, sensorInput{
			path:  filepath.Join(z, "temp"),
			label: typ,
			gpu:   core.ContainsAny(typ, "gpu"),
		})
	}
	return out
}

func readMilliC(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return v, true
}
