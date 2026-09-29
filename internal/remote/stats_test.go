package remote

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

const vitalsDump = `%toktop%loadavg
3.10 2.20 1.05 4/900 12345
%toktop%meminfo
MemTotal:       16096680 kB
MemAvailable:   8000000 kB
SwapTotal:       2000000 kB
SwapFree:        1000000 kB
%toktop%uptime
183729.42 712000.11
%toktop%cpu
AMD Ryzen 9 7950X 16-Core Processor
%toktop%os
"Debian GNU/Linux 12 (bookworm)"
%toktop%kernel
6.1.0-18-amd64
%toktop%gpu
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
	parseVitals(vitalsDumpFrom("", "", "", "", "", "", ""), &s)
	if s.CPUModel != "keep me" || s.OsName != "" || s.Kernel != "" || len(s.GPUs) != 0 {
		t.Errorf("empty sections must not clobber: %+v", s)
	}
}

// The retained sample is parsed into in place, so a section the remote stops
// reporting has to clear what the previous poll recorded. A driver that is
// unloaded, a vendor CLI uninstalled, or a remote turned into a VM all answer
// with an empty GPU section while the connection stays healthy, and without
// this the stale card and its version sit on the dashboard for the rest of
// the run.
func TestParseVitalsEmptyGPUSectionClears(t *testing.T) {
	s := core.SysSample{}
	parseVitals(vitalsDump, &s)
	if len(s.GPUs) == 0 || s.Drivers["nvidia"] == "" {
		t.Fatalf("fixture did not record a GPU: %+v", s)
	}
	parseVitals(vitalsDumpFrom("1.0 1.0 1.0", "", "", "keep me", "", "", ""), &s)
	if len(s.GPUs) != 0 {
		t.Errorf("gpus survived an empty GPU section: %+v", s.GPUs)
	}
	if len(s.Drivers) != 0 {
		t.Errorf("drivers survived an empty GPU section: %v", s.Drivers)
	}
	if s.CPUModel != "keep me" {
		t.Errorf("a present non-empty section must still land: %q", s.CPUModel)
	}
}

// The other half of the rule: a dump that stopped before the last marker
// never reached the vendor CLI, so the last good reading stands.
func TestParseVitalsTruncatedDumpKeepsGPUs(t *testing.T) {
	s := core.SysSample{}
	parseVitals(vitalsDump, &s)
	// Five sections, the last of them named kernel: the GPU section never
	// arrives, so the run is treated as cut short.
	parseVitals(vitalsDumpFrom("1.0 1.0 1.0", "", "", "", "", "6.1.0-18-amd64"), &s)
	if len(s.GPUs) == 0 || s.Drivers["nvidia"] == "" {
		t.Errorf("a truncated dump must not drop the last GPU: %+v", s)
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

	// The stale path must be able to change something: seed the fields the
	// freshness check guards, or "unchanged" is what an empty sample already
	// satisfies and a Merge that ignored the window entirely would pass.
	parseVitals(vitalsDump, &s.last)
	s.last.RemoteHost = "box"
	s.at = time.Now().Add(-30 * time.Second) // stale
	s.Merge(&into)
	if into.RemoteHost != "" || len(into.GPUs) != 1 {
		t.Errorf("stale stats must be ignored: %+v", into)
	}

	s.at = time.Now()
	s.Merge(&into)
	if into.RemoteHost != "box" || into.CPUModel == "" || into.HostUptime <= 0 {
		t.Errorf("fresh merge missing vitals: %+v", into)
	}
	if len(into.GPUs) != 2 || into.GPUs[0].Vendor != "nvidia" {
		t.Errorf("remote GPUs must replace local ones: %+v", into.GPUs)
	}
}

// The staleness window runs on the injected clock: with a stepped clock a
// sample stays fresh for the window's worth of simulated time and expires on
// the step past it, whatever the wall clock does.
func TestMergeFreshnessFollowsInjectedClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := &Stats{last: core.SysSample{RemoteHost: "box"}}
	s.SetNow(func() time.Time { return now })

	var into core.SysSample
	s.at = now
	s.Merge(&into)
	if into.RemoteHost != "box" {
		t.Fatalf("fresh sample not merged: %+v", into)
	}

	now = now.Add(stalenessWindow - time.Second)
	into = core.SysSample{}
	s.Merge(&into)
	if into.RemoteHost != "box" {
		t.Errorf("sample one second short of the window dropped: %+v", into)
	}

	now = now.Add(2 * time.Second)
	into = core.SysSample{}
	s.Merge(&into)
	if into.RemoteHost != "" {
		t.Errorf("sample past the window merged anyway: %+v", into)
	}
}

// --target is repeatable and every target's Merge overlays the same sample,
// so a healthy target merging after a failing one used to clear that
// failure: a dead host then had no symptom at all, and the header named a
// host that was answering. The failing target keeps the slot until it
// recovers on its own.
func TestMergeOneTargetDoesNotClearAnothersFailure(t *testing.T) {
	down := &Stats{at: time.Now(), err: "connection refused", last: core.SysSample{RemoteHost: "box"}}
	up := &Stats{at: time.Now(), last: core.SysSample{RemoteHost: "rack", CPUModel: "Xeon"}}

	var into core.SysSample
	into.MemTotal = 1 << 30 // local readings the remotes overlay
	down.Merge(&into)
	up.Merge(&into)
	if into.RemoteHost != "box" || into.RemoteErr == "" {
		t.Errorf("a healthy target cleared a failing one's error: host=%q err=%q",
			into.RemoteHost, into.RemoteErr)
	}
	// The healthy target's own vitals still land: only the error slot is
	// owned, not the whole sample.
	if into.CPUModel != "Xeon" {
		t.Errorf("healthy target's vitals were dropped: %+v", into)
	}

	// box comes back: its own banner lifts, without touching rack's.
	down.err = ""
	down.Merge(&into)
	if into.RemoteErr != "" {
		t.Errorf("recovered target still reporting: host=%q err=%q", into.RemoteHost, into.RemoteErr)
	}
}

// A sample no failing target owns names the host that answered it, so the
// header's "via ssh:" line keeps naming a host that is reachable.
func TestMergeNamesTheHostThatAnswered(t *testing.T) {
	up := &Stats{at: time.Now(), last: core.SysSample{RemoteHost: "rack"}}
	var into core.SysSample
	up.Merge(&into)
	if into.RemoteHost != "rack" || into.RemoteErr != "" {
		t.Errorf("healthy target did not name itself: host=%q err=%q", into.RemoteHost, into.RemoteErr)
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

// A remote sample is labelled with the host it came from, so no field may
// survive the merge that the local sampler read from this machine.
func TestMergeDropsLocalOnlyReadings(t *testing.T) {
	s := &Stats{
		at:   time.Now(),
		last: core.SysSample{RemoteHost: "box", CPUModel: "Xeon", OsName: "Debian"},
	}
	into := core.SysSample{
		Temps:   []core.TempReading{{Label: "package", MilliC: 62000}},
		NPUs:    []string{"acme"},
		Drivers: map[string]string{"local": "1.0"},
	}
	s.Merge(&into)
	if len(into.Temps) != 0 || len(into.NPUs) != 0 {
		t.Fatalf("local-only readings survived a remote merge: %+v", into)
	}
	if into.CPUModel != "Xeon" || into.OsName != "Debian" {
		t.Fatalf("present remote fields must still merge: %+v", into)
	}
}

// Drivers belong to the devices they were read from, so a remote's map
// replaces the local one rather than joining it, and leaves it alone when the
// remote reported no devices at all.
func TestMergeReplacesDriversWithTheRemoteGPUs(t *testing.T) {
	into := core.SysSample{Drivers: map[string]string{"local": "1.0"}}
	s := &Stats{at: time.Now(), last: core.SysSample{GPUs: []core.GPUDevice{{Vendor: "amd"}}}}
	s.Merge(&into)
	if len(into.Drivers) != 0 {
		t.Fatalf("local drivers kept beside remote GPUs: %+v", into.Drivers)
	}

	withDrivers := &Stats{
		at: time.Now(),
		last: core.SysSample{
			GPUs:    []core.GPUDevice{{Vendor: "nvidia"}},
			Drivers: map[string]string{"nvidia": "550.1"},
		},
	}
	into = core.SysSample{Drivers: map[string]string{"local": "1.0"}}
	withDrivers.Merge(&into)
	if len(into.Drivers) != 1 || into.Drivers["nvidia"] != "550.1" {
		t.Fatalf("remote drivers not merged: %+v", into.Drivers)
	}

	noGPU := &Stats{at: time.Now(), last: core.SysSample{}}
	into = core.SysSample{Drivers: map[string]string{"local": "1.0"}, GPUs: []core.GPUDevice{{Vendor: "local"}}}
	noGPU.Merge(&into)
	if into.Drivers["local"] != "1.0" || len(into.GPUs) != 1 {
		t.Fatalf("a target with no GPUs disturbed the local pair: %+v", into)
	}
}

const rocmJSON = `{"card0":{"Temperature (Sensor edge) (C)":"52.0","GPU use (%)":"88","Used Memory (VRAM)":"12271640576","Total Memory (VRAM)":"17163091968"}}`

// sectionNames is the order vitalsScript writes its sections, which is the
// order vitalsDumpFrom's parts are supplied in, and the full set parseVitals
// reads: TestScriptSectionsCoverParser holds the script and the parser to it
// both ways, so a second list of the section names would be a list that can
// drift from both.
var sectionNames = []string{secLoadavg, secMeminfo, secUptime, secCPU, secOS, secKernel, secGPU}

// vitalsDumpFrom builds a full vitals payload, one named section per part, in
// the order the script writes them.
func vitalsDumpFrom(parts ...string) string {
	var b strings.Builder
	for i, part := range parts {
		b.WriteString(sectionMark)
		b.WriteString(sectionNames[i])
		b.WriteString("\n")
		b.WriteString(part)
		b.WriteString("\n")
	}
	return b.String()
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
	secs := splitSections("\n" + sectionMark + secMeminfo + "\n" + sectionMark + secCPU +
		"\ndata\n" + sectionMark + secKernel + "\n")
	// A named section with an empty body is present and empty, which is how
	// parseVitals tells "the remote reported no GPU" from "the dump stopped
	// before the GPU section".
	if body, ok := secs[secMeminfo]; !ok || strings.TrimSpace(body) != "" {
		t.Errorf("meminfo = %q, present = %t; want a present empty section", body, ok)
	}
	if body := strings.TrimSpace(secs[secCPU]); body != "data" {
		t.Errorf("cpu = %q, want data", body)
	}
	if body, ok := secs[secKernel]; !ok || strings.TrimSpace(body) != "" {
		t.Errorf("kernel = %q, present = %t; want a present empty section", body, ok)
	}
	if len(secs) != 3 {
		t.Errorf("sections = %q, want the three named ones and nothing else", secs)
	}
}

// A section is read by name, so the order the script writes them in carries
// no meaning. This is the property that makes the two sides of the dump
// independent: a section added, removed or moved in the script changes one
// field rather than shifting every field after it into the neighbour's slot.
func TestParseVitalsIgnoresSectionOrder(t *testing.T) {
	scrambled := sectionMark + secKernel + "\n6.1.0-18-amd64\n" +
		sectionMark + secOS + "\n\"Debian GNU/Linux 12 (bookworm)\"\n" +
		sectionMark + secCPU + "\nAMD Ryzen 9 7950X 16-Core Processor\n" +
		sectionMark + secUptime + "\n183729.42 712000.11\n" +
		sectionMark + secMeminfo + "\nMemTotal:       16096680 kB\n" +
		sectionMark + secLoadavg + "\n3.10 2.20 1.05 4/900 12345\n"

	var s core.SysSample
	if loadsOK := parseVitals(scrambled, &s); !loadsOK {
		t.Fatal("scrambled dump reported no loads")
	}
	if s.Load1 != 3.10 || s.CPUModel != "AMD Ryzen 9 7950X 16-Core Processor" ||
		s.OsName != "Debian GNU/Linux 12 (bookworm)" || s.Kernel != "6.1.0-18-amd64" {
		t.Errorf("fields landed in the wrong sections: %+v", s)
	}
	if want := uint64(16096680) << 10; s.MemTotal != want {
		t.Errorf("memtotal = %d want %d", s.MemTotal, want)
	}
}

// The dump is a protocol between a shell script and a parser, and this is the
// only thing that holds the two together: a section the script writes and the
// parser never reads is a field that silently stops arriving, and nothing
// else in the build would say so. A section the parser reads and the script
// never writes is the same failure pointing the other way.
func TestScriptSectionsCoverParser(t *testing.T) {
	written := map[string]bool{}
	for line := range strings.SplitSeq(vitalsScript(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "echo "+sectionMark); ok {
			if name = strings.TrimSpace(name); name != "" {
				written[name] = true
			}
		}
	}
	read := map[string]bool{}
	for _, name := range sectionNames {
		read[name] = true
	}
	if len(read) != len(sectionNames) {
		t.Fatalf("sectionNames repeats a section: %v", sectionNames)
	}
	for name := range read {
		if !written[name] {
			t.Errorf("parseVitals reads the %q section, which vitalsScript never writes", name)
		}
	}
	for name := range written {
		if !read[name] {
			t.Errorf("vitalsScript writes a %q section, which parseVitals never reads", name)
		}
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

// A remote that stops answering must name itself and the reason. Dropping
// the ssh readings without a word leaves the local host's numbers on screen
// passing for the watched one's.
func TestPollFailureNamesTheTarget(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	s := &Stats{Client: cli}
	s.poll(t.Context())
	s.Merge(&s.last) // warm the sample so the failure has a host to drop away from
	if s.err != "" {
		t.Fatalf("successful poll recorded a failure: %q", s.err)
	}

	cli.Close()
	s.poll(t.Context())
	if s.err == "" {
		t.Fatal("failed poll recorded no reason")
	}

	// Past the staleness window the vitals are gone, so the reason is the
	// only thing left that says a remote was ever configured.
	s.at = s.instant().Add(-stalenessWindow - time.Second)
	var into core.SysSample
	s.Merge(&into)
	if into.RemoteHost != "127.0.0.1" {
		t.Errorf("failing target not named: %+v", into)
	}
	if into.RemoteErr == "" {
		t.Errorf("failing target reported no reason: %+v", into)
	}
	if into.CPUModel != "" {
		t.Errorf("stale vitals merged anyway: %+v", into)
	}
}

// The host-identity strings come off the peer, and firstLine bounds them by
// nothing but the line the peer sent: a 5 MB PRETTY_NAME would sit in the
// snapshot, be re-sanitized by every renderer on every frame and be written
// whole into --json. Each field is capped at core.ModelNameMax clusters, and the
// cap counts clusters, so a multi-byte name is cut between characters and an
// escape sequence cannot survive.
func TestParseVitalsBoundsPeerSuppliedFields(t *testing.T) {
	huge := strings.Repeat("z", core.ModelNameMax*3)
	var s core.SysSample
	parseVitals(vitalsDumpFrom(
		"1.0 1.0 1.0", "", "",
		huge+"\x1b[31m",
		`"""`+huge+"\x1b]0;pwned\x07",
		strings.Repeat("é", core.ModelNameMax)+huge,
	), &s)

	for _, f := range []struct {
		name, got string
	}{
		{"CPUModel", s.CPUModel},
		{"OsName", s.OsName},
		{"Kernel", s.Kernel},
	} {
		if n := len([]rune(f.got)); n > core.ModelNameMax {
			t.Errorf("%s is %d characters, want at most %d", f.name, n, core.ModelNameMax)
		}
		if strings.ContainsAny(f.got, "\n\x1b\x07") {
			t.Errorf("%s kept a control byte: %q", f.name, f.got)
		}
	}
	if s.CPUModel != strings.Repeat("z", core.ModelNameMax) {
		t.Errorf("CPUModel = %d characters, want the cap %d", len([]rune(s.CPUModel)), core.ModelNameMax)
	}
}

// The remote sampler is paced by a seam, not by a ticker of its own, so a
// simulated run decides which polls answer. Left on the wall clock it sampled
// a remote once per elapsed interval while stamping the samples on the
// injected clock, and the same seed merged a different number of remote
// readings every time. A nil pacer restores the wall clock, and Run ends on
// the context with either.
func TestStatsRunIsSteppableByADriver(t *testing.T) {
	s := &Stats{}
	if got := s.pacer(); got != core.WallPacer {
		t.Fatalf("pacer() = %#v, want core.WallPacer", got)
	}
	pace := core.NewVirtualPacer()
	s.SetPacer(pace)
	if got := s.pacer(); got != pace {
		t.Fatalf("pacer() = %#v, want the pacer just set", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan struct{})
	go func() { defer close(returned); s.Run(ctx, time.Hour) }()
	pace.Fire(time.Unix(1_700_000_000, 0))
	cancel()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the context was canceled")
	}

	s.SetPacer(nil)
	if got := s.pacer(); got != core.WallPacer {
		t.Fatalf("pacer() = %#v after a nil pacer, want core.WallPacer", got)
	}
}
