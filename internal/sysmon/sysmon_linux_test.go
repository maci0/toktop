//go:build linux

package sysmon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildTree materializes files under a fresh temp root.
func buildTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanTempsPrefersHwmonAndClassifiesGPU(t *testing.T) {
	sysroot := buildTree(t, map[string]string{
		"class/hwmon/hwmon0/name":          "amdgpu\n",
		"class/hwmon/hwmon0/temp1_input":   "67000\n",
		"class/hwmon/hwmon0/temp1_label":   "edge\n",
		"class/hwmon/hwmon0/temp2_input":   "85000\n",
		"class/hwmon/hwmon0/temp2_label":   "junction\n",
		"class/hwmon/hwmon1/name":          "coretemp\n",
		"class/hwmon/hwmon1/temp1_input":   "52000\n",
		"class/hwmon/hwmon1/temp1_label":   "Package id 0\n",
		"class/thermal/thermal_zone0/type": "x86_pkg_temp",
		"class/thermal/thermal_zone0/temp": "99000", // ignored: hwmon exists
	})

	temps := scanTemps(
		filepath.Join(sysroot, "class/hwmon"),
		filepath.Join(sysroot, "class/thermal"),
	)
	if len(temps) != 3 {
		t.Fatalf("temps = %d, want 3: %+v", len(temps), temps)
	}
	if !temps[0].IsGPU || temps[0].MilliC != 85000 {
		t.Errorf("hottest GPU should sort first, got %+v", temps[0])
	}
	if temps[2].IsGPU || temps[2].Label != "package id 0" {
		t.Errorf("cpu reading wrong: %+v", temps[2])
	}
}

func TestScanTempsFallsBackToThermalZones(t *testing.T) {
	sysroot := buildTree(t, map[string]string{
		"class/thermal/thermal_zone0/type": "soc_thermal",
		"class/thermal/thermal_zone0/temp": "45000",
	})
	temps := scanTemps(
		filepath.Join(sysroot, "class/hwmon"), // empty
		filepath.Join(sysroot, "class/thermal"),
	)
	if len(temps) != 1 || temps[0].Label != "soc_thermal" {
		t.Fatalf("fallback failed: %+v", temps)
	}
}

func TestScanTempsEmptyDirs(t *testing.T) {
	root := t.TempDir()
	if temps := scanTemps(root, root); len(temps) != 0 {
		t.Fatalf("expected no temps, got %+v", temps)
	}
}

func TestScanAccelDriversCountsDevices(t *testing.T) {
	root := t.TempDir()
	mk := func(accel, driver string) {
		full := filepath.Join(root, "class", "accel", accel, "device")
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "drivers", driver), filepath.Join(full, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	mk("accel0", "amdxdna")
	mk("accel1", "amdxdna")
	mk("accel2", "intel_vpu")

	got := scanAccelDrivers(filepath.Join(root, "class", "accel"))
	want := []string{"AMD XDNA NPU x2", "Intel NPU"} // accel0/1 are amdxdna
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q want %q", i, got[i], want[i])
		}
	}

	if n := npuDisplayName("ivpu"); n != "Intel NPU" {
		t.Errorf("ivpu alias = %q", n)
	}
	if n := npuDisplayName("qaic"); n != "Qualcomm Cloud AI100" {
		t.Errorf("qaic = %q", n)
	}
	if n := npuDisplayName("somethingelse"); n != "somethingelse" {
		t.Errorf("unknown driver must pass through: %q", n)
	}
}

// parseNvidiaVersion feeds the identity row's driver/CUDA readout; pin the
// accepted layout and the missing-field fallbacks.
func TestParseNvidiaVersion(t *testing.T) {
	drv, cuda := parseNvidiaVersion(
		"NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.14  Wed May  1 23:26:36 UTC 2024\n" +
			"NVRM: Driver Version: 550.54.14         CUDA Version: 12.4\n")
	if drv != "550.54.14" || cuda != "12.4" {
		t.Errorf("drv=%q cuda=%q", drv, cuda)
	}
	drv, cuda = parseNvidiaVersion("NVRM: Driver Version: 535.104.05\n") // CUDA field absent
	if drv != "535.104.05" || cuda != "" {
		t.Errorf("drv=%q cuda=%q, want driver only", drv, cuda)
	}
	if drv, cuda := parseNvidiaVersion("no version data here"); drv != "" || cuda != "" {
		t.Errorf("unrelated text parsed as %q/%q", drv, cuda)
	}
}

func TestMergeHostStaticFillsGapsOnly(t *testing.T) {
	prev := hostStatic{osName: "Debian", nvidiaDrv: "550.54.14"}
	fresh := hostStatic{
		osName:    "other",
		kernel:    "6.1.0",
		nvidiaDrv: "560.0",
		cuda:      "12.4",
		amdgpu:    "6.7.0",
		npus:      []string{"Intel NPU"},
	}
	got := mergeHostStatic(prev, fresh)
	if got.osName != "Debian" || got.nvidiaDrv != "550.54.14" {
		t.Fatalf("filled fields were overwritten: %+v", got)
	}
	if got.kernel != "6.1.0" || got.cuda != "12.4" || got.amdgpu != "6.7.0" || len(got.npus) != 1 {
		t.Fatalf("empty fields were not filled: %+v", got)
	}
}

func TestHostStaticInfoRetriesEmptyDrivers(t *testing.T) {
	reset := func() {
		hostStaticMu.Lock()
		hostStaticVal = hostStatic{}
		hostStaticAt = time.Time{}
		hostStaticMu.Unlock()
	}
	reset()
	orig := loadHostStatic
	t.Cleanup(func() {
		loadHostStatic = orig
		reset()
	})

	var n int
	loadHostStatic = func() hostStatic {
		n++
		if n == 1 {
			return hostStatic{osName: "Debian", kernel: "6.1"}
		}
		return hostStatic{osName: "Debian", kernel: "6.1", nvidiaDrv: "550.54.14", cuda: "12.4"}
	}

	first := hostStaticInfo()
	if first.nvidiaDrv != "" || first.osName != "Debian" {
		t.Fatalf("first sample = %+v", first)
	}
	if n != 1 {
		t.Fatalf("probe ran %d times on first sample, want 1", n)
	}
	second := hostStaticInfo()
	if n != 1 || second.nvidiaDrv != "" {
		t.Fatalf("retried inside the window: calls=%d sample=%+v", n, second)
	}

	hostStaticMu.Lock()
	hostStaticAt = time.Now().Add(-hostStaticRetry - time.Second)
	hostStaticMu.Unlock()
	third := hostStaticInfo()
	if third.nvidiaDrv != "550.54.14" || third.cuda != "12.4" || third.osName != "Debian" {
		t.Fatalf("expired empty driver cache was not filled: %+v", third)
	}
	if n != 2 {
		t.Fatalf("probe ran %d times, want 2", n)
	}
}

func TestSensorLayoutDropsExpiredKeys(t *testing.T) {
	sensorLayoutMu.Lock()
	sensorLayouts = map[string]cachedSensors{
		"stale": {inputs: []sensorInput{{path: "gone"}}, at: time.Now().Add(-sensorLayoutTTL - time.Second)},
	}
	sensorLayoutMu.Unlock()
	t.Cleanup(func() {
		sensorLayoutMu.Lock()
		sensorLayouts = map[string]cachedSensors{}
		sensorLayoutMu.Unlock()
	})

	root := t.TempDir()
	_ = sensorLayout("fresh\x00"+root, root, listHwmon)

	sensorLayoutMu.Lock()
	_, still := sensorLayouts["stale"]
	sensorLayoutMu.Unlock()
	if still {
		t.Fatal("expired sensor layout still in the cache")
	}
}

func TestSensorLayoutDropsExpiredOnHit(t *testing.T) {
	sensorLayoutMu.Lock()
	sensorLayouts = map[string]cachedSensors{
		"stale": {inputs: []sensorInput{{path: "gone"}}, at: time.Now().Add(-sensorLayoutTTL - time.Second)},
		"warm":  {inputs: []sensorInput{{path: "ok"}}, at: time.Now()},
	}
	sensorLayoutMu.Unlock()
	t.Cleanup(func() {
		sensorLayoutMu.Lock()
		sensorLayouts = map[string]cachedSensors{}
		sensorLayoutMu.Unlock()
	})

	got := sensorLayout("warm", "", func(string) []sensorInput {
		t.Fatal("unexpected build call on cache hit")
		return nil
	})
	if len(got) != 1 || got[0].path != "ok" {
		t.Fatalf("warm layout = %+v, want ok", got)
	}

	sensorLayoutMu.Lock()
	_, still := sensorLayouts["stale"]
	sensorLayoutMu.Unlock()
	if still {
		t.Fatal("expired sensor layout still in the cache after a cache hit")
	}
}

func TestHostStaticNPUsDetached(t *testing.T) {
	orig := loadHostStatic
	t.Cleanup(func() { loadHostStatic = orig })

	loadHostStatic = func() hostStatic {
		return hostStatic{osName: "Debian", kernel: "6.1", npus: []string{"accel0"}}
	}
	hostStaticMu.Lock()
	hostStaticAt = time.Time{}
	hostStaticVal = hostStatic{}
	hostStaticMu.Unlock()

	h1 := hostStaticInfo()
	h1.npus[0] = "mutated"
	h2 := hostStaticInfo()
	if h2.npus[0] != "accel0" {
		t.Fatalf("hostStaticInfo aliased npus cache: %+v", h2.npus)
	}
}

func TestParseCPUModel(t *testing.T) {
	cases := map[string]string{
		"processor\t: 0\nmodel name\t: Intel(R) Core(TM) i7-12700K\n": "Intel(R) Core(TM) i7-12700K",
		"processor\t: 0\nBogoMIPS\t: 38.40\nHardware\t: BCM2835\n":    "BCM2835",
		"processor\t: 0\nProcessor\t: ARMv7 Processor rev 4 (v7l)\n":  "ARMv7 Processor rev 4 (v7l)",
		"processor\t: 0\ncpu model\t: Loongson-3A5000\n":              "Loongson-3A5000",
		"processor\t: 0\nBogoMIPS\t: 48.00\n":                         "",
		"model name\t: AMD EPYC\nHardware\t: other\n":                 "AMD EPYC",
	}
	for in, want := range cases {
		if got := parseCPUModel([]byte(in)); got != want {
			t.Errorf("parseCPUModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCPUModelCacheRetriesEmpty(t *testing.T) {
	reset := func() {
		cpuModelMu.Lock()
		cpuModelVal = ""
		cpuModelAt = time.Time{}
		cpuModelMu.Unlock()
	}
	reset()
	orig := cpuModelProbe
	t.Cleanup(func() {
		cpuModelProbe = orig
		reset()
	})

	var n int
	cpuModelProbe = func() string {
		n++
		if n == 1 {
			return ""
		}
		return "AMD EPYC"
	}

	if got := cpuModelCached(); got != "" {
		t.Fatalf("first empty probe = %q", got)
	}
	if got := cpuModelCached(); got != "" || n != 1 {
		t.Fatalf("retried inside the window: calls=%d val=%q", n, got)
	}

	cpuModelMu.Lock()
	cpuModelAt = time.Now().Add(-cpuModelRetry - time.Second)
	cpuModelMu.Unlock()
	if got := cpuModelCached(); got != "AMD EPYC" {
		t.Fatalf("expired empty cache was not filled: %q", got)
	}
	if n != 2 {
		t.Fatalf("probe ran %d times, want 2", n)
	}
	if got := cpuModelCached(); got != "AMD EPYC" || n != 2 {
		t.Fatalf("success was not kept: calls=%d val=%q", n, got)
	}
}

// utsField must stop at the NUL padding of a Utsname char array.
func TestUtsField(t *testing.T) {
	b := make([]byte, 65)
	copy(b, "6.1.0-18-amd64")
	if got := utsField(b); got != "6.1.0-18-amd64" {
		t.Errorf("utsField = %q", got)
	}
	if got := utsField(make([]byte, 65)); got != "" {
		t.Errorf("all-NUL array = %q", got)
	}
}

func TestSampleLinux(t *testing.T) {
	s := Sample()
	if s.MemTotal == 0 {
		t.Error("Sample() MemTotal = 0 on Linux")
	}
	if s.MemUsed > s.MemTotal {
		t.Errorf("Sample() MemUsed (%d) > MemTotal (%d)", s.MemUsed, s.MemTotal)
	}
	if s.HostUptime <= 0 {
		t.Errorf("Sample() HostUptime = %v, want > 0", s.HostUptime)
	}
	if s.OsName == "" {
		t.Error("Sample() OsName is empty on Linux")
	}
	if s.CPUModel == "" {
		t.Error("Sample() CPUModel is empty on Linux")
	}
}
