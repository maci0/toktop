// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A staging file the sweep cannot remove is reported, for the reason
// DiscardStaged reports one beside it: the sweep is the only thing that ever
// removes these, so a refusal here leaves a file in the install or host-key
// directory that no later run will clear. Swallowing it reports a clean
// success over a directory that is not clean.
//
// The removal is made to fail by emptying the write bit on the directory
// rather than by pointing the sweep somewhere else: os.Remove refuses to
// unlink a file out of a directory the caller cannot write, and a directory
// the sweep cannot read at all fails at ReadDir instead, which is the branch
// that deliberately reports nothing. So this test runs unprivileged.
func TestSweepStaleTempsReportsARefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks a file out of a directory the mode says it cannot")
	}
	dir := t.TempDir()
	stuck := filepath.Join(dir, "toktop.tmp-stuck")
	if err := os.WriteFile(stuck, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-StaleTempAge - time.Hour)
	if err := os.Chtimes(stuck, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot drop write permission on the directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := SweepStaleTemps(dir, "toktop.tmp", time.Now())
	if err == nil {
		t.Fatal("the sweep reported success over a staging file it did not remove")
	}
	if !strings.Contains(err.Error(), filepath.Base(stuck)) {
		t.Errorf("the error does not name the file left behind: %v", err)
	}
	if _, serr := os.Stat(stuck); serr != nil {
		t.Errorf("the staging file was removed anyway: %v", serr)
	}
}

// A sweep that removed everything it found reports nothing, so the callers
// that warn on its error stay quiet on the ordinary path.
func TestSweepStaleTempsReportsNothingOnACleanSweep(t *testing.T) {
	dir := t.TempDir()
	aged := filepath.Join(dir, "toktop.tmp-old")
	if err := os.WriteFile(aged, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-StaleTempAge - time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	if err := SweepStaleTemps(dir, "toktop.tmp", time.Now()); err != nil {
		t.Errorf("a sweep that removed its one leftover reported %v", err)
	}
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Errorf("the aged staging file survived the sweep: %v", err)
	}
}
