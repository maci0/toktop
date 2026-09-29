// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A store directory is named with the slug of the working directory, and the
// slug spells the account that owns $HOME inside a single path component. A
// prefix fold of the path cannot see it, so the line would name the account
// in the very text an issue paste carries.
func TestWalkFailureFoldsSlugSpelledHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home", "dev")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	store := filepath.Join(home, ".cursor", "projects", pathSlug(filepath.Join(home, "Desktop", "vllm")), "agent-transcripts")
	lines := captureAudit(t)

	// The error carries the same spelling the walk would, which is the half
	// that survives a fold of the root attribute alone.
	auditWalkFailure(store, &walkError{err: "walk " + pathSlug(filepath.Join(home, "Desktop", "vllm")) + "/chat.jsonl: permission denied"})

	got := lines.String()
	if strings.Contains(got, "-dev") || strings.Contains(got, home) {
		t.Errorf("the line names the account that owns the home; got:\n%s", got)
	}
	if !strings.Contains(got, "~-Desktop-vllm") {
		t.Errorf("the line lost the directory the operator needs; got:\n%s", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("the line lost the reason; got:\n%s", got)
	}
}

// walkError carries preformatted text, the shape a walk failure hands to
// auditWalkFailure.
type walkError struct{ err string }

func (e *walkError) Error() string { return e.err }
