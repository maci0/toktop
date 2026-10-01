// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The recovery the runbook in docs/RECOVERY.md describes, performed end to
// end in the order an operator performs it. Every other test here pins one
// stage of it and the doc points at them one at a time, which leaves the
// thing a backup actually has to answer unasked: does the whole sequence
// still work when it is run as one thing? Each of those tests can hold while
// the sequence has stopped being a recovery — a stage reordered, a stage that
// now clears the evidence the next one reads, a copy that has quietly stopped
// being written — and the breakage reaches an operator at the one moment the
// runbook is being followed for real.
//
// So the stages are asserted against each other here, not only within
// themselves: a copy is what the loss is recovered from, so it is checked
// before the loss and after it; the marks are what tell a loss apart from a
// re-pin, so they are checked before the restore reads them and after it
// spends them; and the store that comes back is the store the next connect
// reads, so the last stage is a real read rather than a file that exists.
func TestTheDocumentedRecoveryRunsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")

	// Stage 1: two hosts pinned, so the restore is checked for carrying every
	// pin back and not merely for producing a file that parses.
	first := pinLine("h1:22", "first")
	second := pinLine("h2:22", "second")
	if err := writeKnownHosts(path, map[string]string{"h1:22": first, "h2:22": second}); err != nil {
		t.Fatal(err)
	}

	// Stage 2: the copy the loss is recovered from. This is checked before the
	// loss rather than after it, because a copy that is not there cannot be
	// checked afterwards either: the restore would report no copy and pass on
	// a host that never had one.
	bak := backupPath(path)
	if why := staleStoreCopy(path); why != "" {
		t.Fatalf("a store that was just written has no usable copy beside it (%s), so the "+
			"loss below would have nothing to recover from", why)
	}

	// Stage 3: the loss. The store goes and the marks a killed write leaves
	// are beside it, which is the only thing that makes this a loss to recover
	// from rather than the operator's re-pin gesture.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	if !interruptedWrite(path) {
		t.Fatal("the loss left no mark, so the store would read as holding no pins and the " +
			"recovery below would be handed back a re-pin instead of the pins")
	}
	// The copy is the only record now, and it has to survive the loss: a
	// restore that reads it is only a recovery if nothing already spent it.
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("the copy is gone with the store, so there is nothing to restore: %v", err)
	}

	// Stage 4: the read that finds the loss, which is where an operator learns
	// there is a copy to restore from.
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("the documented recovery could not read the store back: %v", err)
	}
	if len(store) != 2 || store["h1:22"] != first || store["h2:22"] != second {
		t.Fatalf("the store read back as %v, want both pins; a recovery that returns fewer "+
			"pins than the operator had is worse than one that fails", store)
	}

	// Stage 5: the restore puts the file back and spends the marks it acted on.
	// Both halves matter to the next run, and a stage that cleared the store
	// without clearing them, or the reverse, is a sequence that has stopped
	// being a recovery.
	restoreStore(path)
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the restore did not put the store back: %v", serr)
	}
	if interruptedWrite(path) {
		t.Error("the marks are still beside the restored store, so the next deletion of it reads " +
			"as a loss to recover from rather than as the re-pin gesture, and the pin the " +
			"operator just rejected is handed back")
	}

	// Stage 6: the store that came back is the one the next run reads. A file
	// that exists is not a store, and this is the read that tells the two
	// apart.
	restored, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("the restored store does not read: %v", err)
	}
	if len(restored) != 2 || restored["h1:22"] != first || restored["h2:22"] != second {
		t.Errorf("the restored store reads as %v, want both pins back", restored)
	}

	// Stage 7: the re-pin gesture works again on the very next run, which is
	// what spending the marks bought. Without it the recovery is a one-shot
	// that leaves the host un-re-pinnable until the operator clears the marks
	// by hand.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	afterRePin, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted after a recovery must read as empty, not fail: %v", err)
	}
	if len(afterRePin) != 0 {
		t.Errorf("the store reads as %v after the re-pin gesture, want empty: the evidence the "+
			"repair spent has not been spent, so a host the operator un-pinned is pinned again", afterRePin)
	}

	// Stage 8: the copy the restore spent is left able to protect the next
	// store, or the second loss on this host is the one with nothing behind
	// it. The restore refreshes the copy from the store it recovered, so this
	// is a real assertion about the sequence rather than about the first write.
	if err := writeKnownHosts(path, map[string]string{"h3:22": pinLine("h3:22", "third")}); err != nil {
		t.Fatal(err)
	}
	if why := staleStoreCopy(path); why != "" {
		t.Errorf("after the recovery the store has no usable copy beside it (%s), so the next "+
			"loss on this host is the one nothing recovers", why)
	}
}

// The operator's own copy leaves two things wrong beside a store that is
// present again: the marks a killed write left are still there, and the copy
// the store was copied from is now older than the store it was copied into.
// settleOperatorRestore spends the first and refreshes the second on the next
// connect, and the runbook tells the operator that happens. This runs the
// operator's sequence as one thing and holds that both halves land, because
// the existing per-stage tests reach restoreStore instead: they leave the
// store missing, so restoreStore spends the marks and settleOperatorRestore
// finds nothing to do.
func TestTheRecoverySettlesTheCopyItRestoredFrom(t *testing.T) {
	path := useStore(t)
	line := pinLine("settle:22", "settled")
	if err := writeKnownHosts(path, map[string]string{"settle:22": line}); err != nil {
		t.Fatal(err)
	}
	// The store the operator copied back is present, so restoreStore, which
	// acts only on a store that is missing, leaves it alone and the settle is
	// the half that runs.
	stageInterrupted(t, path)
	// A copy a plain cp always leaves behind the store it was copied into:
	// the operator put the store back by hand, and the copy is older than it.
	aged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(backupPath(path), aged, aged); err != nil {
		t.Fatal(err)
	}
	if why := staleStoreCopy(path); why == "" {
		t.Fatal("the copy was not left behind the store, so there is no settle to run")
	}
	logs := captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if interruptedWrite(path) {
		t.Error("the marks are still beside the restored store, so the next deletion of it reads " +
			"as a loss to recover from rather than as the re-pin gesture")
	}
	if why := staleStoreCopy(path); why != "" {
		t.Errorf("the copy is still unusable after the recovery (%s), so the next connect warns "+
			"about a gap the operator cannot close and the store's protection is a warning", why)
	}
	// Settling copies the store into the copy and never the copy into the
	// store: the pins in force are the ones that end up in both.
	b, err := os.ReadFile(backupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.Fields(line)[1]) {
		t.Errorf("the copy does not hold the pin the store was recovered to: %q", b)
	}
	if got := linesWith(logs, "restored from a copy beside it"); len(got) != 1 {
		t.Errorf("the recovery was not audited exactly once, so a run that restored the store "+
			"left no record that it did, or a record nobody can tell from another: %v", got)
	}
}

// A store whose copy is gone with it is not a loss toktop can paper over: the
// marks are what license a read of the copy, and with neither store nor copy
// the next connect is a re-pin. That is the state the runbook says to expect,
// so it is asserted rather than assumed: a recovery path that invented a pin
// here would be a security change, and a silent one.
func TestALossWithNoCopyAnywhereRepinsRatherThanRecovers(t *testing.T) {
	path := useStore(t)
	if err := writeKnownHosts(path, map[string]string{"gone:22": pinLine("gone:22", "gone")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath(path)); err != nil {
		t.Fatal(err)
	}
	// No marks, because nothing was interrupted: a store removed by hand with
	// nothing beside it is the re-pin gesture and nothing else.
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted by hand must read as empty, not fail: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty: a loss with no copy and no mark has nothing to "+
			"recover from, and handing back a pin here would undo the operator's re-pin", store)
	}
}
