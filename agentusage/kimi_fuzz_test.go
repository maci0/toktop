// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzKimiSessionCwd drives the state.json reader that decides which project a
// Kimi Code CLI wire log belongs to. The log itself names no directory, so this
// file is the only thing that attributes a session's tokens to a working
// directory: a hostile or truncated one must not panic, must not answer a
// directory it did not read, and must not answer differently twice, since the
// verdict is cached and a flip would credit one project with another's tokens.
//
// The sibling path is derived from the wire log's own, so the reader is also
// walked with wire paths whose shape it does not expect: a name that resolves
// outside the session directory yields no verdict rather than a directory read
// from somewhere else.
func FuzzKimiSessionCwd(f *testing.F) {
	for _, seed := range []string{
		`{"cwd":"/home/dev/proj"}`,
		`{"cwd":"/"}`,
		`{"cwd":""}`,
		`{"cwd":null}`,
		`{"cwd":42}`,
		`{"cwd":["/home/dev"]}`,
		`{"cwd":"/home/dev","other":"/elsewhere"}`,
		`{"cwd":"/home/dev"} trailing garbage`,
		// A cwd that is a relative path, a device node, or a name that cannot
		// resolve: all of them are answers, and none of them is this machine's
		// project directory.
		`{"cwd":"relative/path"}`,
		`{"cwd":"//"}`,
		`{"cwd":"\u0000"}`,
		// Nul bytes and a BOM in the bytes themselves, not in the value.
		"{\"cwd\":\"/home/dev\"}\x00",
		"\xef\xbb\xbf" + `{"cwd":"/home/dev"}`,
		`{}`,
		`null`,
		`[]`,
		`{`,
		``,
	} {
		f.Add([]byte(seed), byte(0))
	}
	// The other three shapes, including the one that walks out of the store.
	for _, shape := range []byte{1, 2, 3} {
		f.Add([]byte(`{"cwd":"/home/dev"}`), shape)
	}

	f.Fuzz(func(t *testing.T, state []byte, shape byte) {
		store := t.TempDir()
		session := filepath.Join(store, "work", "session")
		agent := filepath.Join(session, "agents", "agent-1")
		if err := os.MkdirAll(agent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(session, "state.json"), state, 0o644); err != nil {
			t.Fatal(err)
		}
		// The shapes the session directory can sit at relative to the wire
		// log: the CLI's own layout under agents/<agentId>/, a main agent that
		// nests one level less, a log directly in the session, and one that
		// walks out of the store entirely.
		var (
			wire      string
			reachable bool
		)
		switch shape % 4 {
		case 0:
			wire, reachable = filepath.Join(agent, "wire.jsonl"), true
		case 1:
			wire, reachable = filepath.Join(session, "agents", "wire.jsonl"), true
		case 2:
			wire, reachable = filepath.Join(session, "wire.jsonl"), true
		default:
			wire, reachable = filepath.Join(store, "..", "wire.jsonl"), false
		}
		// The answer is the one an independent read of the same bytes gives:
		// a state.json that names no directory, or is not JSON at all, is no
		// verdict. Only the shape that walks out of the store differs, since
		// the file this run wrote is not on its path at all.
		want := readStateCwd(state)

		cwd, ok := kimiSessionCwd(wire)
		if ok != (reachable && want != "") {
			t.Fatalf("kimiSessionCwd(%q) ok=%v for a file holding %q", wire, ok, state)
		}
		if !ok {
			if cwd != "" {
				t.Fatalf("refused read returned %q", cwd)
			}
			return
		}
		// The directory answered must be the one in the bytes written, so a
		// verdict cannot come from anywhere else.
		if cwd != want {
			t.Fatalf("answered %q for a file holding %q", cwd, state)
		}
		// The verdict is cached per transcript, so it has to be the same on
		// every call.
		if again, againOK := kimiSessionCwd(wire); againOK != ok || again != cwd {
			t.Fatalf("unstable verdict: %q/%v then %q/%v", cwd, ok, again, againOK)
		}
	})
}

// readStateCwd is the independent reader the fuzzed one is checked against: the
// cwd field as encoding/json reads it, with the same BOM fold the reader under
// test applies. An empty string is a file that names no directory.
func readStateCwd(state []byte) string {
	var st struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(bytes.TrimPrefix(state, utf8BOM), &st) != nil {
		return ""
	}
	return st.Cwd
}
