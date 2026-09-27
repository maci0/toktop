//go:build linux

package procs

import (
	"os"
	"path/filepath"
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

func TestPagesToBytesSaturates(t *testing.T) {
	ps := uint64(os.Getpagesize())
	if got := pagesToBytes(512); got != 512*ps {
		t.Errorf("512 pages = %d, want %d", got, 512*ps)
	}
	if got := pagesToBytes(^uint64(0)); got != ^uint64(0) {
		t.Errorf("huge page count = %d, want saturation", got)
	}
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
