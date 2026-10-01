// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every line this package writes about one ssh:// target has to name that
// target. Discovery and credential assembly run per target, an operator can
// attach several at once, and both are optional enough on their own that the
// failure is invisible on the dashboard: a box that hid /proc/net/tcp and a
// box that had none are the same picture. The audit line is the only record,
// and a line reading "remote engine scan failed" with nothing on it saying
// which box is not a diagnosis.

// A host whose sweep cannot be run has to name itself on the line, the way the
// connect, tunnel and vitals lines already do.
func TestDiscoveryFailureLinesNameTheTarget(t *testing.T) {
	withKnownHosts(t)
	logs := captureAudit(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Closing the connection is what makes both optional sweeps fail: the
	// scripts cannot run on a peer that no longer answers.
	cli.Close()

	if _, err := Discover(t.Context(), cli, []int{11434}); err == nil {
		t.Fatal("Discover over a closed connection succeeded; the sweeps under test never ran")
	}

	// Discover returns an error when the active port probe cannot run, which is
	// the line the operator sees for a peer that is gone. Whichever of the two
	// sweeps was reached first, the lines written must name the target.
	var seen int
	for _, sub := range []string{"remote listening-port sweep", "remote engine scan failed"} {
		for _, line := range linesWith(logs, sub) {
			seen++
			if !namesTarget(line) {
				t.Errorf("line does not name the target: %q", line)
			}
			if !strings.Contains(line, "level=WARN") {
				t.Errorf("line logged below warn: %q", line)
			}
		}
	}
	if seen == 0 {
		t.Fatalf("no discovery line was audited for a failed sweep: %v", linesWith(logs, "sweep"))
	}
}

// A default key that will not load is a per-connection condition, and an
// operator attaching three targets reads the same line once per box with no
// way to tell them apart unless it names the one it is about.
func TestUnusableDefaultKeyLineNamesTheTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	disableAgent(t)
	logs := captureAudit(t)

	// The port is not what decides the line, but the target is: a chain built
	// for a named host has to say so.
	if _, _, err := (Target{Host: "box", Port: 2222}).authMethods(); err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	warns := linesWith(logs, "default ssh key unusable")
	if len(warns) != 1 {
		t.Fatalf("audit lines for one unusable default key = %d, want 1: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "target=box") {
		t.Errorf("the key line does not name the target: %q", warns[0])
	}
	if !strings.Contains(warns[0], "port=2222") {
		t.Errorf("the key line does not name the port: %q", warns[0])
	}
}
