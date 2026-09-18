//go:build darwin

package gpu

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Apple GPU support has two halves:
//
//   - Identity (chip name, VRAM size) comes from system_profiler once per
//     process on success; a failed probe is retried on later samples. There
//     is no pure-Go IOKit binding without cgo, and
//     system_profiler is its documented CLI, so shelling out is deliberate.
//   - Live statistics come from ioreg's IOAccelerator PerformanceStatistics,
//     readable without root: "In use GPU memory" in bytes plus utilization.
//     This refreshes on a throttle since Sample runs every poll cycle.

func init() {
	var (
		mu     sync.Mutex
		idents []core.GPUDevice
		next   time.Time // earliest retry after an unresolved probe
	)
	platformExtras = func(ctx context.Context) []core.GPUDevice {
		mu.Lock()
		defer mu.Unlock()
		// A failed or timed-out probe must not read as "no Apple GPU"
		// forever: system_profiler can exceed its budget on a cold or
		// loaded boot, and caching that miss would blank the Mac's GPU row
		// for the whole session. Unresolved probes retry on later samples,
		// gated by identityRetry so a persistently failing host pays at
		// most one spawn per window.
		if idents == nil && !next.After(time.Now()) {
			idents = appleGPUs(ctx)
			if len(idents) == 0 {
				next = time.Now().Add(identityRetry)
			}
		}
		if len(idents) == 0 {
			return nil
		}
		// Hand out a copy: samples are published to the UI goroutine, and a
		// later Sample overlaying live ioreg numbers onto the shared identity
		// slice would data-race with a render of an earlier snapshot.
		devs := append([]core.GPUDevice(nil), idents...)
		applyIOAccelStats(ctx, devs)
		return devs
	}
}

// identityRetry spaces out retries of an unresolved identity probe. Success
// is still cached for the process lifetime; only failures come back here.
const identityRetry = 30 * time.Second

// identityTimeout bounds one system_profiler call. Generous on purpose: the
// call is the whole identity probe, and a too-tight cap would leave the Mac
// with no GPU row until the next retry window opens.
const identityTimeout = 10 * time.Second

func appleGPUs(ctx context.Context) []core.GPUDevice {
	ctx, cancel := context.WithTimeout(ctx, identityTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "system_profiler", "SPDisplaysDataType", "-json")
	cmd.WaitDelay = pipeGrace // a hung profiler must not hold the caller past its deadline
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var doc struct {
		Displays []map[string]any `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return nil
	}
	var devs []core.GPUDevice
	for _, d := range doc.Displays {
		dev := core.GPUDevice{Vendor: "apple"}
		if name, ok := d["_name"].(string); ok {
			dev.Name = name
		}
		for k, v := range d {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "vram") {
				if s, ok := v.(string); ok {
					dev.MemTotal = parseSizeString(s)
				}
			}
			if dev.Name == "" && k == "sppci_model" {
				if s, ok := v.(string); ok {
					dev.Name = s
				}
			}
		}
		if dev.Name == "" {
			continue // skip anonymous entries
		}
		dev.Index = len(devs)
		devs = append(devs, dev)
	}
	return devs
}

var (
	ioAccelMu      sync.Mutex
	ioAccelAt      time.Time
	ioAccelMemUsed uint64
	ioAccelUtil    float64
)

const ioAccelRefresh = 2 * time.Second

// applyIOAccelStats overlays live memory/utilization onto the first device:
// aggregate accelerator numbers are exact on single-GPU Macs, which dominate;
// multi-GPU Mac Pros would need device matching that ioreg output does not
// expose portably, so they keep zeros rather than wrong ones.
func applyIOAccelStats(ctx context.Context, devs []core.GPUDevice) {
	ioAccelMu.Lock()
	defer ioAccelMu.Unlock()
	if time.Since(ioAccelAt) < ioAccelRefresh {
		devs[0].MemUsed = ioAccelMemUsed
		devs[0].UtilPct = ioAccelUtil
		return
	}
	// run() caps the spawn so a hung ioreg cannot pin ioAccelMu and stall
	// every later Sample. The caller's budget (sysmon gpuBudget) is the
	// parent, so a cancelled Sample does not wait out runTimeout.
	out, ok := run(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	noteIOAccel(ok, out)
	devs[0].MemUsed = ioAccelMemUsed
	devs[0].UtilPct = ioAccelUtil
}

// noteIOAccel stores a successful ioreg parse. A failure keeps the last
// good numbers rather than flashing zeros for the refresh window as if the
// GPU had gone idle.
func noteIOAccel(ok bool, out []byte) {
	if ok {
		ioAccelMemUsed, ioAccelUtil = parseIOAccelerator(string(out))
	}
	ioAccelAt = time.Now()
}

// parseIOAccelerator sums "In use GPU memory" across accelerators and takes
// the max utilization percentage. Key spellings drift between macOS
// releases; unknown lines are simply ignored.
func parseIOAccelerator(text string) (memUsed uint64, utilPct float64) {
	for line := range strings.SplitSeq(text, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		switch key {
		case `"In use GPU memory"`, `"gpumem_inuse"`:
			memUsed += parseIoregNum(v)
		case `"Device Utilization %"`, `"GPU Device Utilization %"`:
			if u := parseIoregNum(v); float64(u) > utilPct {
				utilPct = float64(u)
			}
		}
	}
	return memUsed, utilPct
}

func parseIoregNum(s string) uint64 {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	v, _ := strconv.ParseUint(s, 10, 64)
	return v // 0 on parse failure, the desired zero value
}

// parseSizeString reads vendor sizes like "128 GB", "8192 MB". The scaled
// magnitude goes through core.SatUint: junk input ("1e300 GB") must saturate
// rather than convert out of range, since the result sits in the
// process-lifetime identity cache
// identity cache for the whole process.
func parseSizeString(s string) uint64 {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	var mult float64
	switch strings.ToLower(strings.TrimSuffix(f[1], "B")) {
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	default:
		return 0
	}
	return core.SatUint(n * mult)
}
