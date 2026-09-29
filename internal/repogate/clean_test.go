// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `make site-rollback` runs once, and the marker that makes the second run a
// no-op rather than a second undo is `dist/site.deployed`. A clean that took
// that marker would leave the rollback refusing with "nothing to roll back"
// while the deployment it would have undone is the one serving, and a routine
// clean is what somebody runs first when the site looks wrong. So the clean is
// run for real here, in a copy of the tree, and the markers are checked where
// the target left them.
func TestCleanKeepsTheSiteDeployAndRollbackMarkers(t *testing.T) {
	tree := copyTreeForClean(t)

	for _, marker := range []string{"site.deployed", "site.rolled-back"} {
		dir := filepath.Join(tree, "dist", marker)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A manifest is what makes the marker a record rather than an empty
		// directory, so the clean has something to be tempted to take.
		if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("commit: deadbeef\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Build output beside the markers, which is what the clean is for. The
	// hidden one is here because a sweep that only walked visible entries
	// would leave it behind.
	for _, junk := range []string{"toktop_0.1.0_linux_amd64", "site.lockless", ".leftover"} {
		if err := os.WriteFile(filepath.Join(tree, "dist", junk), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "toktop"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	runClean(t, tree)

	for _, marker := range []string{"site.deployed", "site.rolled-back"} {
		if _, err := os.Stat(filepath.Join(tree, "dist", marker, "manifest")); err != nil {
			t.Errorf("make clean took dist/%s, so the guard against a second rollback is gone: %v", marker, err)
		}
	}
	for _, junk := range []string{"toktop_0.1.0_linux_amd64", "site.lockless", ".leftover"} {
		if _, err := os.Stat(filepath.Join(tree, "dist", junk)); !os.IsNotExist(err) {
			t.Errorf("make clean left dist/%s behind: %v", junk, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tree, "toktop")); !os.IsNotExist(err) {
		t.Errorf("make clean left the built binary behind: %v", err)
	}
}

// A clean runs against a dist/ that holds only the markers (every release
// build emptied it), and against no dist/ at all (a fresh checkout, or one
// already cleaned). Both have to succeed, and the first has to keep the
// markers, or the guard is a thing that holds only when something else
// happens to be in the way.
func TestCleanWithoutBuildOutputKeepsTheMarkers(t *testing.T) {
	tree := copyTreeForClean(t)
	dir := filepath.Join(tree, "dist", "site.deployed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	runClean(t, tree)

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("make clean took the only entry in dist/: %v", err)
	}
}

// The deploy lock is the one thing under dist/ whose removal is a live
// hazard rather than build output, so the clean refuses while it is held
// instead of deleting around it. Carried here because the sweep that keeps
// the markers is the same recipe that has to leave this refusal alone.
func TestCleanRefusesWhileTheDeployLockIsHeld(t *testing.T) {
	tree := copyTreeForClean(t)
	if err := os.MkdirAll(filepath.Join(tree, "dist", "site.lock"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The refusal goes to stderr, which Output() does not carry.
	cmd := exec.Command("make", "clean")
	cmd.Dir = tree
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("make clean removed dist/site.lock out from under a running deploy")
	}
	if !strings.Contains(string(out), "site.lock") {
		t.Errorf("the refusal does not name the lock it is refusing over: %q", out)
	}
	if _, err := os.Stat(filepath.Join(tree, "dist", "site.lock")); err != nil {
		t.Errorf("the refusal still removed the lock: %v", err)
	}
}

// runClean runs the target in a copied tree and fails the test on a non-zero
// exit, so a caller states only what it expects to find afterwards.
func runClean(t *testing.T, tree string) {
	t.Helper()
	cmd := exec.Command("make", "clean")
	cmd.Dir = tree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make clean: %v\n%s", err, out)
	}
}

// copyTreeForClean copies the module root into a temp dir, skipping dist/ and
// the directories a clean never looks at. The copy is what keeps the test off
// the developer's own dist/ and the checkout's build output: `make clean`
// deletes the binary and everything in dist/, which is not this test's tree to
// spend.
func copyTreeForClean(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			// dist/ is the tree under test and .git is a large directory of
			// history the clean neither reads nor writes.
			if name := d.Name(); name == "dist" || name == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(tree, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tree, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}
