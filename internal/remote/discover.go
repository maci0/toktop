package remote

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/procs"
)

// Discovery is the result of one sweep over a remote host.
type Discovery struct {
	// Listening holds every port answering on the target, read straight from
	// /proc/net/tcp(+6) so no bash/nc is needed remotely.
	Listening []int
	// EnginePorts holds ports inferred from engine-looking processes: their
	// --port flag when present, otherwise the engine's default. These catch
	// engines bound to custom ports the well-known list misses.
	EnginePorts []int
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

// Discover sweeps a remote host for inference engines over an established
// connection.
func Discover(ctx context.Context, c *Client, wellKnown []int) (*Discovery, error) {
	d := &Discovery{}

	// A failing /proc/net/tcp read is not fatal: hardened kernels hide it
	// from unprivileged readers and the active probe below covers the gap.
	if out, err := c.Run(ctx, netTCPScript); err == nil {
		d.Listening = parseNetTCP(out)
	}
	if len(d.Listening) == 0 {
		// Hardened kernels hide /proc/net/tcp from unprivileged readers; fall
		// back to actively probing the well-known ports through the shell.
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
	// sweep above still drives the tunnel.
	if out, err := c.Run(ctx, procScanScript()); err == nil {
		d.EnginePorts = enginePorts(parseProcScan(out))
	}
	return d, nil
}

const netTCPScript = "(cat /proc/net/tcp 2>/dev/null; cat /proc/net/tcp6 2>/dev/null; true)"

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

// ProbeScript prints listening ports from the given candidate list. Uses bash
// /dev/tcp first, falling back to nc. Kept as a fallback for hosts where
// /proc/net/tcp is not readable.
func probeScript(ports []int) string {
	var b strings.Builder
	b.WriteString("for p in")
	for _, p := range ports {
		b.WriteString(" " + strconv.Itoa(p))
	}
	b.WriteString(`; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"
  elif command -v nc >/dev/null 2>&1 && nc -z -w1 127.0.0.1 "$p" >/dev/null 2>&1; then echo "$p"
  fi
done`)
	return b.String()
}

// procScanScript dumps "pid argv..." for every readable /proc/PID/cmdline,
// control characters flattened to spaces so each process is one line.
// Matching happens locally in Go; the remote side stays generic POSIX.
// The trailing exit 0 keeps a vanished/empty final cmdline (kernel threads
// race constantly) from failing the whole command despite good output.
func procScanScript() string {
	return `for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$$" ] && continue
  c=$(tr '\000-\037' '  ' <"$d/cmdline" 2>/dev/null) || continue
  [ -n "$c" ] && printf '%s %s\n' "$p" "$c"
done
exit 0`
}

// parseProcScan turns procScanScript output into Infos. Argv splitting by
// spaces is lossy for quoted arguments, which is acceptable: both consumers
// (engine matching, --port extraction) scan tokens rather than exact paths.
func parseProcScan(out string) []procs.Info {
	var infos []procs.Info
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		infos = append(infos, procs.Info{PID: pid, Name: fields[1], Args: fields[1:]})
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
