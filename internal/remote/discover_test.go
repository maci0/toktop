package remote

import (
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/procs"
)

const netTCPSample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:2CAA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 ffff8ba0c4a48000 100 0 0 10 1
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 23456 1 ffff8ba0c4a48800 100 0 0 10 1
   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9876 1 ffff8ba0c4a49000 100 0 0 10 2
   3: 0100007F:C350 0100007F:C351 01 00000000:00000000 00:00000000 00000000  1000        0 34567 1 ffff8ba0c4a49800 20 4 30 10 -1
`

func TestParseNetTCP(t *testing.T) {
	// 2CA6=11434, 1F90=8080, 0016=22 listening; the ESTABLISHED row is skipped.
	got := parseNetTCP(netTCPSample)
	want := []int{22, 8080, 11434}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNetTCP = %v, want %v", got, want)
	}
}

func TestParseNetTCP6(t *testing.T) {
	out := "  sl  local_address remote_address                         st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000000000000000000000000000:1F92 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 777 1 f 100 0 0 10 1\n"
	got := parseNetTCP(out)
	if len(got) != 1 || got[0] != 8082 { // 1F92 = 8082
		t.Errorf("parseNetTCP6 = %v, want [8082]", got)
	}
}

func TestParseProcScan(t *testing.T) {
	out := "101 /usr/bin/ollama serve\n" +
		"102 python -m vllm.entrypoints.openai.api_server --port 9911\n" +
		"103 \n" + // empty cmdline -> dropped by script, tolerated here
		"notapid junk\n"
	infos := parseProcScan(out)
	if len(infos) != 2 {
		t.Fatalf("got %d infos: %+v", len(infos), infos)
	}
	if infos[0].PID != 101 || infos[0].Name != "/usr/bin/ollama" {
		t.Errorf("info[0] = %+v", infos[0])
	}
	ports := enginePorts(infos)
	// ollama default 11434; vllm hint --port 9911 beats its default 8000.
	if !reflect.DeepEqual(ports, []int{9911, 11434}) {
		t.Errorf("enginePorts = %v, want [9911 11434]", ports)
	}
}

func TestEnginePortsCustomFlagForms(t *testing.T) {
	infos := parseProcScan(
		"1 llama-server --port=9001\n" +
			"2 sglang.launch_server --http-port 9100\n" +
			"3 sglang serve --port 9200\n")
	if got := enginePorts(infos); !reflect.DeepEqual(got, []int{9001, 9100, 9200}) {
		t.Errorf("enginePorts = %v", got)
	}
}

func TestForwardSet(t *testing.T) {
	d := &Discovery{
		Listening:   []int{22, 3000, 5005, 8080},
		EnginePorts: []int{5005}, // custom-port engine
	}
	got := d.ForwardSet([]int{3000, 8080, 11434})
	if !reflect.DeepEqual(got, []int{3000, 5005, 8080}) {
		t.Errorf("ForwardSet = %v", got)
	}
}

func TestVitalsScript(t *testing.T) {
	s := vitalsScript()
	for _, want := range []string{
		"/proc/loadavg", "/proc/meminfo", "/proc/uptime",
		"kern.boottime",
		"/proc/cpuinfo", "/etc/os-release", "uname -r", "nvidia-smi",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("vitalsScript missing %q", want)
		}
	}
	// One named mark per section, the same set parseVitals reads.
	marks := strings.Count(s, "echo "+sectionMark)
	if marks != len(sectionNames) {
		t.Errorf("vitalsScript has %d section marks, want %d", marks, len(sectionNames))
	}
	if got := strings.Count(s, sectionMark); got != marks {
		t.Errorf("vitalsScript has %d sectionMark occurrences, want the %d that name a section", got, marks)
	}
}

// Discover must sweep a remote host end to end: the listening-port sweep
// (from the remote /proc tree, or the active probe where it is hidden) has
// to see the sshd's own port, since that listener demonstrably exists.
func TestDiscoverSweepsOverConnection(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	d, err := Discover(t.Context(), cli, []int{11434, srv.Port()})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var found bool
	for _, p := range d.Listening {
		if p == srv.Port() {
			found = true
		}
	}
	if !found {
		t.Errorf("listening = %v, want it to contain sshd port %d", d.Listening, srv.Port())
	}
}

// probeScript is the fallback when /proc/net/tcp is unreadable; pin its
// shape so a drift cannot silently break discovery on hardened hosts. The
// /dev/tcp branch is bash-only, so the gate on BASH_VERSION is part of what is
// pinned: without it a dash or zsh login shell pays a failed open per port.
func TestProbeScriptShape(t *testing.T) {
	s := probeScript([]int{11434, 8080})
	for _, want := range []string{
		`ports="11434 8080"`,
		`[ -n "${BASH_VERSION:-}" ]`,
		"/dev/tcp/127.0.0.1/$p",
		"nc -z",
		"exit 0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("probeScript missing %q:\n%s", want, s)
		}
	}
}

// The cmdline sweep carries every process's command line off the remote host,
// so it must ship no more than the prefix a match can read. Without the cut,
// a prompt typed after an agent's flags, a home directory or a key on the
// command line crosses the ssh connection unread.
func TestProcScanScriptCapsCmdline(t *testing.T) {
	s := procScanScript()
	cut := "cut -c 1-" + strconv.Itoa(procs.CmdlinePrefix)
	if !strings.Contains(s, cut) {
		t.Errorf("procScanScript does not cap the command line with %q:\n%s", cut, s)
	}
	// A line that ends inside an argument still parses: the matcher scans
	// tokens, so a clipped tail is the same shape a short command line has.
	infos := parseProcScan("42 " + strings.Repeat("x", procs.CmdlinePrefix+1))
	if len(infos) != 1 || len(infos[0].Args) == 0 {
		t.Fatalf("parseProcScan of a clipped line = %+v", infos)
	}
}

// The sweep's cut is a byte cut, so a line clipped inside a multi-byte
// character arrives holding half of one. A name and an argument that are not
// valid UTF-8 are not text: a fold walking runes rewrites them to U+FFFD, so
// the same process reads under two spellings depending on which side of the
// fold looked at it.
func TestParseProcScanDropsPartialCharacter(t *testing.T) {
	// "ollama run --model café-" with the last byte of a further "é" left
	// dangling, which is what a byte cut at that offset ships.
	infos := parseProcScan("42 ollama run --model caf\xc3\xa9-\xc3")
	if len(infos) != 1 {
		t.Fatalf("parseProcScan = %+v, want one info", infos)
	}
	i := infos[0]
	for _, s := range append([]string{i.Name}, i.Args...) {
		if !utf8.ValidString(s) {
			t.Errorf("field %q is not valid UTF-8", s)
		}
	}
	if i.Name != "ollama" {
		t.Errorf("Name = %q, want ollama", i.Name)
	}
	// Only the half character goes; every whole one before it survives.
	if !slices.Contains(i.Args, "café-") {
		t.Errorf("Args = %q, want the whole characters kept", i.Args)
	}
}

// The sweep script ends in `true` so a host with no /proc/net/tcp6 still exits
// 0, which means its exit status says nothing about whether the sweep worked.
// The marker line is what carries that: without it a hardened kernel that
// hides /proc/net/tcp from unprivileged readers yields empty output and
// success, the caller logs nothing, and a host full of engines is reported as
// a host with none listening.
func TestNetTCPScriptMarksAnUnreadableProcNetTCP(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	run := func(tcp, tcp6 string) string {
		t.Helper()
		out, err := exec.Command("sh", "-c", netTCPScriptFor(tcp, tcp6)).Output()
		if err != nil {
			t.Fatalf("sweep script (%s, %s): %v", tcp, tcp6, err)
		}
		return string(out)
	}

	const hidden = "/proc/toktop-does-not-exist/tcp"
	if out := run(hidden, hidden+"-6"); !strings.Contains(out, noProcNetTCPMarker) {
		t.Fatalf("an unreadable /proc/net/tcp printed no marker:\n%s", out)
	} else if got := parseNetTCP(out); len(got) != 0 {
		t.Errorf("parseNetTCP = %v, want none from an unreadable sweep", got)
	}

	// A host whose tcp table reads but whose tcp6 table is absent is a normal
	// IPv4-only host, not an unreadable sweep, so the marker must stay off.
	if out := run("/proc/net/tcp", "/proc/toktop-does-not-exist/tcp6"); strings.Contains(out, noProcNetTCPMarker) {
		t.Fatalf("a missing /proc/net/tcp6 was reported as an unreadable sweep:\n%s", out)
	}
}
