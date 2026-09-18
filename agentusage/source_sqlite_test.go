// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"sync"
	"testing"
	"time"
)

// fakeUsageSource counts how often a watcher consults it, so a test can see
// whether a provider that was withdrawn is still being read.
type fakeUsageSource struct {
	mu    sync.Mutex
	reads int
}

func (f *fakeUsageSource) read([]string, time.Time) (values, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return values{output: 1, input: 1}, true
}

func (f *fakeUsageSource) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// TestWatcherStopsReadingWithdrawnSource: a provider is a coeffect, not a fact
// of the watcher's creation. EnableOpenCodeDB(false) deletes the registry row,
// and a watcher built while it was on must stop reading the store rather than
// keep the binding it attached with.
func TestWatcherStopsReadingWithdrawnSource(t *testing.T) {
	const tool = "zz-withdraw-source"
	src := &fakeUsageSource{}
	registerSource(tool, tokenSource{usage: src})
	t.Cleanup(func() { registerSource(tool, tokenSource{}) })

	w := Watch(tool, t.TempDir(), time.Now())
	if w == nil {
		t.Fatal("a registered source should be watchable")
	}
	w.poll(nil)
	if got := src.readCount(); got != 1 {
		t.Fatalf("first poll read the provider %d times, want 1", got)
	}
	if got := w.Sample().Output; got != 1 {
		t.Fatalf("sample output %d, want 1", got)
	}

	// Withdraw the way setOpenCodeDB(false) does.
	registerSource(tool, tokenSource{})
	before := src.readCount()
	w.poll(nil)
	if got := src.readCount(); got != before {
		t.Fatalf("withdrawn provider was still read: %d reads, want %d", got, before)
	}
	if got := w.Sample().Output; got != 1 {
		t.Fatalf("sample moved after withdrawal: output %d, want the last reading 1", got)
	}
}
