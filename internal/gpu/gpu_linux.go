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

// amdCard is one amdgpu-bound card and the board name its sysfs reports.
// The name is fixed hardware identity, so it is discovered and cached with
// the card list rather than re-read per poll.
type amdCard struct {
	dir  string
	name string
}

// amdCards caches which /sys/class/drm cards are amdgpu-bound, and each one's
// board name. The set of cards a host has and the name of a card it has
// change only when a GPU is added, while the poll runs every interval:
// without this, a machine with no AMD GPU (every NVIDIA or Intel one, where
// rocm-smi is absent and this path is the fallback) globs /sys/class/drm and
// stats each card for an answer that is permanently empty, and a machine
// with one re-opens that card's product_name and re-sanitizes a constant
// string every second. A cached empty list is retried on the same spacing a
// missing vendor CLI is, so a GPU added mid-session is still found.
var amdCards struct {
	sync.Mutex
	cards []amdCard
	at    time.Time
}

func amdCardDirs() []amdCard {
	amdCards.Lock()
	defer amdCards.Unlock()
	if core.Age(instant(), amdCards.at) < toolRetry {
		return amdCards.cards
	}
	amdCards.cards = findAmdCards(defaultDrmRoot)
	amdCards.at = instant()
	return amdCards.cards
}

// findAmdCards is the discovery half of the sysfs walk: the cards under
// drmRoot that carry amdgpu's VRAM accounting, each with the board name its
// product_name file holds.
func findAmdCards(drmRoot string) []amdCard {
	cards, err := filepath.Glob(filepath.Join(drmRoot, "card[0-9]*"))
	if err != nil {
		return nil
	}
	out := make([]amdCard, 0, len(cards))
	for _, card := range cards {
		dev := filepath.Join(card, "device")
		if _, err := os.Stat(filepath.Join(dev, "mem_info_vram_used")); err != nil {
			continue // not an amdgpu-bound card
		}
		c := amdCard{dir: card}
		if b, err := os.ReadFile(filepath.Join(dev, "product_name")); err == nil {
			c.name = core.ModelName(string(b))
		}
		out = append(out, c)
	}
	return out
}

func scanAmdSysfs(drmRoot string) []core.GPUDevice {
	if drmRoot == defaultDrmRoot {
		return readAmdCards(amdCardDirs())
	}
	return readAmdCards(findAmdCards(drmRoot))
}

func readAmdCards(cards []amdCard) []core.GPUDevice {
	var devs []core.GPUDevice
	for _, card := range cards {
		dev := filepath.Join(card.dir, "device")
		d := core.GPUDevice{Vendor: "amd", Name: card.name}
		if _, rest, ok := strings.Cut(filepath.Base(card.dir), "card"); ok {
			d.Index, _ = strconv.Atoi(rest)
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
