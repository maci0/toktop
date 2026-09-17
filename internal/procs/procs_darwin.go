//go:build darwin

package procs

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

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
		rssKB, _ := strconv.ParseUint(fields[2], 10, 64)

		list = append(list, raw{
			pid:        pid,
			name:       fields[3],
			args:       fields[3:],
			rss:        rssKB << 10,
			cpuPercent: cpu,
		})
	}
	return list
}
