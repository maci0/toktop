//go:build darwin

package procs

import (
	"context"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

func init() {
	platformList = listDarwin
}

// kibToBytes converts a ps(1) RSS column (KiB) to bytes, saturating so an
// absurd value cannot wrap to a small byte count in the shift.
func kibToBytes(kib uint64) uint64 {
	const capKib = ^uint64(0) >> 10
	if kib > capKib {
		return ^uint64(0)
	}
	return kib << 10
}

// listDarwin shells out to ps(1): there is no pure-Go API for the BSD process
// table without cgo. One call yields pid, %cpu, RSS(kB) and full command.
func listDarwin() ([]raw, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,%cpu=,rss=,command=")
	cmd.WaitDelay = listPipeGrace
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinProcesses(string(out)), nil
}

func parseDarwinProcesses(out string) []raw {
	var list []raw
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[1], 64)
		if !(cpu >= 0) || math.IsInf(cpu, 0) {
			cpu = 0
		}
		rssKB, _ := strconv.ParseUint(fields[2], 10, 64)

		r := raw{
			pid:        pid,
			name:       fields[3],
			args:       fields[3:],
			rss:        kibToBytes(rssKB),
			cpuPercent: cpu,
		}
		annotate(&r)
		list = append(list, r)
	}
	return list
}
