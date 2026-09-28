// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import (
	"os"
	"testing"
	"time"
)

// A panic inside the platform lister must not leave the in-flight sweep flag
// set. Every later SnapshotAt short-circuits on that flag, so a flag stuck
// true freezes the engine list at its last values for the life of the process,
// and the frozen panel reports no error to explain itself.
func TestPanicInListerReleasesTheSweep(t *testing.T) {
	orig := platformList
	t.Cleanup(func() { platformList = orig })

	panicked := false
	platformList = func() ([]raw, error) {
		if !panicked {
			panicked = true
			panic("lister exploded")
		}
		list := []raw{{pid: os.Getpid() + 1, name: "ollama", args: []string{"ollama", "serve"}}}
		for i := range list {
			annotate(&list[i])
		}
		return list, nil
	}

	s := NewSampler()
	s.refreshMin = time.Hour

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the stubbed lister did not panic")
			}
		}()
		s.SnapshotAt(time.Now())
	}()

	// The window is set on the way in, so a second call inside it returns the
	// cache whether or not the flag was released. Reading the flag says which
	// of the two short-circuits applied; zeroing s.last then takes the window
	// out of the way so the lister is reached for real.
	s.mu.Lock()
	stillSweeping := s.sweeping
	s.last = time.Time{}
	s.mu.Unlock()
	if stillSweeping {
		t.Fatal("sweep flag was not released by the panic")
	}

	got := s.SnapshotAt(time.Now())
	if len(got) != 1 {
		t.Fatalf("the sampler did not sweep again after the panic: %+v", got)
	}
}
