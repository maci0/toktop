// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
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

// A case-folding file system hands a process the spelling it was started
// with, and the environment spells the account its own way: a home of
// /home/Dev and a process launched from /home/dev name one account and reach
// the line spelled two ways, so the fold has to compare folds, not bytes.
func TestRedactStorePathFoldsCaseSpelledSlug(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home", "Dev")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// The spelling the process carried, one directory under the same home.
	started := filepath.Join(filepath.Dir(home), "dev", "Desktop", "vllm")
	got := redactStorePath("read " + pathSlug(started) + "/chat.jsonl: permission denied")

	if strings.Contains(got, "dev") {
		t.Errorf("the line names the account that owns the home; got:\n%s", got)
	}
	if !strings.Contains(got, "~-Desktop-vllm") {
		t.Errorf("the line lost the directory the operator needs; got:\n%s", got)
	}
}

// The same fold, one spelling apart in normalization: a macOS home stored
// decomposed against a precomposed one is one account, and the slug carries
// the difference into the line because it is one path component.
func TestRedactStorePathFoldsDecomposedSlug(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, norm.NFC.String("jose\u0301"))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	started := filepath.Join(base, norm.NFD.String("jose\u0301"), "Desktop")
	got := redactStorePath("read " + pathSlug(started) + "/chat.jsonl: permission denied")

	if strings.Contains(got, "jose") {
		t.Errorf("the line names the account that owns the home; got:\n%s", got)
	}
	if !strings.Contains(got, "~-Desktop") {
		t.Errorf("the line lost the directory the operator needs; got:\n%s", got)
	}
}

// walkError carries preformatted text, the shape a walk failure hands to
// auditWalkFailure.
type walkError struct{ err string }

func (e *walkError) Error() string { return e.err }
