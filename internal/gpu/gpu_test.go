package gpu

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

const nvidiaCSV = `0, NVIDIA GeForce RTX 4090, 65, 12345, 24564, 98, 350.51, 550.54.14
1, NVIDIA A100-SXM4-80GB, 71, 64000, 81920, 42, 275.00, [N/A]
2, Some, Name With Commas, 55, 100, 200, [N/A], [N/A], [N/A]
`

func TestParseNvidiaSMI(t *testing.T) {
	devs := ParseNvidiaSMI([]byte(nvidiaCSV))
	if len(devs) != 3 {
		t.Fatalf("devices = %d", len(devs))
	}
	d := devs[0]
	if d.Index != 0 || d.Name != "NVIDIA GeForce RTX 4090" || d.MilliC != 65000 {
		t.Errorf("dev0 = %+v", d)
	}
	if want := uint64(12345) << 20; d.MemUsed != want {
		t.Errorf("memused = %d want %d", d.MemUsed, want)
	}
	if d.UtilPct != 98 || d.PowerW != 350.51 {
		t.Errorf("util/power = %v/%v", d.UtilPct, d.PowerW)
	}
	if d.Driver != "550.54.14" {
		t.Errorf("extended fields: %+v", d)
	}
	if devs[1].Driver != "" { // "[N/A]" degrades to empty
		t.Errorf("driver N/A handling: %+v", devs[1])
	}
	if devs[2].Name != "Some, Name With Commas" || devs[2].PowerW != 0 ||
		devs[2].UtilPct != 0 || devs[2].Driver != "" {
		t.Errorf("comma-name / N/A handling: %+v", devs[2])
	}
}

const rocmJSON = `{
  "card0": {
    "Temperature (Sensor edge) (C)": "52.0",
    "Temperature (Sensor junction) (C)": ["61.0"],
    "VRAM Total Used Memory (B)": "17179869184",
    "VRAM Total Memory (B)": "68719476736",
    "GPU use (%)": "87"
  }
}`

func TestParseRocmSMI(t *testing.T) {
	devs := ParseRocmSMI([]byte(rocmJSON))
	if len(devs) != 1 {
		t.Fatalf("devices = %d", len(devs))
	}
	d := devs[0]
	if d.Vendor != "amd" || d.Index != 0 {
		t.Errorf("identity: %+v", d)
	}
	if d.MilliC != 52000 { // edge preferred over junction
		t.Errorf("temp = %d", d.MilliC)
	}
	if d.MemUsed != 16<<30 || d.MemTotal != 64<<30 {
		t.Errorf("vram = %d/%d", d.MemUsed, d.MemTotal)
	}
	if d.UtilPct != 87 {
		t.Errorf("util = %v", d.UtilPct)
	}
}

const xpuDiscoveryJSON = `{"devices":[{"device_id":0,"device_name":"Intel(R) Arc(TM) A770"},{"device_id":1,"device_name":"Intel(R) Data Center GPU Max"}]}`

const xpuMetricsJSON = `{"device_id":"0","metrics":{
		"gpu_utilization":{"values":[63.5]},
		"gpu_temperature":{"values":[58]},
		"memory_used":{"values":[1024]},
		"gpu_power":{"values":[180.5]}
	}}`

const xpuDiscoveryBare = `[{"device_id":3,"device_name":"iGPU"}]`

func TestParseXpuDiscoveryAndMetrics(t *testing.T) {
	order := parseXpuDiscovery([]byte(xpuDiscoveryJSON))
	if len(order) != 2 || order[1].Name != "Intel(R) Data Center GPU Max" {
		t.Fatalf("discovery parse: %+v", order)
	}
	dev, ok := parseXpuMetrics([]byte(xpuMetricsJSON), 0)
	if !ok {
		t.Fatal("metrics parse failed")
	}
	if dev.MilliC != 58000 || dev.UtilPct != 63.5 || dev.MemUsed != 1024 || dev.PowerW != 180.5 {
		t.Errorf("metrics: %+v", dev)
	}

	order = parseXpuDiscovery([]byte(xpuDiscoveryBare))
	if len(order) != 1 || order[0].ID != 3 {
		t.Errorf("bare array discovery: %+v", order)
	}
}

// Two temperature keys must pick the same winner every time: last write on
// sorted keys, matching ParseRocmSMI's stable visit order.
func TestParseXpuMetricsOverlappingSensorsAreStable(t *testing.T) {
	const body = `{"metrics":{
		"gpu_temperature_1":{"values":[90]},
		"gpu_temperature":{"values":[58]}
	}}`
	dev, ok := parseXpuMetrics([]byte(body), 0)
	if !ok {
		t.Fatal("metrics parse failed")
	}
	if dev.MilliC != 90000 {
		t.Errorf("temp = %d, want 90000 (last sorted temperature key)", dev.MilliC)
	}
	dev2, ok2 := parseXpuMetrics([]byte(body), 0)
	if !ok2 || dev2 != dev {
		t.Fatal("parseXpuMetrics is not deterministic")
	}
}

func TestFlexF(t *testing.T) {
	cases := map[string]float64{
		"[N/A]": 0, "[Not Supported]": 0, "42.5": 42.5, "-1": 0, " 12 ": 12,
		"NaN": 0, "nan": 0, "inf": 0, "+Inf": 0, "-Inf": 0, "Infinity": 0,
	}
	for in, want := range cases {
		if got := flexF(in); got != want {
			t.Errorf("flexF(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFlexAnyRejectsNonFinite(t *testing.T) {
	if got := flexAny(math.Inf(1)); got != 0 {
		t.Errorf("flexAny(+Inf) = %v, want 0", got)
	}
	if got := flexAny(math.NaN()); got != 0 {
		t.Errorf("flexAny(NaN) = %v, want 0", got)
	}
	if got := flexAny(-3.0); got != 0 {
		t.Errorf("flexAny(-3) = %v, want 0", got)
	}
	if got := flexAny(87.0); got != 87 {
		t.Errorf("flexAny(87) = %v, want 87", got)
	}
}

func TestMibBytesFractional(t *testing.T) {
	if got := mibBytes(1.5); got != 1572864 {
		t.Errorf("mibBytes(1.5) = %d, want 1572864", got)
	}
	if got := mibBytes(0.5); got != 524288 {
		t.Errorf("mibBytes(0.5) = %d, want 524288", got)
	}
	if got := mibBytes(0); got != 0 {
		t.Errorf("mibBytes(0) = %d, want 0", got)
	}
	if got := mibBytes(math.NaN()); got != 0 {
		t.Errorf("mibBytes(NaN) = %d, want 0", got)
	}
}

func TestParseNvidiaSMIInfUtilIsZero(t *testing.T) {
	devs := ParseNvidiaSMI([]byte("0, GPU, 55, 100, 200, inf, inf, 550.54.14\n"))
	if len(devs) != 1 {
		t.Fatalf("devices = %d", len(devs))
	}
	if devs[0].UtilPct != 0 || devs[0].PowerW != 0 {
		t.Errorf("inf util/power leaked: %+v", devs[0])
	}
	if devs[0].MilliC != 55000 {
		t.Errorf("sane temp disturbed: %+v", devs[0])
	}
}

// Vendor output is untrusted: a broken CLI or JSON feed may report any
// finite magnitude, and the float->int conversion behind MilliC/MemUsed is
// implementation-defined past the type's range (on amd64 a huge temp
// renders as a huge negative number). Absurd magnitudes must saturate.
// A values wrapper nested past flattenMaxDepth is junk, not a hang or a
// stack blow-up: the parser must stop and report no reading.
func TestFlattenBoundsDeepValuesWrappers(t *testing.T) {
	inner := `"87"`
	wrapped := inner
	for range 32 {
		wrapped = `{"values":[` + wrapped + `]}`
	}
	devs := ParseRocmSMI([]byte(`{"card0":{"GPU use (%)":` + wrapped + `}}`))
	if len(devs) != 1 {
		t.Fatalf("devices = %d", len(devs))
	}
	if devs[0].UtilPct != 0 {
		t.Fatalf("deeply nested values wrapper parsed as %v; want 0", devs[0].UtilPct)
	}
}

func TestSaturatesAbsurdVendorNumbers(t *testing.T) {
	devs := ParseNvidiaSMI([]byte("0, GPU, 1e300, 24564, 81920, 50, 300, 550.54.14\n" +
		"1, GPU2, 55, 1e300, 81920, 50, 300, 550.54.14\n"))
	if len(devs) != 2 {
		t.Fatalf("devices = %d", len(devs))
	}
	if devs[0].MilliC != math.MaxInt {
		t.Errorf("MilliC = %d, want saturation", devs[0].MilliC)
	}
	if devs[0].MemTotal != 81920<<20 || devs[0].UtilPct != 50 || devs[0].PowerW != 300 {
		t.Errorf("sane columns disturbed: %+v", devs[0])
	}
	if devs[1].MemUsed != math.MaxUint64 {
		t.Errorf("MemUsed = %d, want saturation", devs[1].MemUsed)
	}
}

func TestVendorOrdering(t *testing.T) {
	if vendorOrder["nvidia"] >= vendorOrder["amd"] || vendorOrder["amd"] >= vendorOrder["intel"] {
		t.Fatal("vendor sort order drifted")
	}
	var _ core.GPUDevice // keep import honest
}

func TestLookupRetriesExpiredMisses(t *testing.T) {
	name := "toktop-missing-gpu-tool"
	tools.Delete(name)
	orig := lookPath
	t.Cleanup(func() {
		lookPath = orig
		tools.Delete(name)
	})

	var n int
	lookPath = func(string) (string, error) {
		n++
		if n == 1 {
			return "", exec.ErrNotFound
		}
		return "/opt/bin/" + name, nil
	}

	if _, ok := lookup(name); ok {
		t.Fatal("first lookup of a missing tool must miss")
	}
	if _, ok := lookup(name); ok {
		t.Fatal("a fresh miss must still be served from cache")
	}
	if n != 1 {
		t.Fatalf("LookPath called %d times before expiry, want 1", n)
	}

	v, ok := tools.Load(name)
	if !ok {
		t.Fatal("miss was not stored")
	}
	v.(*toolInfo).at = time.Now().Add(-toolRetry - time.Second)

	path, ok := lookup(name)
	if !ok || path != "/opt/bin/"+name {
		t.Fatalf("expired miss = %q, %v; want the tool that appeared later", path, ok)
	}
	if n != 2 {
		t.Fatalf("LookPath called %d times, want 2 (retry after expiry)", n)
	}

	path, ok = lookup(name)
	if !ok || path != "/opt/bin/"+name || n != 2 {
		t.Fatalf("hit was re-probed: path=%q ok=%v calls=%d", path, ok, n)
	}
}

// A driver or container reinstall can move or remove a vendor CLI. A hit
// cached for the process lifetime would keep executing the stale path and
// leave the GPU row empty for the rest of the session, so the lookup is
// re-resolved once the hit window passes.
func TestLookupRetriesExpiredHit(t *testing.T) {
	name := "toktop-moving-gpu-tool"
	tools.Delete(name)
	orig := lookPath
	t.Cleanup(func() {
		lookPath = orig
		tools.Delete(name)
	})

	var n int
	lookPath = func(string) (string, error) {
		n++
		if n == 1 {
			return "/opt/old/" + name, nil
		}
		return "/opt/new/" + name, nil
	}

	if path, ok := lookup(name); !ok || path != "/opt/old/"+name {
		t.Fatalf("first lookup = %q, %v; want the original path", path, ok)
	}
	if path, ok := lookup(name); !ok || path != "/opt/old/"+name || n != 1 {
		t.Fatalf("hit inside the window = %q, %v; calls=%d", path, ok, n)
	}

	v, ok := tools.Load(name)
	if !ok {
		t.Fatal("hit was not stored")
	}
	v.(*toolInfo).at = time.Now().Add(-toolHitTTL - time.Second)

	path, ok := lookup(name)
	if !ok || path != "/opt/new/"+name {
		t.Fatalf("expired hit = %q, %v; want the relocated tool", path, ok)
	}
	if n != 2 {
		t.Fatalf("LookPath called %d times, want 2 (re-resolve after expiry)", n)
	}
}

func TestSample(t *testing.T) {
	ctx := t.Context()
	devs := Sample(ctx)
	for i := 1; i < len(devs); i++ {
		prev, cur := devs[i-1], devs[i]
		if vendorOrder[prev.Vendor] > vendorOrder[cur.Vendor] {
			t.Errorf("devices not sorted by vendorOrder: %s > %s", prev.Vendor, cur.Vendor)
		} else if vendorOrder[prev.Vendor] == vendorOrder[cur.Vendor] && prev.Index > cur.Index {
			t.Errorf("devices not sorted by index for vendor %s: %d > %d", prev.Vendor, prev.Index, cur.Index)
		}
	}
}

// A caller that cancels (the UI tearing down, the sysmon budget spent) must
// not pay runTimeout per vendor, and must not report a device the canceled
// sample never got to read. The fake CLI is present on PATH, so a Sample that
// ignored the cancellation would block for the full timeout and leave the
// marker behind.
func TestSampleCanceledContextSkipsVendorCLIs(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "cli-ran")
	fake := filepath.Join(dir, "toktop-fake-smi")
	script := "#!/bin/sh\ntouch " + marker + "\n" + nvidiaCSV
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubTools(t, func(string) (string, error) { return fake, nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	devs := Sample(ctx)
	if elapsed := time.Since(start); elapsed >= runTimeout {
		t.Errorf("canceled Sample took %s: it waited out the %s vendor timeout", elapsed, runTimeout)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("canceled Sample still spawned a vendor CLI")
	}
	for _, d := range devs {
		if strings.Contains(d.Name, "RTX") {
			t.Errorf("canceled Sample reported a device the canceled CLI never returned: %+v", d)
		}
	}
}

// The same stub with a live context must produce the device, so the
// cancellation assertions above cannot pass by the fake CLI simply never
// being reached.
func TestSampleReadsVendorCLIOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "toktop-fake-smi")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat <<'CSV'\n"+nvidiaCSV+"CSV\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubTools(t, func(string) (string, error) { return fake, nil })

	var nvidia []core.GPUDevice
	for _, d := range Sample(t.Context()) {
		if d.Vendor == "nvidia" {
			nvidia = append(nvidia, d)
		}
	}
	if len(nvidia) != 3 {
		t.Fatalf("nvidia devices = %d, want the 3 rows the fake CLI printed: %+v", len(nvidia), nvidia)
	}
	if nvidia[0].Name != "NVIDIA GeForce RTX 4090" || nvidia[2].Name != "Some, Name With Commas" {
		t.Errorf("nvidia devices = %+v, want the printed rows in order", nvidia)
	}
}

// Sample merges three vendors sampled concurrently, so the merge order is not
// the order the CLIs finished in. The host supplies whatever GPUs it has,
// which on a build machine is none: this pins the sort against a known
// three-vendor set so the ordering is checked on every machine.
func TestSampleOrdersVendorsAndIndices(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	// xpu-smi is called twice, once per subcommand; the discovery listing and
	// the per-device metrics are different documents.
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The heredoc terminator must be alone on its line, so the document
	// needs a trailing newline.
	cat := func(doc string) string { return "cat <<'OUT'\n" + strings.TrimRight(doc, "\n") + "\nOUT\n" }
	// Intel's indices sort ahead of nvidia's on purpose: only the vendor rank
	// may decide the order, and a plain index sort would get it wrong.
	paths := map[string]string{
		"nvidia-smi": write("nvidia-smi", "#!/bin/sh\n"+cat(
			"1, Fake NVIDIA B, 65, 1, 2, 98, 350, 550.0\n"+
				"0, Fake NVIDIA A, 65, 1, 2, 98, 350, 550.0\n")),
		"rocm-smi": write("rocm-smi", "#!/bin/sh\n"+cat(rocmFakeJSON)),
		"xpu-smi":  write("xpu-smi", "#!/bin/sh\ncase \"$1\" in\ndiscovery) "+cat(xpuFakeDiscovery)+";;\n*) "+cat(xpuFakeMetrics)+";;\nesac\n"),
	}
	stubTools(t, func(name string) (string, error) {
		p, ok := paths[name]
		if !ok {
			return "", exec.ErrNotFound
		}
		return p, nil
	})

	// The host's own /sys/class/drm cards come back too, and this machine may
	// have any number of them, so pick out the fakes by what only they carry.
	var got []string
	for _, d := range Sample(t.Context()) {
		switch {
		case strings.HasPrefix(d.Name, "Fake NVIDIA"):
			got = append(got, "nvidia/"+d.Name)
		case d.Vendor == "amd" && d.MemUsed == rocmFakeUsed:
			got = append(got, fmt.Sprintf("amd/card%d", d.Index))
		case strings.HasPrefix(d.Name, "Fake Intel"):
			got = append(got, fmt.Sprintf("intel/%d", d.Index))
		}
	}
	want := []string{"nvidia/Fake NVIDIA A", "nvidia/Fake NVIDIA B", "amd/card0", "amd/card1",
		"intel/0", "intel/1"}
	if !slices.Equal(got, want) {
		t.Fatalf("vendor/index order = %v, want %v", got, want)
	}
}

const rocmFakeUsed = 17179869184

const xpuFakeMetrics = `{"device_id":"0","metrics":{
	"gpu_utilization":{"values":[63.5]},
	"gpu_temperature":{"values":[58]},
	"memory_used":{"values":[1024]},
	"gpu_power":{"values":[180.5]}}}`

const rocmFakeJSON = `{"card0":{"Temperature (Sensor edge) (C)":"52.0",
	"VRAM Total Used Memory (B)":"17179869184","VRAM Total Memory (B)":"68719486736","GPU use (%)":"87"},
	"card1":{"Temperature (Sensor edge) (C)":"52.0",
	"VRAM Total Used Memory (B)":"17179869184","VRAM Total Memory (B)":"34359738368","GPU use (%)":"87"}}`

const xpuFakeDiscovery = `{"devices":[
	{"device_id":1,"device_name":"Fake Intel B"},
	{"device_id":0,"device_name":"Fake Intel A"}]}`

// stubTools points every vendor CLI lookup at fn and clears the process-wide
// tool cache, which otherwise keeps a hit (or an unexpired miss) for the
// rest of the test binary's life.
func stubTools(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	const names = "nvidia-smi\000rocm-smi\000xpu-smi"
	orig := lookPath
	clear := func() {
		for _, n := range strings.Split(names, "\x00") {
			tools.Delete(n)
		}
	}
	clear()
	lookPath = fn
	t.Cleanup(func() { lookPath = orig; clear() })
}
