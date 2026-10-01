package gpu

import (
	"cmp"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// The vendor CLI argument lists, named because the parsers below read their
// output positionally. A query edited on one side and not the other yields
// zeros on every device with no error, so the two live here: the local
// sampler passes them to the resolved path, and the ssh path splices the same
// strings into its remote shell.
const (
	// NvidiaQuery is the column list ParseNvidiaSMI reads, in order.
	NvidiaQuery = "--query-gpu=index,name,temperature.gpu,memory.used,memory.total,utilization.gpu,power.draw,driver_version"
	// NvidiaFormat is the CSV headerless, unitless form that query is parsed from.
	NvidiaFormat = "--format=csv,noheader,nounits"
)

// RocmArgs is rocm-smi's argument list, in the order ParseRocmSMI expects. A
// function rather than a var so no caller can reorder the slice it is
// matched against.
func RocmArgs() []string {
	return []string{"--showtemp", "--showusemem", "--showmeminfo", "vram", "--showuse", "--json"}
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
			Vendor:   core.VendorNvidia,
			Index:    index,
			Name:     core.ModelName(name),
			MilliC:   core.SatInt(flexF(tail[0]) * 1000),
			MemUsed:  mibBytes(flexF(tail[1])),
			MemTotal: mibBytes(flexF(tail[2])),
			UtilPct:  flexF(tail[3]),
			PowerW:   flexF(tail[4]),
			Driver:   core.ModelName(driver),
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
		d := core.GPUDevice{Vendor: core.VendorAMD, Index: index}
		fields := raw[card]
		for _, k := range slices.Sorted(maps.Keys(fields)) {
			lk, val := core.FoldASCII(k), flatten(fields[k])
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
	// The shape is read off the first token rather than by trying the wrapper
	// and falling back to the bare array. `xpu-smi discovery` answers with the
	// bare array, and a top-level array never decodes into the wrapper struct,
	// so the fallback order parsed every discovery payload twice on the one
	// shape that ships. This runs per device discovery poll.
	var list []device
	switch firstJSONToken(b) {
	case '[':
		if json.Unmarshal(b, &bare) != nil {
			return nil
		}
		list = bare
	case '{':
		if json.Unmarshal(b, &wrap) != nil || wrap.Devices == nil {
			return nil
		}
		list = wrap.Devices
	default:
		return nil
	}
	order := make([]xpuDevice, 0, len(list))
	for _, d := range list {
		order = append(order, xpuDevice{ID: d.DeviceID, Name: core.ModelName(d.DeviceName)})
	}
	return order
}

// firstJSONToken returns the first non-whitespace byte of a JSON payload, or 0
// when the payload holds none. It answers the shape question the decoder would
// otherwise answer twice, by failing, on the shape that does not apply.
func firstJSONToken(b []byte) byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	if i == len(b) {
		return 0
	}
	return b[i]
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
	d := core.GPUDevice{Vendor: core.VendorIntel, Index: index}
	// Sorted keys, same as ParseRocmSMI: map ranges are randomized, and
	// which of two overlapping sensors (two temperature keys, memory_size
	// vs memory_total) wins would otherwise flip between polls.
	for _, k := range slices.Sorted(maps.Keys(raw.Metrics)) {
		lk := core.FoldASCII(k)
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
