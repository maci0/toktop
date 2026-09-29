//go:build linux

package procs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSamplerLinuxTree runs the linux sampler against a synthetic /proc tree.
func TestSamplerLinuxTree(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// fake ollama: comm-less cmdline NUL-separated; stat with utime+stime
	// (fields 14,15) and rss in pages (field 24: 512 pages = 2 MiB)
	write("123/cmdline", "ollama\x00serve\x00--port\x0011434\x00")
	write("123/stat", "123 (ollama) S 1 1 0 0 -1 0 0 0 0 0 100 50 0 0 0 0 1 0 0 0 512")
	// kernel thread without cmdline must be skipped
	write("456/stat", "456 (kworker/0:1) S 2 0 0 0")
	// firefox: not an engine
	write("789/cmdline", "/usr/bin/firefox\x00")
	write("789/stat", "789 (firefox) S 1 1 0 0 0 0 500 700 0 0")

	oldRoot := procRoot
	procRoot = root
	defer func() { procRoot = oldRoot }()

	s := NewSampler()
	first := s.Snapshot()
	var found *Info
	for i := range first {
		if first[i].PID == 123 {
			found = &first[i]
		}
	}
	if found == nil {
		t.Fatal("engine process missing from sample")
	}
	if found.Engine != "ollama" || found.PortHint != 11434 || found.RSS != 2048<<10 {
		t.Fatalf("sample = %+v", found)
	}
	if found.CPUPct != 0 {
		t.Errorf("first sample must have no cpu delta, got %v", found.CPUPct)
	}
	for _, p := range first {
		if p.PID == 456 {
			t.Error("kernel thread leaked into listing")
		}
		if p.PID == 789 {
			t.Error("unrelated process leaked into engine listing")
		}
	}
}

// The walk reads each command line into a buffer of CmdlinePrefix bytes, so
// a process whose command line is longer than that is read to its prefix. The
// engine it is must still be found (its name and module path sit at the
// front), and nothing past the prefix may reach a match, since that is the
// same bound ClipArgs puts on every other lister's listing.
func TestSamplerLinuxLongCmdlineIsBounded(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A browser's shape: the engine-looking flag is at the front and a
	// --disable-features blob of tens of kilobytes trails it.
	write("321/cmdline", "python\x00-m\x00vllm.entrypoints\x00--disable-features\x00"+
		strings.Repeat("X", 64*1024)+"\x00")
	write("321/stat", "321 (python) S 1 1 0 0 -1 0 0 0 0 0 100 50 0 0 0 0 1 0 0 0 512")
	// The same engine named only past the prefix: no match, and no port.
	write("654/cmdline", "browser\x00"+strings.Repeat("Y", CmdlinePrefix)+"\x00--port\x0011434\x00")

	oldRoot := procRoot
	procRoot = root
	defer func() { procRoot = oldRoot }()

	first := NewSampler().Snapshot()
	var found *Info
	for i := range first {
		if first[i].PID == 321 {
			found = &first[i]
		}
		if first[i].PID == 654 {
			t.Error("a match past the prefix reached the listing")
		}
	}
	if found == nil {
		t.Fatal("engine with an over-long command line missing from the sample")
	}
	if found.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm", found.Engine)
	}
	joined := 0
	for _, a := range found.Args {
		joined += len(a) + 1
	}
	if joined > CmdlinePrefix+1 {
		t.Errorf("retained %d command-line bytes, cap is %d", joined, CmdlinePrefix)
	}
}

// BenchmarkListLinux measures one whole /proc sweep on the host running the
// benchmark, which is the sweep every poll pays for and the reason the walk
// reuses its buffers: the process count and the width of a browser's argv
// are what set the cost, and neither is visible from a fixture.
func BenchmarkListLinux(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := listLinux(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPagesToBytesSaturates(t *testing.T) {
	_, rss := procStatCPUAndRSS("1 (x) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 18446744073709551615")
	if rss != ^uint64(0) {
		t.Errorf("stat RSS wrap: %d, want saturation", rss)
	}
}

// The tick sum is the other half of the same stat line: utime+stime must
// saturate, since a wrapped sum reads as no CPU used at all.
func TestStatTicksSaturate(t *testing.T) {
	// Fields 4..13 are the ten tokens between state and utime; utime is
	// field 14, stime field 15, rss field 24.
	const huge = "18446744073709551615"
	const filler = "0 0 0 0 0 0 0 0 0 0 "
	ticks, _ := procStatCPUAndRSS("1 (x) S " + filler + huge + " " + huge + " 0 0 0 0 0 0 0 0 0")
	if ticks != ^uint64(0) {
		t.Errorf("stat tick sum = %d, want saturation", ticks)
	}
	ticks, _ = procStatCPUAndRSS("1 (x) S " + filler + "10 20 0 0 0 0 0 0 0 0 0")
	if ticks != 30 {
		t.Errorf("stat tick sum = %d, want 30", ticks)
	}
}
