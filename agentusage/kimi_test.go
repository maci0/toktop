// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"testing"
)

// kimiLayout builds the store the CLI writes:
// <root>/<workDirKey>/<session>/agents/<agentId>/wire.jsonl, with state.json
// beside agents/, not inside it. It returns the wire log's path and the session
// directory it belongs to.
func kimiLayout(t *testing.T, cwd string) (wire, session string) {
	t.Helper()
	session = filepath.Join(t.TempDir(), "wd_key", "session-1")
	agentDir := filepath.Join(session, "agents", "agent-0")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeKimiFile(t, filepath.Join(session, "state.json"), `{"cwd":"`+cwd+`"}`)
	wire = filepath.Join(agentDir, "wire.jsonl")
	writeKimiFile(t, wire, "")
	return wire, session
}

func writeKimiFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKimiSessionCwd(t *testing.T) {
	want := t.TempDir()
	wire, _ := kimiLayout(t, want)
	got, ok := kimiSessionCwd(wire)
	if !ok {
		t.Fatal("kimiSessionCwd found no state.json")
	}
	if got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestKimiSessionCwdNoState(t *testing.T) {
	wire, session := kimiLayout(t, t.TempDir())
	if err := os.Remove(filepath.Join(session, "state.json")); err != nil {
		t.Fatal(err)
	}
	if cwd, ok := kimiSessionCwd(wire); ok {
		t.Errorf("kimiSessionCwd = (%q, true), want no verdict", cwd)
	}
}

func TestKimiParseUsage(t *testing.T) {
	line := []byte(`{"type":"usage.record","usage":{"inputOther":10,"output":5,"inputCacheRead":100,"inputCacheCreation":7}}`)
	v, cwd, ok := parseKimi(line)
	if !ok {
		t.Fatal("parseKimi rejected a usage.record line")
	}
	if cwd != "" {
		t.Errorf("cwd = %q, want empty: the wire log names no directory", cwd)
	}
	if v.input != 117 {
		t.Errorf("input = %d, want 117", v.input)
	}
	if v.output != 5 {
		t.Errorf("output = %d, want 5", v.output)
	}
}
