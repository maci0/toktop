// Package gpu samples accelerator telemetry across vendors.
//
// NVIDIA/AMD/Intel are read through their vendor CLIs (nvidia-smi,
// rocm-smi, xpu-smi) where present; AMD falls back to amdgpu sysfs on Linux,
// and on macOS the platform extras add the Apple devices system_profiler and
// ioreg report, which no vendor CLI here reads. We shell out deliberately:
// NVML and
// Level Zero have
// no stable in-process Go API without cgo-linking driver libraries, and the
// vendor CLIs are their documented interfaces. A resolved path is reused
// for toolHitTTL and a miss is retried on the shorter toolRetry, so a driver
// that appears or moves after start is picked up mid-session.
//
// One concern per file: gpu.go is the per-tick sampling fan-out, run.go the
// vendor-CLI execution and the caches around it (the resolved path, the
// injected clock, the outage latch), parse.go the decoders for what those
// tools printed.
package gpu

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/maci0/toktop/internal/core"
)

// platformExtras lets GOOS-specific files contribute devices the vendor CLIs
// do not report (amdgpu sysfs on Linux, system_profiler on macOS).
var platformExtras func(ctx context.Context) []core.GPUDevice

// nvidiaDecode reads nvidia-smi's headerless CSV once per poll and judges it
// by the same walk: output this build cannot read is a driver upgrade that
// changed the column set, or a tool that cannot reach the driver, and both
// leave the GPU row blank while the tool looks healthy. Same shape as the
// xpu metrics decode in sampleXPU.
func nvidiaDecode(devs *[]core.GPUDevice) func([]byte) error {
	return func(b []byte) error {
		*devs = ParseNvidiaSMI(b)
		if len(*devs) == 0 {
			return errors.New("no device rows in nvidia-smi CSV output")
		}
		return nil
	}
}

// rocmDecode is nvidiaDecode for rocm-smi, whose output is JSON keyed by
// card.
func rocmDecode(devs *[]core.GPUDevice) func([]byte) error {
	return func(b []byte) error {
		*devs = ParseRocmSMI(b)
		if len(*devs) == 0 {
			return errors.New("no cards in rocm-smi JSON output")
		}
		return nil
	}
}

var vendorOrder = map[string]int{
	core.VendorNvidia: 0,
	core.VendorAMD:    1,
	core.VendorIntel:  2,
	core.VendorApple:  3,
}

// maxXpuDevices caps how many Intel devices one tick spawns a metrics
// process for.
const maxXpuDevices = 4

// Sample collects devices from every vendor present on the host.
// Vendor CLIs are independent, so they run concurrently: a 1s nvidia-smi
// plus a 1s rocm-smi finishes in ~1s instead of ~2s. runTimeout bounds one
// CLI invocation, not a vendor, and xpu-smi takes two rounds (discovery, then
// metrics per device), so the whole sweep is bounded by sysmon's gpuBudget
// rather than by runTimeout.
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
			var devs []core.GPUDevice
			if _, ran := run(ctx, "nvidia-smi", p, nvidiaDecode(&devs), NvidiaQuery, NvidiaFormat); ran {
				add(devs)
			}
		}
	})
	wg.Go(func() {
		add(sampleAMD(ctx))
	})
	wg.Go(func() {
		if p, ok := lookup("xpu-smi"); ok {
			add(sampleXPU(ctx, "xpu-smi", p))
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
		var devs []core.GPUDevice
		// rocmDecode rejects an empty card list, so a successful run has
		// already proved devs holds at least one card.
		if _, ran := run(ctx, "rocm-smi", p, rocmDecode(&devs), RocmArgs()...); ran {
			return devs
		}
	}
	if platformExtras != nil {
		return platformExtras(ctx)
	}
	return nil
}

func sampleXPU(ctx context.Context, name, xpu string) []core.GPUDevice {
	out, ok := run(ctx, name, xpu, xpuUsable, "discovery", "-j")
	if !ok {
		return nil
	}
	discs := parseXpuDiscovery(out)
	if len(discs) > maxXpuDevices { // bound process spawns on multi-GPU nodes
		discs = discs[:maxXpuDevices]
	}
	// One spawn per device, each with its own runTimeout. Run them
	// concurrently: sequential calls stack, so on a four-device node the
	// last one starts after three runTimeout windows and the shared
	// sysmon budget has already cancelled it.
	devs := make([]*core.GPUDevice, len(discs))
	var wg sync.WaitGroup
	for i, d := range discs {
		wg.Go(func() {
			// The decode runs inside run so a metrics payload this build
			// cannot read is audited against the tool rather than silently
			// leaving the device without a row.
			var dev core.GPUDevice
			_, ok := run(ctx, name, xpu, func(b []byte) error {
				var pok bool
				if dev, pok = parseXpuMetrics(b, d.ID); !pok {
					return fmt.Errorf("metrics for device %d are not readable xpu-smi JSON", d.ID)
				}
				return nil
			}, "metrics", "-d", strconv.Itoa(d.ID), "-j")
			if !ok {
				return
			}
			dev.Vendor = core.VendorIntel
			dev.Name = d.Name
			devs[i] = &dev
		})
	}
	wg.Wait()
	devices := make([]core.GPUDevice, 0, len(devs))
	for _, d := range devs {
		if d != nil {
			devices = append(devices, *d)
		}
	}
	return devices
}
