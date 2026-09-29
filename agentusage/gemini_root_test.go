// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory may end in a rune Unicode calls a space: a checkout at
// "/work/toktop\u00a0" exists and is the one the chat ran in. Only the line
// terminator comes off .project_root, because trimming the space leaves a
// different path, and the watcher then drops every record of that chat as
// belonging to another directory, silently and for as long as it runs.
func TestGeminiRootKeepsASpaceInsideThePath(t *testing.T) {
	for _, trailing := range []string{"\u00a0", " ", "\u3000"} {
		work := filepath.Join(t.TempDir(), "toktop"+trailing)
		project := filepath.Join(t.TempDir(), "project")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, geminiProjectFile), []byte(work+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		cwd, ok := readGeminiRoot(project)
		if !ok {
			t.Fatalf("readGeminiRoot(%q) found no root", project)
		}
		if cwd != work {
			t.Errorf("readGeminiRoot(%q) = %q, want %q", project, cwd, work)
		}
	}
}
