package remote

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
)

// Discovery is the result of one sweep over a remote host.
type Discovery struct {
	// Listening holds every port answering on the target, read from
	// /proc/net/tcp(+6). A read that fails or yields nothing falls back to
	// actively probing the well-known ports over the shell.
	Listening []int
	// EnginePorts holds ports inferred from engine-looking processes: their
	// --port flag when present, otherwise the engine's default. These catch
	// engines bound to custom ports the well-known list misses.
	EnginePorts []int
}

// Discover sweeps a remote host for inference engines over an established
// connection.
func Discover(ctx context.Context, c *Client, wellKnown []int) (*Discovery, error) {
	d := &Discovery{}

	// A sweep that could not read /proc/net/tcp is not fatal: hardened kernels
	// hide it from unprivileged readers and the active probe below covers the
	// gap. The reason is still logged: without it, a sweep that never read a
	// listening port is indistinguishable from a host that has none.
	//
	// The script ends in `true` so a host with no /proc/net/tcp6 still exits
	// 0, which means its exit status cannot report the failure this line
	// exists to name. The script marks the unreadable case on stdout instead.
	if out, err := c.Run(ctx, netTCPScript); err != nil {
		audit().Warn("toktop: remote listening-port sweep failed, falling back to an active probe",
			"error", core.RedactHome(core.Snippet([]byte(err.Error()))))
	} else if strings.Contains(out, noProcNetTCPMarker) {
		audit().Warn("toktop: remote listening-port sweep could not read /proc/net/tcp, falling back to an active probe")
	} else {
		d.Listening = parseNetTCP(out)
	}
	if len(d.Listening) == 0 {
		out, err := c.Run(ctx, probeScript(wellKnown))
		if err != nil {
			return nil, fmt.Errorf("port probe failed: %w", err) // unreachable host: nothing else will work either
		}
		for f := range strings.FieldsSeq(out) {
			if p, err := strconv.Atoi(f); err == nil && p > 0 {
				d.Listening = append(d.Listening, p)
			}
		}
	}

	// Engine-port hints are optional: a failing or missing cmdline sweep just
	// means custom-port engines are not pre-discovered; the listening-port
	// sweep above still drives the tunnel. Logged, because an engine bound
	// to a custom port then never appears and the only symptom is its
	// absence from the dashboard.
	if out, err := c.Run(ctx, procScanScript()); err != nil {
		audit().Warn("toktop: remote engine scan failed; engines on custom ports will not be discovered",
			"error", core.RedactHome(core.Snippet([]byte(err.Error()))))
	} else {
		d.EnginePorts = enginePorts(parseProcScan(out))
	}
	return d, nil
}

// ForwardSet returns the ports worth tunneling: engine hints always, plus
// well-known-candidate ports that are actually listening. Random other
// listeners (sshd, printers) are ignored.
func (d *Discovery) ForwardSet(wellKnown []int) []int {
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range d.EnginePorts {
		add(p)
	}
	wk := map[int]bool{}
	for _, p := range wellKnown {
		wk[p] = true
	}
	for _, p := range d.Listening {
		if wk[p] {
			add(p)
		}
	}
	slices.Sort(out)
	return out
}

// noProcNetTCPMarker is printed by netTCPScript when /proc/net/tcp could not
// be read at all. A line of its own, so parseNetTCP skips it: it is neither a
// header nor a socket row.
const noProcNetTCPMarker = "__no_proc_net_tcp__"

// netTCPScriptFor dumps the kernel's TCP tables at the given paths. It ends in
// `true` so a host with no /proc/net/tcp6 (or with one this account may not
// read) still exits 0 and the sibling sweep is not lost with it, so the exit
// status carries no information about whether the sweep worked at all. The
// marker carries it instead: without it, a hardened kernel that hides
// /proc/net/tcp from unprivileged readers produces empty output and a success,
// and the run continues as if the host had no listening port.
func netTCPScriptFor(tcp, tcp6 string) string {
	return `(r=0; cat ` + tcp + ` 2>/dev/null || r=1; cat ` + tcp6 + ` 2>/dev/null; [ "$r" = 1 ] && echo ` + noProcNetTCPMarker + `; true)`
}

var netTCPScript = netTCPScriptFor("/proc/net/tcp", "/proc/net/tcp6")

// parseNetTCP extracts listening ports (state 0A) from /proc/net/tcp text.
// The local address column is hex like 0100007F:2CA6. Ports are parsed as
// 16-bit: a wider reading would let a hostile remote plant impossible
// listeners that later drive the tunnel set.
func parseNetTCP(out string) []int {
	seen := map[int]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue // header
		}
		if f[3] != "0A" { // TCP_LISTEN
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || p == 0 { // port 0 is not a listener anything can reach
			continue
		}
		seen[int(p)] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// probeScript prints listening ports from the given candidate list. Uses bash
// /dev/tcp first, falling back to nc. It is the fallback for hosts where
// /proc/net/tcp is not readable.
//
// The remote side runs under the login shell of whatever account the key
// belongs to, which is only bash on some of the machines this connects to
// (dash and zsh are the common alternatives). /dev/tcp is a bash extension, so
// it is used only when bash is the shell and nc(1) is the fallback everywhere.
// Offering the bash branch to a shell without it costs a failed open per
// candidate port on every probe, and the sibling sweep above claims the remote
// scripts stay generic POSIX, which this one then would not.
func probeScript(ports []int) string {
	var b strings.Builder
	b.WriteString(`ports="`)
	for i, p := range ports {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(strconv.Itoa(p))
	}
	b.WriteString(`"`)
	b.WriteString(`
if [ -n "${BASH_VERSION:-}" ]; then
  for p in $ports; do
    (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null && echo "$p"
  done
elif command -v nc >/dev/null 2>&1; then
  for p in $ports; do
    nc -z -w 1 127.0.0.1 "$p" >/dev/null 2>&1 && echo "$p"
  done
fi
exit 0`)
	return b.String()
}

// procScanScript dumps "pid argv..." for every readable /proc/PID/cmdline,
// control characters flattened to spaces so each process is one line.
// Matching happens locally in Go; the remote side stays generic POSIX.
// Each command line is cut to procs.CmdlinePrefix bytes: a match cannot
// read past that (see procs.CmdlinePrefix), while the tail is exactly where
// an agent's inline prompt, a home directory or a credential on the command
// line sits. Nothing after the cut crosses the ssh connection. A --port
// beyond the cut is not seen, which costs the same engine hint a failed
// cmdline sweep already costs. The trailing exit 0 keeps a vanished/empty
// final cmdline (kernel threads race constantly) from failing the whole
// command despite good output.
//
// LC_ALL=C is what makes that bound bytes rather than characters. POSIX cut -c
// counts characters, so under the login locale of a UTF-8 peer a command line
// of 4096 emoji keeps all 4096 of them and ships 16 KiB per process, four
// times the documented budget, for every process in the table. The C locale
// also makes the tr range below a byte range, which is the only reading of
// '\000-\037' that does not mangle the continuation bytes of a multi-byte
// character.
func procScanScript() string {
	return `LC_ALL=C; export LC_ALL
for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$$" ] && continue
  c=$(tr '\000-\037\177' '   ' <"$d/cmdline" 2>/dev/null | cut -c 1-` + strconv.Itoa(procs.CmdlinePrefix) + `) || continue
  [ -n "$c" ] && printf '%s %s\n' "$p" "$c"
done
exit 0`
}

// parseProcScan turns procScanScript output into Infos. Argv splitting by
// spaces is lossy for quoted arguments, which is acceptable: both consumers
// (engine matching, --port extraction) scan tokens rather than exact paths.
// The sweep already cut every line, so the clip here is the same bound in
// bytes rather than characters: an Info retains the prefix and nothing past
// it, exactly as a local listing holds one.
func parseProcScan(out string) []procs.Info {
	var infos []procs.Info
	for raw := range strings.SplitSeq(out, "\n") {
		// The sweep's cut is a byte cut (procScanScript pins LC_ALL=C), so
		// the last character of a clipped command line can be the leading
		// bytes of a multi-byte one: a process whose argument past
		// CmdlinePrefix starts with an emoji, a CJK word or a combining
		// mark ships a line ending in half a character. Every other text
		// field in this program is valid UTF-8 before anything reads it
		// (core.SanitizeText drops ill-formed bytes for the same reason),
		// and a half a character is not one: a fold that walks runes
		// rewrites it to U+FFFD, so the same process is matched under a
		// different spelling on the far side of a fold than this side.
		fields := strings.Fields(strings.ToValidUTF8(raw, ""))
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		infos = append(infos, procs.Info{PID: pid, Name: fields[1], Args: procs.ClipArgs(fields[1:])})
	}
	return infos
}

// enginePorts maps engine-matched processes to their listen ports.
func enginePorts(infos []procs.Info) []int {
	seen := map[int]bool{}
	var out []int
	for _, i := range infos {
		_, def, ok := procs.MatchEngine(i)
		if !ok {
			continue
		}
		port := procs.ExtractPort(i.Args)
		if port == 0 {
			port = def
		}
		if port > 0 && !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	slices.Sort(out)
	return out
}
