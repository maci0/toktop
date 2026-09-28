//go:build linux

package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// init wires the amdgpu sysfs walker as the platform extra. amdgpu exposes
// VRAM accounting, busy percentage and hwmon temperatures with no tooling,
// which covers AMD GPUs when rocm-smi is absent (common on desktops).
func init() {
	platformExtras = func(context.Context) []core.GPUDevice { return scanAmdSysfs(defaultDrmRoot) }
}

// defaultDrmRoot is the real sysfs mount. scanAmdSysfs walks it through the
// card cache; any other root (a test's fixture) is walked directly.
const defaultDrmRoot = "/sys/class/drm"

// amdCards caches which /sys/class/drm cards are amdgpu-bound. The set of
// cards a host has changes only when a GPU is added, while the poll runs
// every interval: without this, a machine with no AMD GPU (every NVIDIA or
// Intel one, where rocm-smi is absent and this path is the fallback) globs
// /sys/class/drm and stats each card for an answer that is permanently empty.
// A cached empty list is retried on the same spacing a missing vendor CLI is,
// so a GPU added mid-session is still found.
var amdCards struct {
	sync.Mutex
	dirs []string
	at   time.Time
}

func amdCardDirs() []string {
	amdCards.Lock()
	defer amdCards.Unlock()
	if core.Age(instant(), amdCards.at) < toolRetry {
		return amdCards.dirs
	}
	amdCards.dirs = findAmdCards(defaultDrmRoot)
	amdCards.at = instant()
	return amdCards.dirs
}

// findAmdCards is the discovery half of the sysfs walk: the directories under
// drmRoot that carry amdgpu's VRAM accounting.
func findAmdCards(drmRoot string) []string {
	cards, err := filepath.Glob(filepath.Join(drmRoot, "card[0-9]*"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(cards))
	for _, card := range cards {
		dev := filepath.Join(card, "device")
		if _, err := os.Stat(filepath.Join(dev, "mem_info_vram_used")); err != nil {
			continue // not an amdgpu-bound card
		}
		out = append(out, card)
	}
	return out
}

func scanAmdSysfs(drmRoot string) []core.GPUDevice {
	if drmRoot == defaultDrmRoot {
		return readAmdCards(amdCardDirs())
	}
	return readAmdCards(findAmdCards(drmRoot))
}

func readAmdCards(cards []string) []core.GPUDevice {
	var devs []core.GPUDevice
	for _, card := range cards {
		dev := filepath.Join(card, "device")
		d := core.GPUDevice{Vendor: "amd"}
		if _, rest, ok := strings.Cut(filepath.Base(card), "card"); ok {
			d.Index, _ = strconv.Atoi(rest)
		}
		if b, err := os.ReadFile(filepath.Join(dev, "product_name")); err == nil {
			d.Name = core.ModelName(string(b))
		}
		if u, err := readU64(dev, "mem_info_vram_used"); err == nil {
			d.MemUsed = u
		}
		if t, err := readU64(dev, "mem_info_vram_total"); err == nil {
			d.MemTotal = t
		}
		if b, err := os.ReadFile(filepath.Join(dev, "gpu_busy_percent")); err == nil {
			d.UtilPct = flexF(string(b))
		}
		scanAmdHwmon(filepath.Join(dev, "hwmon"), &d)
		devs = append(devs, d)
	}
	return devs
}

// scanAmdHwmon lifts temperature and power readings out of the card's hwmon
// dir.
func scanAmdHwmon(hwmonDir string, d *core.GPUDevice) {
	matches, _ := filepath.Glob(filepath.Join(hwmonDir, "hwmon*"))
	for _, h := range matches {
		if v, ok := readI(h, "temp1_input"); ok && d.MilliC == 0 {
			d.MilliC = v
		}
		if b, err := os.ReadFile(filepath.Join(h, "power1_average")); err == nil {
			if uw := flexF(string(b)); uw > 0 {
				d.PowerW = uw / 1e6 // microwatts -> watts
			}
		}
	}
}

func readI(dir, name string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return v, err == nil
}

func readU64(dir, name string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
