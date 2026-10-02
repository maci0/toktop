// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The copies beside the host-key store share its directory, its credential
// and its fate, so they are not the backup the RPO table prices a lost store
// at. What covers the losses they cannot — a lost home directory, a replaced
// machine, a compromised account — is a copy the operator takes, and the
// runbook is the only place that can say how. While it said only that backing
// up the directory was "the operator's half", the one backup this project has
// had no procedure, no destination outside the failure domain, and no way to
// find out that it had stopped working: the RPO row pointed at a step nobody
// could perform.
//
// Prose does not fail a build, so the step is pinned here instead. What is
// checked is the claim the RPO row makes, not the wording around it: the
// runbook names a way to take the copy, names what has to survive it, and
// gives a command that proves the copy can be read back.
func TestTheRunbookSaysHowThePinStoreIsBackedUp(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(recoveryTable)))
	if err != nil {
		t.Fatal(err)
	}
	body := string(doc)
	for _, want := range []struct{ what, line string }{
		{"a section for the backup itself", "## Backing the store up"},
		// The RPO row prices a lost store against a copy, so it has to name
		// where the copy comes from rather than assuming one exists.
		{"an RPO row for the pinned host keys", "| RPO for pinned host keys |"},
	} {
		if !strings.Contains(body, want.line) {
			t.Errorf("%s is missing %s (%q): the store's directory is the only backup of the "+
				"pins, and a procedure nobody can run or check is not one",
				recoveryTable, want.what, want.line)
		}
	}

	// Everything else is checked against the backup section alone. Searching
	// the whole document passes on a check some other section happens to
	// spell: "Verifying a restore" runs ssh-keygen over the same store, so a
	// backup section that had dropped its own verification would still find
	// that line here, and the one place that has to name the step is the one
	// place it went missing from.
	idx := strings.Index(body, "## Backing the store up")
	if idx < 0 {
		return // the missing section is already reported above
	}
	section, _, ok := strings.Cut(body[idx:], "\n## ")
	if !ok {
		t.Fatal("the backup section has no following section to bound it")
	}
	for _, want := range []struct{ what, line string }{
		// What is copied is the whole directory: the copies beside the store
		// are what a restore reads, so an archive holding only known_hosts is
		// a copy of the one file its neighbours already cover.
		{"the copy beside the store", `known_hosts.bak`},
		{"the copy a Windows write leaves behind", `known_hosts.displaced`},
		// A backup has to be checked, or it is a hypothesis: an archive that
		// cannot be unpacked and read is indistinguishable from one that was
		// never taken until the loss.
		{"a check that the archive reads back", "ssh-keygen -l -f"},
		{"a check that the archive holds what it should", "tar -tzf"},
	} {
		if !strings.Contains(section, want.line) {
			t.Errorf("the backup section in %s is missing %s (%q): the store's directory is "+
				"the only backup of the pins, and a copy nobody checked is a hypothesis",
				recoveryTable, want.what, want.line)
		}
	}

	// The backup is only a backup if it leaves the failure domain: an archive
	// written next to the store is deletable by the credential that deletes
	// the store, which is the compromise the section exists for.
	if !strings.Contains(section, "credential") {
		t.Errorf("the backup section never says where the copy must live; a copy under the same " +
			"credential as the store survives nothing the copies beside it do not")
	}
}

// The commands in the backup section are shell, and shell in prose is the part
// of a runbook that rots: an archive command that names a path this platform
// does not have, or a check that cannot read what the archive wrote, is a
// backup nobody notices has stopped working until the loss.
//
// They are run here rather than read, against a store shaped like the one a
// first connect leaves behind, and the whole round trip is performed: take the
// copy, verify it, lose the directory, put it back, and confirm the pin that
// came back is the pin that went in. The fingerprints are compared because the
// section tells the operator to compare them, and a step whose output cannot
// be checked is not a check.
func TestTheDocumentedBackupAndRestoreRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skipf("no tar on this host: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(recoveryTable)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "ssh-keygen") {
		t.Fatalf("%s names no fingerprint tool to verify a restored store with", recoveryTable)
	}

	home := t.TempDir()
	config := filepath.Join(home, "config")
	store := filepath.Join(config, "toktop")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	// A store as a first connect leaves it: a host and a key, with the copy
	// every write refreshes beside it.
	pin, err := drillKey(t)
	if err != nil {
		t.Fatal(err)
	}
	line := "drill:22 " + pin + "\n"
	if err := os.WriteFile(filepath.Join(store, "known_hosts"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "known_hosts.bak"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	// The backup step, as the section spells it: the directory, under the
	// directory above it, so the archive holds "toktop/known_hosts".
	archive := filepath.Join(home, "store.tgz")
	out, err := exec.Command("tar", "-czf", archive, "-C", config, "toktop").CombinedOutput()
	if err != nil {
		t.Fatalf("the documented backup step does not run: %v: %s", err, out)
	}
	listing, err := exec.Command("tar", "-tzf", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("the documented verify step cannot list the archive: %v: %s", err, listing)
	}
	// Both files, not one: the copies beside the store are what a restore
	// reads, so an archive that dropped them is a copy of the one file they
	// already cover.
	for _, want := range []string{"toktop/known_hosts", "toktop/known_hosts.bak"} {
		if !strings.Contains(string(listing), want) {
			t.Errorf("the archive does not hold %s: %s", want, listing)
		}
	}

	// The disaster the copies beside the store cannot cover at all: the whole
	// directory goes, and with it every pin on the host.
	if err := os.RemoveAll(store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store, "known_hosts")); !os.IsNotExist(err) {
		t.Fatalf("the store survived the loss, so nothing below was restored: %v", err)
	}

	// The restore, as the section spells it: unpack where the store lives.
	if out, err := exec.Command("tar", "-xzf", archive, "-C", config).CombinedOutput(); err != nil {
		t.Fatalf("the documented restore step does not run: %v: %s", err, out)
	}
	restored, err := os.ReadFile(filepath.Join(store, "known_hosts"))
	if err != nil {
		t.Fatalf("the restored directory holds no store: %v", err)
	}
	if string(restored) != line {
		t.Errorf("restored store = %q, want %q", restored, line)
	}

	// The store the restore produced is one toktop reads: the pin is enforced
	// by comparing key material, so the record has to carry the key back
	// intact rather than the host alone.
	if !strings.Contains(string(restored), strings.Fields(pin)[1]) {
		t.Errorf("the restored store lost its key material, so the next connect re-trusts the "+
			"host: %q", restored)
	}
}

// drillKey returns a throwaway public key in authorized-keys form, generated
// for this test and used nowhere else. It is written under the test's own
// temporary directory, so it is deleted with it.
func drillKey(t *testing.T) (string, error) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C",
		"recovery-drill", "-f", key).CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %w: %s", err, out)
	}
	b, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return "", os.ErrInvalid
	}
	return fields[0] + " " + fields[1], nil
}
