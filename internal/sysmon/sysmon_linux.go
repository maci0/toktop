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
)

const (
	procMeminfo = "/proc/meminfo"
	procLoadavg = "/proc/loadavg"
	procUptime  = "/proc/uptime"
	sysHwmon    = "/sys/class/hwmon"
	sysThermal  = "/sys/class/thermal"
)

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
		noteSourceFailure(path, err)
		return nil, false
	}
	noteSourceOK(path)
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
	if cpuModelVal != "" || (!cpuModelAt.IsZero() && core.Age(instant(), cpuModelAt) < cpuModelRetry) {
		v := cpuModelVal
		cpuModelMu.Unlock()
		return v
	}
	cpuModelMu.Unlock()
	// The probe reads /proc/cpuinfo and /proc/device-tree/model, and neither
	// read is bounded by anything toktop holds: a mount that stops answering
	// keeps the kernel in the read for as long as it takes. It runs with the
	// lock released so one such sample does not pin every other one behind
	// it, which is what a memo miss on the vitals poller and on the emit
	// loop's cold fallback looks like at once. Two samples may walk at the
	// same time; the store below keeps the first answer, so the slower walk
	// cannot blank a name the faster one found.
	val := cpuModelProbe()
	cpuModelMu.Lock()
	if cpuModelVal == "" {
		cpuModelVal, cpuModelAt = val, instant()
	}
	out := cpuModelVal
	cpuModelMu.Unlock()
	return out
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
	stale := hostStaticAt.IsZero() ||
		(hostStaticFill < hostStaticTries && core.Age(instant(), hostStaticAt) >= hostStaticRetry)
	res := hostStaticVal
	hostStaticMu.Unlock()
	if stale {
		// The load reads /etc/os-release, uname and the driver version files,
		// none of it bounded by a deadline. It runs with the lock released so
		// a mount that stops answering cannot hold every other sample on
		// this memo, and so the vitals poller and the emit loop's cold
		// fallback do not queue behind each other on a missing field. The
		// merge is by first answer, so overlapping loads cannot lose a
		// reading a slower one found.
		fresh := loadHostStatic()
		hostStaticMu.Lock()
		hostStaticVal = mergeHostStatic(hostStaticVal, fresh)
		hostStaticAt = instant()
		hostStaticFill++
		res = hostStaticVal
		hostStaticMu.Unlock()
	}
	res.npus = slices.Clone(res.npus)
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
		h.nvidiaDrv, h.cuda = parseNvidiaVersion(kernelText(b))
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
		s.Drivers[core.VendorNvidia] = h.nvidiaDrv
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
// leading and trailing quote, so PRETTY_NAME=""" reads as no name at all. A
// value that is not a Go string literal is returned verbatim rather than
// half-unwrapped, and the remote reader of the same file
// (internal/remote/stats.go trimQuotes) applies the same rule, so one file
// answers the same both ways.
//
// The bytes are a distribution's own text, written by its build rather than
// the kernel, so they cross kernelText like every other vendor-written string
// here: a PRETTY_NAME carrying a byte the build's encoding put there reaches
// the system panel as U+FFFD instead of as ill-formed bytes the panel cannot
// measure.
func prettyOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return parseOSRelease(b)
}

// parseOSRelease reads PRETTY_NAME out of an os-release file's bytes. The
// unquoted value crosses kernelText once more: strconv.Unquote reads the octal
// escapes a build never wrote as raw bytes, and a name carrying one reaches the
// panel as ill-formed text the panel cannot measure.
func parseOSRelease(b []byte) string {
	for line := range strings.SplitSeq(kernelText(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == "PRETTY_NAME" {
			v = strings.TrimSpace(v)
			if u, err := strconv.Unquote(v); err == nil {
				return kernelText([]byte(u))
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
	return driver, cuda
}

func sysModuleVersion(module string) string {
	b, err := os.ReadFile(filepath.Join("/sys/module", module, "version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(kernelText(b))
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
	// in the device tree, NUL-terminated. Firmware writes it, so it is
	// kernelText's boundary too: the encoding is whatever the board's
	// build used, and the system panel measures the result as text.
	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		return strings.TrimRight(kernelText(b), "\x00\n\r\t ")
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
//
// The values are firmware-written too, so they cross kernelText like the
// device-tree string beside them. A CPU string in a Latin-1 or Shift-JIS
// byte reaches the system panel as text, and U+FFFD is what that panel
// measures; left raw, the same bytes are dropped a byte at a time by the
// sanitizer downstream and the model name loses characters.
func parseCPUModel(b []byte) string {
	var modelName, hardware, processor, cpuModel string
	for line := range strings.SplitSeq(kernelText(b), "\n") {
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
	if len(temps) > maxTempSensors {
		temps = temps[:maxTempSensors]
	}
	return temps
}

// maxTempSensors caps the sensors one sample carries, hottest first with the
// GPUs sorted to the front. It keeps the frame cheap on sensor-farm machines:
// what the cap sheds is already gone by the time internal/ui counts its
// "+N more", which is taken from how many readings the strip actually drew
// (shownCPUTemps) rather than from this number.
const maxTempSensors = 16

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
	at := instant()
	sensorLayoutMu.Lock()
	for k, c := range sensorLayouts {
		if core.Age(at, c.at) >= sensorLayoutTTL {
			delete(sensorLayouts, k)
		}
	}
	// The sweep above already dropped every entry past its TTL, so anything
	// still in the map is fresh.
	c, ok := sensorLayouts[key]
	sensorLayoutMu.Unlock()
	if ok {
		return c.inputs
	}
	// The walk globs /sys/class/hwmon and reads every chip's name and label
	// labels, on a mount that can stop answering, so it runs with the lock
	// released: one stalled layout must not pin every other sample behind
	// it. Two walks can overlap, and the store below keeps the first answer
	// to land, so a slower walk cannot overwrite a newer layout.
	inputs := build(root)
	sensorLayoutMu.Lock()
	if c, ok := sensorLayouts[key]; ok {
		inputs = c.inputs
	} else {
		sensorLayouts[key] = cachedSensors{inputs: inputs, at: at}
	}
	sensorLayoutMu.Unlock()
	return inputs
}

// sensorBufBytes is the size of the buffer every sensor read is made into. A
// hwmon temp*_input holds a decimal number of millidegrees and a newline; a
// pathologically wide value is truncated rather than split across two reads,
// since the parse rejects anything that is not an integer anyway.
const sensorBufBytes = 64

// readSensors samples every input the layout walk found. The read is made
// into one buffer reused across the whole sweep rather than through
// os.ReadFile, which allocated a file-sized slice per sensor: a sensor-farm
// host with a discrete GPU carries tens of inputs and this runs on every
// sample. The result is sized to the input count, so a sweep of known length
// does not regrow it.
func readSensors(inputs []sensorInput) []core.TempReading {
	if len(inputs) == 0 {
		return nil
	}
	out := make([]core.TempReading, 0, len(inputs))
	buf := make([]byte, sensorBufBytes)
	for _, in := range inputs {
		mc, ok := readMilliCBuf(buf, in.path)
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
			// A zero-byte _label is a chip that has none, not one whose
			// label is empty: falling back to the chip name keeps the
			// gpu/junction needles working.
			if lb, err := os.ReadFile(filepath.Join(chip, base+"_label")); err == nil && len(lb) > 0 {
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
	v, ok := parseMilliC(b)
	return v, ok
}

// readMilliCBuf is readMilliC against a caller-owned buffer, so a sweep over
// many sensors allocates for the buffer once rather than once per sensor. A
// value too long for the buffer is refused rather than silently read short:
// the buffer is a bound, not a truncation, and a sensor reporting more digits
// than any temperature has is not one to guess at.
func readMilliCBuf(buf []byte, path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	n, err := f.Read(buf)
	cerr := f.Close()
	if err != nil || cerr != nil {
		return 0, false
	}
	if n == len(buf) {
		return 0, false
	}
	return parseMilliC(buf[:n])
}

func parseMilliC(b []byte) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return v, true
}
