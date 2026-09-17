package procs

import (
	"slices"
	"testing"
)

func TestParseDarwinProcesses(t *testing.T) {
	for _, line := range []string{
		"123 2.5 4096 /usr/bin/ollama serve --port 11434",
		"  123   2.5  4096 /usr/bin/ollama   serve --port 11434  ",
		"\t123\t2.5\t4096\t/usr/bin/ollama\tserve\t--port\t11434",
	} {
		t.Run(line, func(t *testing.T) {
			list := parseDarwinProcesses("\ninvalid\n" + line + "\n")
			if len(list) != 1 {
				t.Fatalf("processes = %+v, want one process", list)
			}
			p := list[0]
			if p.pid != 123 || p.cpuPercent != 2.5 || p.rss != 4096*1024 || p.name != "/usr/bin/ollama" {
				t.Errorf("process = %+v, want pid 123, CPU 2.5, RSS 4194304 and ollama executable", p)
			}
			if want := []string{"/usr/bin/ollama", "serve", "--port", "11434"}; !slices.Equal(p.args, want) {
				t.Errorf("args = %q, want %q", p.args, want)
			}
			if port := ExtractPort(p.args); port != 11434 {
				t.Errorf("port = %d, want 11434", port)
			}
		})
	}
}
