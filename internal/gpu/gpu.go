// Package gpu samples accelerator telemetry across vendors.
//
// NVIDIA/AMD/Intel are read through their vendor CLIs (nvidia-smi,
// rocm-smi, xpu-smi). We shell out deliberately: NVML and Level Zero have
// no stable in-process Go API without cgo-linking driver libraries, and the
// vendor CLIs are their documented interfaces. A found tool is remembered
// for the process lifetime; a miss is retried so a driver that appears after
// start is not blank for the whole session.
package gpu

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

const runTimeout = 2500 * time.Millisecond

// pipeGrace bounds Output's wait on the command's output pipes after the
// process exits or is killed by the context deadline: a grandchild inheriting
// stdout (nested shells, CLI wrappers) would otherwise hold the read end
// open past every deadline and pin the sampler's caller indefinitely.
const pipeGrace = 500 * time.Millisecond

// platformExtras lets GOOS-specific files contribute devices (amdgpu sysfs
// on Linux, system_profiler on macOS).
var platformExtras func(ctx context.Context) []core.GPUDevice

type toolInfo struct {
	path string
	ok   bool
	at   time.Time // when a miss was recorded; hits are immortal
}

var tools sync.Map // tool name -> *toolInfo

// toolRetry spaces out LookPath of a missing vendor CLI. A miss cached
// forever would blank the GPU row for a session that started before the
// driver module (or its bin dir) was on PATH; a hit is still kept.
const toolRetry = 30 * time.Second

// lookPath is exec.LookPath, swapped in tests.
var lookPath = exec.LookPath

func lookup(name string) (string, bool) {
	if v, ok := tools.Load(name); ok {
		ti := v.(*toolInfo)
		if ti.ok || time.Since(ti.at) < toolRetry {
			return ti.path, ti.ok
		}
	}
	p, err := lookPath(name)
	ti := &toolInfo{path: p, ok: err == nil}
	if !ti.ok {
		ti.at = time.Now()
	}
	tools.Store(name, ti)
	return ti.path, ti.ok
}

func run(ctx context.Context, path string, args ...string) ([]byte, bool) {
	c, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, path, args...)
	cmd.WaitDelay = pipeGrace
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

var vendorOrder = map[string]int{"nvidia": 0, "amd": 1, "intel": 2, "apple": 3}

// Sample collects devices from every vendor present on the host.
// Vendor CLIs are independent and each can take up to runTimeout, so they
// run concurrently: a 1s nvidia-smi plus a 1s rocm-smi finishes in ~1s
// instead of ~2s on the sysmon budget.
func Sample(ctx context.Context) []core.GPUDevice {
	var (
		mu   sync.Mutex
		devs []core.GPUDevice
		wg   sync.WaitGroup
	)
	add := func(d []core.GPUDevice) {
		if len(d) == 0 {
			return
		}
		mu.Lock()
		devs = append(devs, d...)
		mu.Unlock()
	}

	wg.Go(func() {
		if p, ok := lookup("nvidia-smi"); ok {
			if out, ok2 := run(ctx, p,
				"--query-gpu=index,name,temperature.gpu,memory.used,memory.total,utilization.gpu,power.draw,driver_version",
				"--format=csv,noheader,nounits"); ok2 {
				add(ParseNvidiaSMI(out))
			}
		}
	})
	wg.Go(func() {
		add(sampleAMD(ctx))
	})
	wg.Go(func() {
		if p, ok := lookup("xpu-smi"); ok {
			add(sampleXPU(ctx, p))
		}
	})
	wg.Wait()

	slices.SortStableFunc(devs, func(a, b core.GPUDevice) int {
		if c := cmp.Compare(vendorOrder[a.Vendor], vendorOrder[b.Vendor]); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return devs
}

func sampleAMD(ctx context.Context) []core.GPUDevice {
	if p, ok := lookup("rocm-smi"); ok {
		if out, ok2 := run(ctx, p,
			"--showtemp", "--showusemem", "--showmeminfo", "vram", "--showuse", "--json"); ok2 {
			if devs := ParseRocmSMI(out); len(devs) > 0 {
				return devs
			}
		}
	}
	if platformExtras != nil {
		return platformExtras(ctx)
	}
	return nil
}

func sampleXPU(ctx context.Context, xpu string) []core.GPUDevice {
	out, ok := run(ctx, xpu, "discovery", "-j")
	if !ok {
		return nil
	}
	discs := parseXpuDiscovery(out)
	var devs []core.GPUDevice
	for _, d := range discs {
		if len(devs) >= 4 { // bound process spawns on multi-GPU nodes
			break
		}
		mo, ok2 := run(ctx, xpu, "metrics", "-d", strconv.Itoa(d.ID), "-j")
		if !ok2 {
			continue
		}
		if dev, ok3 := parseXpuMetrics(mo, d.ID); ok3 {
			dev.Vendor = "intel"
			dev.Name = d.Name
			devs = append(devs, dev)
		}
	}
	return devs
}

// ParseNvidiaSMI reads CSV rows of
// index,name,temp,memused,memtotal,util,power,driver_version.
// The name may contain commas; the last six fields are always fixed.
// Exported so the remote ssh path can parse nvidia-smi output gathered from
// another host with the exact same rules.
func ParseNvidiaSMI(b []byte) []core.GPUDevice {
	const fixedTail = 6
	var devs []core.GPUDevice
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < fixedTail+1 {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		name := strings.Join(f[1:len(f)-fixedTail], ",")
		tail := f[len(f)-fixedTail:]
		driver := strings.TrimSpace(tail[5])
		if driver == "[N/A]" {
			driver = ""
		}
		devs = append(devs, core.GPUDevice{
			Vendor:   "nvidia",
			Index:    index,
			Name:     strings.TrimSpace(name),
			MilliC:   core.SatInt(flexF(tail[0]) * 1000),
			MemUsed:  mibBytes(flexF(tail[1])),
			MemTotal: mibBytes(flexF(tail[2])),
			UtilPct:  flexF(tail[3]),
			PowerW:   flexF(tail[4]),
			Driver:   driver,
		})
	}
	return devs
}

// flexF parses vendor-CSV numbers, tolerating "[N/A]" / "[Not Supported]".
// Non-finite and non-positive results, including text like "nan" and "inf"
// that ParseFloat accepts, collapse to zero so no sensor can inject NaN or
// ±Inf into temps, percentages or watts downstream.
func flexF(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	v, _ := strconv.ParseFloat(s, 64)
	return posFinite(v)
}

// posFinite returns v when it is a usable positive measurement. NaN fails
// every comparison, +Inf compares greater than zero, and either would
// poison util/power readouts and every later format of them.
func posFinite(v float64) float64 {
	if !(v > 0) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// ParseRocmSMI reads rocm-smi --json output keyed by "cardN". Values may be
// strings or single-element arrays depending on version. Exported so the
// remote ssh path can parse rocm-smi output gathered from another host.
func ParseRocmSMI(b []byte) []core.GPUDevice {
	var raw map[string]map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	// Cards and their fields are visited in sorted order: map ranges are
	// randomized, and both the device list order and which sensor wins
	// where several report a temperature would otherwise flip between
	// polls on identical input.
	var devs []core.GPUDevice
	for _, card := range slices.Sorted(maps.Keys(raw)) {
		index := 0
		if _, rest, ok := strings.Cut(card, "card"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				if n, err := strconv.Atoi(fields[0]); err == nil {
					index = n
				}
			}
		}
		d := core.GPUDevice{Vendor: "amd", Index: index}
		fields := raw[card]
		for _, k := range slices.Sorted(maps.Keys(fields)) {
			lk, val := strings.ToLower(k), flatten(fields[k])
			switch {
			case strings.Contains(lk, "temperature"):
				if strings.Contains(lk, "edge") || d.MilliC == 0 {
					d.MilliC = core.SatInt(flexAny(val) * 1000)
				}
			case strings.Contains(lk, "used memory"):
				d.MemUsed = core.SatUint(flexAny(val))
			case strings.Contains(lk, "total memory"):
				d.MemTotal = core.SatUint(flexAny(val))
			case strings.Contains(lk, "gpu use"):
				d.UtilPct = flexAny(val)
			}
		}
		devs = append(devs, d)
	}
	slices.SortStableFunc(devs, func(a, b core.GPUDevice) int { return cmp.Compare(a.Index, b.Index) })
	return devs
}

type xpuDevice struct {
	ID   int
	Name string
}

// parseXpuDiscovery reads `xpu-smi discovery -j` output. Accepts either a
// bare device array or an object wrapping one.
func parseXpuDiscovery(b []byte) []xpuDevice {
	type device struct {
		DeviceID   int    `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	var (
		bare []device
		wrap struct {
			Devices []device `json:"devices"`
		}
	)
	list := bare
	switch {
	case json.Unmarshal(b, &wrap) == nil && wrap.Devices != nil:
		list = wrap.Devices
	case json.Unmarshal(b, &bare) == nil:
		list = bare
	default:
		return nil
	}
	order := make([]xpuDevice, 0, len(list))
	for _, d := range list {
		order = append(order, xpuDevice{ID: d.DeviceID, Name: d.DeviceName})
	}
	return order
}

// parseXpuMetrics reads `xpu-smi metrics -d N -j`; values arrive as
// {"values":[x]} wrappers whose key set drifts between releases.
func parseXpuMetrics(b []byte, index int) (core.GPUDevice, bool) {
	var raw struct {
		Metrics map[string]any `json:"metrics"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Metrics == nil {
		return core.GPUDevice{}, false
	}
	d := core.GPUDevice{Vendor: "intel", Index: index}
	// Sorted keys, same as ParseRocmSMI: map ranges are randomized, and
	// which of two overlapping sensors (two temperature keys, memory_size
	// vs memory_total) wins would otherwise flip between polls.
	for _, k := range slices.Sorted(maps.Keys(raw.Metrics)) {
		lk := strings.ToLower(k)
		val := flatten(raw.Metrics[k])
		switch {
		case strings.Contains(lk, "temperature"):
			d.MilliC = core.SatInt(flexAny(val) * 1000)
		case lk == "gpu_utilization":
			d.UtilPct = flexAny(val)
		case strings.Contains(lk, "memory_used"):
			d.MemUsed = core.SatUint(flexAny(val))
		case strings.Contains(lk, "memory_size"), strings.Contains(lk, "memory_total"):
			d.MemTotal = core.SatUint(flexAny(val))
		case lk == "gpu_power":
			d.PowerW = flexAny(val)
		}
	}
	return d, true
}

// flattenMaxDepth bounds unwrap of {"values":[x]} / [x] wrappers. Past the
// bound the value stays wrapped and flexAny treats it as junk.
const flattenMaxDepth = 8

func flatten(v any) any {
	for range flattenMaxDepth {
		switch t := v.(type) {
		case []any:
			if len(t) == 0 {
				return v
			}
			v = t[0]
		case map[string]any:
			vals, ok := t["values"]
			if !ok {
				return v
			}
			v = vals
		default:
			return v
		}
	}
	return v
}

// flexAny coerces JSON scalars to float64, ignoring junk. max(NaN, 0) is
// NaN in Go, and max(+Inf, 0) is +Inf, so the float64 arm uses the same
// filter as flexF rather than a clamp that would let either through.
func flexAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return posFinite(t)
	case string:
		return flexF(t)
	}
	return 0
}

// mibBytes converts a vendor-reported MiB count to bytes, saturating on
// absurd magnitudes and preserving fractional MiB.
func mibBytes(mib float64) uint64 {
	return core.SatUint(mib * (1 << 20))
}
