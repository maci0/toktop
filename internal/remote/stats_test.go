package remote

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

const vitalsDump = `3.10 2.20 1.05 4/900 12345
%toktop%
MemTotal:       16096680 kB
MemAvailable:   8000000 kB
SwapTotal:       2000000 kB
SwapFree:        1000000 kB
%toktop%
183729.42 712000.11
%toktop%
AMD Ryzen 9 7950X 16-Core Processor
%toktop%
"Debian GNU/Linux 12 (bookworm)"
%toktop%
6.1.0-18-amd64
%toktop%
0, NVIDIA GeForce RTX 4090, 54, 12345, 24564, 97, 410.2, 550.54.15
1, NVIDIA A100-SXM4-40GB, 61, 30000, 40960, 80, 250.0, [N/A]
`

func TestParseVitals(t *testing.T) {
	var s core.SysSample
	parseVitals(vitalsDump, &s)

	if s.Load1 != 3.10 || s.Load5 != 2.20 || s.Load15 != 1.05 {
		t.Errorf("load = %v %v %v", s.Load1, s.Load5, s.Load15)
	}
	if want := uint64(16096680) << 10; s.MemTotal != want {
		t.Errorf("memtotal = %d want %d", s.MemTotal, want)
	}
	if s.MemUsed != (uint64(16096680)-8000000)<<10 {
		t.Errorf("memused = %d", s.MemUsed)
	}
	if s.SwapUsed != (uint64(2000000)-1000000)<<10 {
		t.Errorf("swapused = %d", s.SwapUsed)
	}
	if want := time.Duration(183729.42 * float64(time.Second)); s.HostUptime != want {
		t.Errorf("uptime = %v want %v", s.HostUptime, want)
	}
	if s.CPUModel != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("cpumodel = %q", s.CPUModel)
	}
	if s.OsName != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("osname = %q", s.OsName)
	}
	if s.Kernel != "6.1.0-18-amd64" {
		t.Errorf("kernel = %q", s.Kernel)
	}
	if len(s.GPUs) != 2 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}
	g0 := s.GPUs[0]
	if g0.Index != 0 || g0.Vendor != "nvidia" || g0.Name != "NVIDIA GeForce RTX 4090" ||
		g0.MilliC != 54000 || g0.UtilPct != 97 || g0.PowerW != 410.2 || g0.Driver != "550.54.15" {
		t.Errorf("gpu[0] = %+v", g0)
	}
	g1 := s.GPUs[1]
	if g1.Name != "NVIDIA A100-SXM4-40GB" || g1.Driver != "" {
		t.Errorf("gpu[1] = %+v", g1) // [N/A] fields must degrade to zero values
	}
	if s.Drivers["nvidia"] != "550.54.15" {
		t.Errorf("drivers = %v", s.Drivers)
	}
}

func TestParseVitalsCRLF(t *testing.T) {
	crlfDump := strings.ReplaceAll(vitalsDump, "\n", "\r\n")
	var s core.SysSample
	parseVitals(crlfDump, &s)

	if s.Load1 != 3.10 || s.Load5 != 2.20 || s.Load15 != 1.05 {
		t.Errorf("load = %v %v %v", s.Load1, s.Load5, s.Load15)
	}
	if s.CPUModel != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("cpumodel = %q", s.CPUModel)
	}
	if s.OsName != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("osname = %q", s.OsName)
	}
	if s.Kernel != "6.1.0-18-amd64" {
		t.Errorf("kernel = %q", s.Kernel)
	}
	if len(s.GPUs) != 2 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}
}

func TestParseVitalsInfLoadAndHugeUptime(t *testing.T) {
	var s core.SysSample
	if parseVitals(vitalsDumpFrom("+Inf nan 0", "", "1000000000000", "", "", "", ""), &s) {
		t.Fatal("Inf loadavg reported usable")
	}
	if s.Load1 != 0 || s.Load5 != 0 || s.Load15 != 0 {
		t.Errorf("Inf loadavg leaked: %v %v %v", s.Load1, s.Load5, s.Load15)
	}
	if s.HostUptime != time.Duration(1<<63-1) {
		t.Errorf("huge uptime = %v, want saturation", s.HostUptime)
	}
	s = core.SysSample{}
	parseVitals(vitalsDumpFrom("1.0 1.0 1.0", "", "+Inf", "", "", "", ""), &s)
	if s.HostUptime != 0 {
		t.Errorf("Inf uptime = %v, want 0", s.HostUptime)
	}
}

func TestParseVitalsPartial(t *testing.T) {
	var s core.SysSample
	s.CPUModel = "keep me"
	parseVitals("\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n", &s)
	if s.CPUModel != "keep me" || s.OsName != "" || s.Kernel != "" || len(s.GPUs) != 0 {
		t.Errorf("empty sections must not clobber: %+v", s)
	}
}

// A remote that polls fine but publishes no loadavg (macOS, hardened
// kernels) must not zero the local load readout: Merge overlays fresh data,
// and without the validity flag every poll would clobber local values with
// absent ones for as long as the connection stays up.
func TestMergeKeepsLocalLoadsWithoutRemoteLoadavg(t *testing.T) {
	var s Stats
	into := core.SysSample{Load1: 1.5, Load5: 1.2, Load15: 0.9}

	// macOS-shaped dump: no /proc/loadavg section, but CPU model arrives.
	if loadsOK := parseVitals(vitalsDumpFrom(
		"", "", "", "Apple M3 Max", "macOS 15.5", "24.5.0", "",
	), &s.last); loadsOK {
		t.Fatal("load-less dump reported usable loads")
	}
	s.at = time.Now()
	s.Merge(&into)
	if into.Load1 != 1.5 || into.Load5 != 1.2 || into.Load15 != 0.9 {
		t.Errorf("absent remote loads clobbered local ones: %+v", into)
	}
	if into.CPUModel != "Apple M3 Max" {
		t.Errorf("present fields must still merge: %+v", into)
	}

	// Once the remote does publish loads, they overlay the locals.
	s.loadsValid = parseVitals(vitalsDumpFrom(
		"4.0 3.0 2.0", "", "", "Apple M3 Max", "macOS 15.5", "24.5.0", "",
	), &s.last)
	if !s.loadsValid {
		t.Fatal("dump with loads reported none")
	}
	s.at = time.Now()
	s.Merge(&into)
	if into.Load1 != 4.0 || into.Load5 != 3.0 || into.Load15 != 2.0 {
		t.Errorf("remote loads not merged: %+v", into)
	}
}

func TestStatsMergeFreshnessAndOverlay(t *testing.T) {
	var s Stats
	var into core.SysSample
	into.GPUs = []core.GPUDevice{{Vendor: "apple", Index: 0}}

	s.Merge(&into) // never polled: nothing changes
	if into.RemoteHost != "" || len(into.GPUs) != 1 {
		t.Fatalf("merge before poll changed sample: %+v", into)
	}

	parseVitals(vitalsDump, &s.last)
	s.at = time.Now().Add(-30 * time.Second) // stale
	s.Merge(&into)
	if into.RemoteHost != "" {
		t.Error("stale stats must be ignored")
	}

	s.at = time.Now()
	s.last.RemoteHost = "box"
	s.Merge(&into)
	if into.RemoteHost != "box" || into.CPUModel == "" || into.HostUptime <= 0 {
		t.Errorf("fresh merge missing vitals: %+v", into)
	}
	if len(into.GPUs) != 2 || into.GPUs[0].Vendor != "nvidia" {
		t.Errorf("remote GPUs must replace local ones: %+v", into.GPUs)
	}
}

// The merged sample is published to the UI while the next poll rewrites
// s.last.GPUs; aliasing that slice would data-race with a render.
func TestMergeCopiesGPUs(t *testing.T) {
	s := &Stats{
		at:   time.Now(),
		last: core.SysSample{GPUs: []core.GPUDevice{{Vendor: "nvidia", Name: "A"}}},
	}
	var into core.SysSample
	s.Merge(&into)
	s.last.GPUs[0].Name = "mutated"
	if into.GPUs[0].Name != "A" {
		t.Fatalf("Merge aliased GPU slice: %+v", into.GPUs)
	}
}

const rocmJSON = `{"card0":{"Temperature (Sensor edge) (C)":"52.0","GPU use (%)":"88","Used Memory (VRAM)":"12271640576","Total Memory (VRAM)":"17163091968"}}`

// vitalsDumpFrom builds a full vitals payload from ordered sections.
func vitalsDumpFrom(parts ...string) string {
	return strings.Join(parts, "\n"+sectionMark+"\n") + "\n"
}

func TestParseVitalsRocmGPUs(t *testing.T) {
	var s core.SysSample
	parseVitals(vitalsDumpFrom(
		"1.0 1.0 1.0",
		"", // meminfo absent
		"",
		"",
		"",
		"",
		rocmJSON,
	), &s)
	if len(s.GPUs) != 1 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}
	g := s.GPUs[0]
	if g.Vendor != "amd" || g.Index != 0 || g.MilliC != 52000 || g.UtilPct != 88 ||
		g.MemUsed == 0 || g.MemTotal == 0 {
		t.Errorf("rocm gpu = %+v", g)
	}
}

func TestSplitSectionsConsecutiveEmpty(t *testing.T) {
	secs := splitSections("\n" + sectionMark + "\n" + sectionMark + "\ndata\n" + sectionMark + "\n")
	// lines: "", MARK, MARK, "data", MARK, "" -> four sections incl. trailing
	if len(secs) != 4 {
		t.Fatalf("sections = %q", secs)
	}
	if strings.TrimSpace(secs[2]) != "data" {
		t.Errorf("section 2 = %q, want data", secs[2])
	}
}

// Run must actually poll over the wire: against the in-process sshd it
// samples real host vitals via the vitals script, stamps freshness so Merge
// accepts them, tags the host, and stops promptly on cancel.
func TestRunPollsAndMergesRemoteVitals(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	s := &Stats{Client: cli}
	var into core.SysSample
	s.Merge(&into) // never polled: must not touch the sample
	if into.RemoteHost != "" {
		t.Fatal("merge before any poll changed the sample")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx, 10*time.Millisecond) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		polled := !s.at.IsZero()
		s.mu.Unlock()
		if polled {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("poll never recorded a successful vitals sample")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	s.Merge(&into)
	if into.RemoteHost != "127.0.0.1" {
		t.Errorf("merged sample not tagged with remote host: %+v", into)
	}
	if runtime.GOOS == "linux" && into.MemTotal == 0 {
		t.Errorf("linux remote must yield memory vitals: %+v", into)
	}
}
