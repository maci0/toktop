//go:build linux

package sysmon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
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
		// A chip whose name is not a GPU, labelled by a sensor that is:
		// the label needles are the only thing that can catch this one.
		"class/hwmon/hwmon2/name":        "nouveau\n",
		"class/hwmon/hwmon2/temp1_input": "31000\n",
		"class/hwmon/hwmon2/temp1_label": "edge\n",
	})

	temps := scanTemps(
		filepath.Join(sysroot, "class/hwmon"),
		filepath.Join(sysroot, "class/thermal"),
	)
	// GPU readings first, hottest within each group. A regression that
	// marked every sensor a GPU, dropped a label, or lost the label-based
	// classification shows up as a diff here.
	want := []struct {
		label string
		milli int
		gpu   bool
	}{
		{"junction", 85000, true},
		{"edge", 67000, true},
		{"edge", 31000, true},
		{"package id 0", 52000, false},
	}
	if len(temps) != len(want) {
		t.Fatalf("temps = %d, want %d: %+v", len(temps), len(want), temps)
	}
	for i, w := range want {
		got := temps[i]
		if got.Label != w.label || got.MilliC != w.milli || got.IsGPU != w.gpu {
			t.Errorf("[%d] = %+v, want label %q at %d milliC, gpu %t", i, got, w.label, w.milli, w.gpu)
		}
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
	if len(temps) != 1 {
		t.Fatalf("fallback returned %d readings, want 1: %+v", len(temps), temps)
	}
	if got := temps[0]; got.Label != "soc_thermal" || got.MilliC != 45000 || got.IsGPU {
		t.Errorf("fallback reading = %+v, want label soc_thermal at 45000 milliC, gpu false", got)
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
	drv, cuda = parseNvidiaVersion( // CUDA on its own trailing line
		"NVRM: Driver Version: 535.104.05\nNVRM: CUDA Version: 12.6\n")
	if drv != "535.104.05" || cuda != "12.6" {
		t.Errorf("drv=%q cuda=%q, want the trailing line read", drv, cuda)
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

// reset clears the cached host static probe so the next call re-reads the
// process globals rather than a value a previous test faked in.
func reset() {
	hostStaticMu.Lock()
	hostStaticVal = hostStatic{}
	hostStaticAt = time.Time{}
	hostStaticFill = 0
	hostStaticMu.Unlock()
}

// The host identity window ages on the injected clock, so a run replaying on
// a stepped timeline expires it by stepping the clock. On the wall clock the
// window is a function of how long the process happened to run: two replays
// of one seed read the same /etc/os-release and cache it at different
// moments, and the host strip of the frame differs with nothing in the run
// to tell them apart.
func TestHostStaticWindowAgesOnTheInjectedClock(t *testing.T) {
	reset()
	orig := loadHostStatic
	t.Cleanup(func() {
		loadHostStatic = orig
		reset()
		SetNow(nil)
	})

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var stepped time.Time
	SetNow(func() time.Time { return stepped })

	var n int
	loadHostStatic = func() hostStatic {
		n++
		return hostStatic{osName: "Debian", kernel: "6.1"}
	}

	stepped = base
	if got := hostStaticInfo(); got.osName != "Debian" || n != 1 {
		t.Fatalf("first sample = %+v after %d probes, want Debian after 1", got, n)
	}

	stepped = base.Add(hostStaticRetry - time.Second)
	hostStaticInfo()
	if n != 1 {
		t.Fatalf("probe ran %d times inside the window, want 1", n)
	}

	stepped = base.Add(hostStaticRetry)
	if got := hostStaticInfo(); got.osName != "Debian" || n != 2 {
		t.Fatalf("probe ran %d times after the window on the injected clock, want 2: %+v", n, got)
	}
}

func TestHostStaticInfoRetriesEmptyDrivers(t *testing.T) {
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

// A host with no NPU and no AMD module has optional fields that stay empty
// forever. The memo must not treat that as "the cache never became valid":
// past the retry budget an empty optional field is answered, not chased, so
// the probe stops running for the life of the process.
func TestHostStaticInfoStopsRetrying(t *testing.T) {
	reset()
	orig := loadHostStatic
	t.Cleanup(func() {
		loadHostStatic = orig
		reset()
	})

	var n int
	loadHostStatic = func() hostStatic {
		n++
		return hostStatic{osName: "Debian", kernel: "6.1"}
	}

	hostStaticInfo() // the first probe
	for range hostStaticTries + 2 {
		hostStaticMu.Lock()
		hostStaticAt = time.Now().Add(-hostStaticRetry - time.Second)
		hostStaticMu.Unlock()
		hostStaticInfo()
	}
	if n != hostStaticTries {
		t.Fatalf("probe ran %d times, want the %d-probe budget", n, hostStaticTries)
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
	sensorLayout("fresh\x00"+root, root, listHwmon)

	sensorLayoutMu.Lock()
	_, still := sensorLayouts["stale"]
	_, stored := sensorLayouts["fresh\x00"+root]
	sensorLayoutMu.Unlock()
	if still {
		t.Fatal("expired sensor layout still in the cache")
	}
	if !stored {
		t.Fatal("the layout read here was never stored, so the cache rebuilds on every poll")
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
	reset()
	orig := loadHostStatic
	t.Cleanup(func() {
		loadHostStatic = orig
		reset()
	})

	loadHostStatic = func() hostStatic {
		return hostStatic{osName: "Debian", kernel: "6.1", npus: []string{"accel0"}}
	}

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

// A required /proc read that fails is not a fact about this host: the host
// strip then shows zero memory and no load, which is what an idle machine
// shows. readProc must record the outage once however often Sample is called,
// name the file, and say so again when the read works.
func TestReadProcAuditsOutageOnceAndRecovery(t *testing.T) {
	var lines bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := audit
	audit = func() *slog.Logger { return lg }
	defer func() { audit = old }()

	missing := filepath.Join(t.TempDir(), "proc", "meminfo")
	for range 3 {
		if _, ok := readProc(missing); ok {
			t.Fatal("readProc reported success for a file that does not exist")
		}
	}
	if n := strings.Count(lines.String(), "host vitals source unreadable"); n != 1 {
		t.Fatalf("audited %d outage lines for three failing reads, want 1:\n%s", n, lines.String())
	}
	if !strings.Contains(lines.String(), "meminfo") {
		t.Fatalf("the outage line does not name the file:\n%s", lines.String())
	}

	lines.Reset()
	if err := os.MkdirAll(filepath.Dir(missing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("1234.56 890.12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, ok := readProc(missing); !ok || len(b) == 0 {
		t.Fatalf("readProc(%s) = %q, %v; want the file read", missing, b, ok)
	}
	// Recovery only speaks for a file that had failed: a first read that works
	// is the normal case and must stay silent.
	if n := strings.Count(lines.String(), "host vitals source readable again"); n != 1 {
		t.Fatalf("audited %d recovery lines, want 1:\n%s", n, lines.String())
	}
}

// down_for is a duration, and the clock that stamps it is a wall clock on a
// real run. An NTP correction or a laptop resuming from sleep moves it
// backwards between the failure and the recovery, and the raw subtraction then
// audits "down_for=-2h0m0s": a negative outage, which reads as a broken clock
// rather than as a file that was unreadable for the length it was.
func TestReadProcOutageIsNotNegativeAfterAClockStep(t *testing.T) {
	var lines bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := audit
	audit = func() *slog.Logger { return lg }
	defer func() { audit = old }()

	now := time.Unix(1_700_000_000, 0).UTC()
	SetNow(func() time.Time { return now })
	defer SetNow(nil)

	missing := filepath.Join(t.TempDir(), "proc", "meminfo")
	readProc(missing)
	// The clock steps back two hours while the file stays unreadable.
	now = now.Add(-2 * time.Hour)
	if err := os.MkdirAll(filepath.Dir(missing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("1234.56 890.12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines.Reset()
	readProc(missing)
	if !strings.Contains(lines.String(), "down_for=0s") {
		t.Fatalf("recovery after a backward step audited a negative down_for:\n%s", lines.String())
	}
}

// A hwmon chip name is matched against the ASCII literals in gpuChips, so
// it folds with core.FoldASCII. strings.ToLower also folds runes whose
// lowercase form is ASCII: U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE)
// becomes "i", so a chip named "nvdİa" satisfies the "nvidia" needle
// under ToLower and is marked a GPU that no driver reports as one.
func TestSensorLabelFoldsASCIIOnly(t *testing.T) {
	for _, spoof := range []string{"nv\u0130a", "\u0130915"} {
		if got := sensorLabel(spoof); core.ContainsAny(got, gpuChips...) {
			t.Errorf("sensorLabel(%q) = %q, must not match a GPU chip", spoof, got)
		}
	}
	// The genuine spellings still match, which is what the fold is for.
	for _, real := range []string{"nvidia", "i915", "amdgpu"} {
		if got := sensorLabel(real); !core.ContainsAny(got, gpuChips...) {
			t.Errorf("sensorLabel(%q) = %q, want it to match a GPU chip", real, got)
		}
	}
}

// A /proc/cpuinfo key is folded the same way. The literal keys hold no
// foldable ASCII letter, so the guard is that a real key still reads and a
// key spelled with a rune that folds stays unrecognized.
func TestParseCPUModelFoldsASCIIOnly(t *testing.T) {
	if got := parseCPUModel([]byte("cpu model\t: Loongson-3A5000\n")); got != "Loongson-3A5000" {
		t.Errorf("parseCPUModel(cpu model) = %q, want Loongson-3A5000", got)
	}
	if got := parseCPUModel([]byte("cpu \u212Amodel\t: Spoofed\n")); got != "" {
		t.Errorf("parseCPUModel with a Kelvin-signed key = %q, want no brand", got)
	}
}

// The value of a /proc/cpuinfo brand string is firmware-written, so it is
// kernelText's boundary exactly as the device-tree model beside it is. A
// string in a Latin-1 or Shift-JIS byte has to reach the panel as U+FFFD
// rather than as raw bytes, which the sanitizer downstream drops one byte
// at a time and so loses characters.
func TestParseCPUModelKernelText(t *testing.T) {
	got := parseCPUModel([]byte("model name\t: Intel\xe9 Core i7\n"))
	if !utf8.ValidString(got) {
		t.Fatalf("parseCPUModel returned ill-formed UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "Intel") || !strings.Contains(got, "Core i7") {
		t.Errorf("parseCPUModel = %q, want the brand with the bad byte replaced", got)
	}
}
