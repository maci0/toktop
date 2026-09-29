//go:build darwin

package procs

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// bytesPerKiB is the unit of the ps(1) rss column.
const bytesPerKiB uint64 = 1 << 10

func init() {
	platformList = listDarwin
}

// listDarwin shells out to ps(1): there is no pure-Go API for the BSD process
// table without cgo. One call yields pid, %cpu, RSS(kB) and full command.
func listDarwin() ([]raw, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,%cpu=,rss=,command=")
	cmd.WaitDelay = listPipeGrace
	core.GroupKill(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps -axo pid=,%%cpu=,rss=,command=: %w", err)
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
		if err != nil || pid <= 0 {
			continue // not a process row
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
			rss:        core.MulSatU64(rssKB, bytesPerKiB),
			cpuPercent: cpu,
		}
		annotate(&r)
		list = append(list, r)
	}
	return list
}
