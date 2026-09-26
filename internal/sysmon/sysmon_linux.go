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
	sysHwmon    = "/sys/class/hwmon"
	sysThermal  = "/sys/class/thermal"
)

func init() {
	platformMemory = sampleMemoryLinux
	platformLoad = func(s *core.SysSample) {
		if b, err := os.ReadFile(procLoadavg); err == nil {
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
	if b, err := os.ReadFile(procMeminfo); err == nil {
		ParseMeminfo(b, s)
	}
}

// hostStatic is identity that is usually fixed for the process lifetime:
// distro name, kernel release, driver versions and the NPU inventory.
// Filled fields are kept; empty optional ones (driver not loaded yet, NPU
// sysfs not mounted at start) are retried on hostStaticRetry so the first
// sample cannot blank them for the whole session.
type hostStatic struct {
	osName    string
	kernel    string
	nvidiaDrv string
	cuda      string
	amdgpu    string
	npus      []string
}

const hostStaticRetry = 30 * time.Second

var (
	hostStaticMu  sync.Mutex
	hostStaticVal hostStatic
	hostStaticAt  time.Time
)

func hostStaticInfo() hostStatic {
	hostStaticMu.Lock()
	defer hostStaticMu.Unlock()
	if hostStaticAt.IsZero() || (!hostStaticFilled(hostStaticVal) && time.Since(hostStaticAt) >= hostStaticRetry) {
		hostStaticVal = mergeHostStatic(hostStaticVal, loadHostStatic())
		hostStaticAt = time.Now()
	}
	return hostStaticVal
}

func hostStaticFilled(h hostStatic) bool {
	return h.osName != "" && h.kernel != "" && h.nvidiaDrv != "" && h.cuda != "" && h.amdgpu != "" && len(h.npus) > 0
}

func mergeHostStatic(prev, fresh hostStatic) hostStatic {
	if prev.osName == "" {
		prev.osName = fresh.osName
	}
	if prev.kernel == "" {
		prev.kernel = fresh.kernel
	}
	if prev.nvidiaDrv == "" {
		prev.nvidiaDrv = fresh.nvidiaDrv
	}
	if prev.cuda == "" {
		prev.cuda = fresh.cuda
	}
	if prev.amdgpu == "" {
		prev.amdgpu = fresh.amdgpu
	}
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
	s.NPUs = slices.Clone(h.npus)
}

func prettyOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == "PRETTY_NAME" {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func linuxUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	return ParseUptimeSecs(f[0])
}

// parseNvidiaVersion extracts "Driver Version: 550.54.14" and the trailing
// "CUDA Version: 12.4" from /proc/driver/nvidia/version.
func parseNvidiaVersion(text string) (driver, cuda string) {
	for line := range strings.SplitSeq(text, "\n") {
		if _, after, ok := strings.Cut(line, "Driver Version:"); ok {
			fields := strings.Fields(after)
			if len(fields) > 0 {
				driver = fields[0]
			}
			if _, after, ok := strings.Cut(line, "CUDA Version:"); ok {
				cfields := strings.Fields(after)
				if len(cfields) > 0 {
					cuda = cfields[0]
				}
			}
			return driver, cuda
		}
	}
	return "", ""
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
// and "processor : 0" is a core index, not a brand.
func parseCPUModel(b []byte) string {
	var modelName, hardware, processor, cpuModel string
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
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
	if c, ok := sensorLayouts[key]; ok && now.Sub(c.at) < sensorLayoutTTL {
		return c.inputs
	}
	for k, c := range sensorLayouts {
		if now.Sub(c.at) >= sensorLayoutTTL {
			delete(sensorLayouts, k)
		}
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

func listHwmon(root string) []sensorInput {
	chips, err := filepath.Glob(filepath.Join(root, "hwmon*"))
	if err != nil {
		return nil
	}
	var out []sensorInput
	for _, chip := range chips {
		nameB, err := os.ReadFile(filepath.Join(chip, "name"))
		chipName := strings.ToLower(strings.TrimSpace(string(nameB)))
		if err != nil {
			continue
		}
		isGPUChip := core.ContainsAny(chipName, gpuChips...)
		inputs, _ := filepath.Glob(filepath.Join(chip, "temp*_input"))
		for _, in := range inputs {
			label := chipName
			base := strings.TrimSuffix(filepath.Base(in), "_input")
			if lb, err := os.ReadFile(filepath.Join(chip, base+"_label")); err == nil {
				label = strings.ToLower(strings.TrimSpace(string(lb)))
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
		typ := strings.ToLower(strings.TrimSpace(string(tb)))
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
