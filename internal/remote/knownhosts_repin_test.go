package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
)

// Deleting the store is how an operator re-pins a host on purpose, after a
// key change they have checked. The backup is written on every write, so if a
// missing store falls back to it unconditionally, that repair is silently
// undone and the host is pinned to the very key the operator just rejected.
func TestDeletingTheStoreRepinsRatherThanRecoveringTheBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("repin"))))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath(path)); err != nil {
		t.Fatalf("the write must leave a backup to make this test mean anything: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted on purpose must read as empty, not fail: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty: the backup re-pinned the host the operator un-pinned", store)
	}
	restoreStore(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("restoreStore must not write the backup back over a store deleted on purpose: %v", err)
	}
}

// The deletion itself is silent: nothing logs a store that is simply absent,
// because absent is the re-pin gesture, and every run that finds no store is
// either a first contact or an operator's decision. An accidental removal (a
// config reset, a cleanup script, a sync that dropped the file) is therefore
// first reported by the next connect trusting a host the operator had already
// pinned, which is the interception the store exists to refuse, while a copy
// holding those pins sits beside it unread. The copy must not be restored
// (that is the re-pin), so the warning is the whole of what the operator is
// told, and it has to name the copy and the command that puts it back.
func TestDeletingTheStoreWarnsWhenACopyStillHoldsItsPins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	path := filepath.Join(dir, "known_hosts")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("repin"))))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	restoreStore(path)

	warns := linesWith(logs, "host key store was removed")
	if len(warns) != 1 {
		t.Fatalf("audit lines for a deleted store holding a copy = %d, want 1: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "level=WARN") {
		t.Errorf("deletion logged below warn: %q", warns[0])
	}
	if !strings.Contains(warns[0], backupSuffix) {
		t.Errorf("deletion line does not name the copy that still holds the pins: %q", warns[0])
	}
	if !strings.Contains(warns[0], core.RedactHome(restoreCommand(backupPath(path), path))) {
		t.Errorf("deletion line does not carry the command that restores the store: %q", warns[0])
	}
	if strings.Contains(warns[0], dir) {
		t.Errorf("audit line carries the home directory: %q", warns[0])
	}

	// A first run on a host that has never pinned anything is not a loss, and
	// warning about it would put a line in front of every operator's first
	// connection.
	logs.Reset()
	restoreStore(filepath.Join(t.TempDir(), "known_hosts"))
	if got := linesWith(logs, "host key store was removed"); len(got) != 0 {
		t.Errorf("a store that was never written is audited as removed: %v", got)
	}
}

// The other half of the same rule: a write killed between creating its
// staging file and renaming it over the store is a store that went missing by
// accident, and that one the copy does stand in for.
func TestInterruptedWriteRecoversTheStoreFromItsCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("repin"))))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	// The marks a killed write leaves: the staging file it never renamed, and
	// the store it never put back after moving it aside.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, knownHostsTempPrefix+"killed")
	if err := os.WriteFile(staging, []byte("half a write"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("an interrupted write must recover from its copy: %v", err)
	}
	if len(store) != 1 || store["h:22"] != line {
		t.Fatalf("store = %v, want the pinned host recovered", store)
	}
	restoreStore(path)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("an interrupted write must leave the store back in place: %v", err)
	}
}

// The evidence a restore acted on is spent once the store is back. Left
// sitting, it outlasts the recovery it justified, and the next delete is not
// the operator's re-pin gesture any more: the marks beside the store say a
// write was interrupted, so the backup is handed back and the operator is
// pinned to the key they had just rejected, for as long as the mark survives.
func TestRestoringTheStoreClearsTheEvidenceItActedOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("repin"))))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Both marks a write that did not finish leaves: the staging file it
	// never renamed, and the store it never put back after moving it aside.
	stageInterrupted(t, path)
	if err := os.WriteFile(displacedPath(path), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !interruptedWrite(path) {
		t.Fatal("the setup left no evidence, so this test would mean nothing")
	}

	restoreStore(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an interrupted write must leave the store back in place: %v", err)
	}
	if interruptedWrite(path) {
		t.Errorf("the marks a restore acted on are still beside the store, so a later delete reads as a loss instead of a re-pin")
	}

	// The re-pin gesture, run after the recovery: it has to re-pin.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted after a restore must read as empty, not fail: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty: evidence the repair had already happened re-pinned the host the operator un-pinned", store)
	}
}

// stageInterrupted leaves the mark a write that died between creating its
// staging file and renaming it over the store would leave, so a test can set
// up the one absence the copies are allowed to stand in for.
func stageInterrupted(t *testing.T, path string) {
	t.Helper()
	name := filepath.Join(filepath.Dir(path), knownHostsTempPrefix+"interrupted")
	if err := os.WriteFile(name, []byte("a write that did not finish"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A displaced copy on its own is the same evidence: replaceFile leaves one
// when a kill lands between its two renames on Windows.
func TestInterruptedWriteRecognizesTheDisplacedCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("repin"))))
	if err := os.WriteFile(displacedPath(path), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !interruptedWrite(path) {
		t.Fatal("a leftover displaced copy is a write that did not finish")
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != 1 {
		t.Fatalf("store = %v, want the displaced pins recovered", store)
	}
}
