package remote

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseVitals throws arbitrary bytes at parseVitals, the entry point for
// everything an SSH remote sends back from the vitals script: loadavg,
// meminfo, uptime, CPU/OS/kernel strings and GPU telemetry (nvidia-smi CSV
// or rocm-smi JSON). A hostile remote must not be able to panic the
// dashboard or plant impossible state in a sample: saturating memory math,
// non-negative sensor readings and load-validity reporting are asserted.
func FuzzParseVitals(f *testing.F) {
	for _, seed := range []string{
		vitalsDump,
		vitalsDumpFrom("1.0 1.0 1.0", "", "", "", "", "", rocmJSON),
		vitalsDumpFrom("", "", "", "", "", "", ""),
		"\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n%toktop%\n",
		"",
		"%toktop%",
		"nan nan nan\n%toktop%MemTotal: x\n%toktop%-1e999\n%toktop%%toktop%%toktop%%toktop%\n-1, , [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]",
		"1e300 0 0",
		"MemTotal: 99999999999999999999 kB\nMemAvailable: 1 kB\nSwapTotal: 5 kB\nSwapFree: 9 kB",
		`{"card0":{"Temperature (Sensor edge) (C)":"1e308","GPU use (%)":"nan","Used Memory (VRAM)":[1e308],"Total Memory (VRAM)":{"values":[-42]}}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var s core.SysSample
		loadsOK := parseVitals(string(data), &s)

		if loadsOK != (s.Load1 > 0 || s.Load5 > 0 || s.Load15 > 0) {
			t.Fatalf("loadsOK=%v but loads = %v %v %v", loadsOK, s.Load1, s.Load5, s.Load15)
		}
		if s.MemUsed > s.MemTotal {
			t.Fatalf("mem used %d exceeds total %d", s.MemUsed, s.MemTotal)
		}
		if s.SwapUsed > s.SwapTotal {
			t.Fatalf("swap used %d exceeds total %d", s.SwapUsed, s.SwapTotal)
		}
		for i, g := range s.GPUs {
			if g.UtilPct < 0 || g.PowerW < 0 {
				t.Fatalf("gpu[%d] negative sensor reading: %+v", i, g)
			}
		}
	})
}

// FuzzParseDiscoveryOutput throws arbitrary bytes at the remote-discovery
// parsers: parseNetTCP and parseProcScan read whatever a hostile SSH host
// prints for the /proc sweeps, and enginePorts plus ForwardSet turn that into
// the tunnel set. Nothing a remote prints may plant an impossible port (a
// tunnel target outside the TCP range is a broken connection at best), lists
// must stay sorted and duplicate-free, and parsing is deterministic.
func FuzzParseDiscoveryOutput(f *testing.F) {
	for _, seed := range []string{
		netTCPSample,
		netTCPSample + "   9: 0100007F:FFFFF 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 ffff8ba0c4a48000 100 0 0 10 1\n",
		"   0: 0100007F:10000 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 f 100 0 0 10 1\n",
		"1 llama-server --port=99999999\n2 ollama serve\n3 python -m vllm.entrypoints.openai.api_server --port -7\n",
		"102 python -m vllm.entrypoints.openai.api_server --port 9911\n103 \nnotapid junk\n",
		"2147483647 x --port 65535\n-5 ollama serve\n0 proc\n",
		"garbage\n\n0:\n x:y z A\n::::\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out := string(data)

		listening := parseNetTCP(out)
		assertPorts(t, "listening", listening)
		if again := parseNetTCP(out); !slices.Equal(again, listening) {
			t.Fatal("parseNetTCP is not deterministic")
		}

		var d Discovery
		d.Listening = listening
		d.EnginePorts = assertPorts(t, "engine", enginePorts(parseProcScan(out)))
		assertPorts(t, "forward set", d.ForwardSet([]int{11434, 8080, 3000}))

		for _, info := range parseProcScan(out) {
			if info.PID <= 0 {
				t.Fatalf("pid %d survived parseProcScan: %+v", info.PID, info)
			}
			if len(info.Args) == 0 {
				t.Fatalf("info %d has no argv: %+v", info.PID, info)
			}
		}
	})
}

// assertPorts fails unless every port is a number something can listen on and
// the list is strictly increasing, which is what sorted+deduped looks like.
func assertPorts(t *testing.T, which string, ports []int) []int {
	t.Helper()
	for i, p := range ports {
		if p < 1 || p > 65535 {
			t.Fatalf("%s[%d] = %d is not a port", which, i, p)
		}
		if i > 0 && p <= ports[i-1] {
			t.Fatalf("%s not sorted/deduped at %d: %v", which, i, ports)
		}
	}
	return ports
}

// FuzzParseTarget drives ParseTarget with arbitrary strings. ParseTarget parses
// user-provided CLI arguments and targets (ssh://[user@]host[:port]), validates
// against URL parsing injection, enforces port ranges (1..65535), forbids
// embedded passwords without leaking them in errors, forbids paths, queries,
// fragments, and integrates with ~/.ssh/config resolution.
func FuzzParseTarget(f *testing.F) {
	for _, seed := range []string{
		"ssh://gpu",
		"ssh://maci@192.168.0.211",
		"ssh://root@gpu-box:2222",
		"ssh://192.168.1.5",
		"ssh://maci@box/",
		"ssh://root@[::1]:22",
		"ssh://dev@cluster.internal:2222",
		"ssh://box.lab",
		"ssh://user:s3cret@box",
		"ssh://:s3cret@box",
		"ssh://user:@box",
		"ssh://box/opt/engines",
		"ssh://box?jump=1",
		"ssh://box#frag",
		"ssh://",
		"ssh://:22",
		"ssh://user@",
		"ssh://user@:22",
		"ssh://h:notaport",
		"ssh://h:-1",
		"ssh://h:0",
		"ssh://h:65536",
		"ssh://h:999999999999999999999999",
		"http://x",
		"gpu",
		"",
		"ssh://[fe80::1%eth0]:22",
		"ssh://\x00\xff",
		"ssh://user@host:22/path/sub",
		"ssh://user@host:22?query#frag",
		"ssh://user@host:22/",
		"ssh://user:pass@host:22",
		"ssh://user:pass@host/path",
		"ssh://[::1]:0",
		"ssh://[::1]:65535",
		"ssh://example.com:80",
	} {
		f.Add(seed)
	}

	oldPath, oldRead := sshConfigPath, configReader
	defer func() { sshConfigPath, configReader = oldPath, oldRead }()
	sshConfigPath = func() string { return "/test/config" }
	configReader = func(path string) ([]byte, error) {
		return []byte("Host gpu\n  hostname 192.168.0.212\n  user maci\n  port 2022\n  identityfile ~/.ssh/gpu_key\nHost *.lab\n  user labadmin\nHost *\n  user fallback\n"), nil
	}

	f.Fuzz(func(t *testing.T, raw string) {
		tgt, err := ParseTarget(raw)
		if err == nil {
			if tgt.Host == "" {
				t.Fatalf("ParseTarget(%q) succeeded with empty host: %+v", raw, tgt)
			}
			if tgt.Port < 1 || tgt.Port > 65535 {
				t.Fatalf("ParseTarget(%q) returned invalid port %d: %+v", raw, tgt.Port, tgt)
			}
			again, err2 := ParseTarget(raw)
			if err2 != nil || again != tgt {
				t.Fatalf("ParseTarget(%q) is not deterministic: (%+v, %v) vs (%+v, %v)", raw, tgt, err, again, err2)
			}
		} else if strings.HasPrefix(raw, "ssh://") {
			if u, parseErr := url.Parse(raw); parseErr == nil && u.User != nil {
				if pass, ok := u.User.Password(); ok && len(pass) > 8 {
					if !strings.Contains("ssh target must not contain a password; set TOKTOP_SSH_PASSWORD or use --ssh-key", pass) {
						if strings.Contains(err.Error(), pass) {
							t.Fatalf("ParseTarget(%q) leaked password in error %q", raw, err.Error())
						}
					}
				}
			}
		}
	})
}

// FuzzParseSSHConfig drives the ~/.ssh/config parser with arbitrary config files
// and host lookup keys. An attacker or corrupted config file must not trigger
// panics, infinite loops, or invalid ports outside 1..65535, and pattern
// matching with glob wildcards and negations must be deterministic.
func FuzzParseSSHConfig(f *testing.F) {
	for _, seed := range []struct {
		cfg  string
		host string
	}{
		{
			cfg: `
Host gpu
  hostname 192.168.0.212
  user maci
  port 2022
  identityfile ~/.ssh/gpu_key

Host *.lab !bad.lab
  user labadmin
  port 2222

Host *
  user fallback
`,
			host: "gpu",
		},
		{
			cfg:  "Host\tgpu\n\tHostName\t10.9.8.7\n\tPort\t2022\n",
			host: "gpu",
		},
		{
			cfg:  "Host = box\nHostName = 1.2.3.4\nUser = root\nPort = 22\nIdentityFile = ~/key\n",
			host: "box",
		},
		{
			cfg:  "Host a b c !d\n  user u\n",
			host: "b",
		},
		{
			cfg:  "Host *\n  port -1\n  port 0\n  port 65536\n  port 9999999999999999999999\n",
			host: "any",
		},
		{
			cfg:  "# comment line only\n\n",
			host: "gpu",
		},
		{
			cfg:  "Host [a-z*\n  user broken-glob\n",
			host: "a",
		},
		{
			cfg:  "Host *\n  identityfile ~\n  identityfile ~/relative\n  identityfile ~/.ssh/key\n",
			host: "x",
		},
		{
			cfg:  "Host !* \n user nobody\n",
			host: "x",
		},
		{
			cfg:  "",
			host: "",
		},
	} {
		f.Add([]byte(seed.cfg), seed.host)
	}

	f.Fuzz(func(t *testing.T, cfgBytes []byte, host string) {
		entry := parseSSHConfig(cfgBytes, host)
		if entry != nil {
			if entry.Port != 0 && (entry.Port < 1 || entry.Port > 65535) {
				t.Fatalf("parseSSHConfig returned out-of-range port %d: %+v", entry.Port, entry)
			}
			again := parseSSHConfig(cfgBytes, host)
			if again == nil || *again != *entry {
				t.Fatalf("parseSSHConfig is not deterministic: %+v vs %+v", entry, again)
			}
		}
	})
}
