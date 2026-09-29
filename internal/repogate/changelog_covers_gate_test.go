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

// changelogCoversTarget is the target whose recipe is the gate: the one that
// asks whether a surface a reader reads moved since the last release tag
// without CHANGELOG.md moving with it.
const changelogCoversTarget = "check-changelog-covers:"

// changelogCoversRecipe returns the shell behind `make check-changelog-covers`
// with the watched-path list resolved and the recipe's `$$` escapes collapsed
// to `$`, ready to run with its working directory set to the checkout under
// test. The target is a recipe rather than a `VAR =` assignment because it
// reads the working tree's git history, which no single file holds.
func changelogCoversRecipe(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	watched := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "CHANGELOG_WATCHED = ") {
			watched = strings.TrimSpace(strings.TrimPrefix(line, "CHANGELOG_WATCHED = "))
			break
		}
	}
	if watched == "" {
		t.Fatal("Makefile names no CHANGELOG_WATCHED list; the coverage gate compares nothing")
	}

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, changelogCoversTarget) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("Makefile has no %s target; the coverage gate is gone", changelogCoversTarget)
	}
	var recipe strings.Builder
	for _, line := range lines[start:] {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		// Only the first line carries the recipe prefix; the rest are the
		// continuations of one shell command, indented but unprefixed.
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "\t"), "@"))
		recipe.WriteString(body)
		if !strings.HasSuffix(body, `\`) {
			break
		}
		recipe.WriteString("\n")
	}
	body := recipe.String()
	if body == "" {
		t.Fatalf("Makefile's %s target has no recipe", changelogCoversTarget)
	}
	body = strings.ReplaceAll(body, "$(CHANGELOG_WATCHED)", watched)
	return strings.ReplaceAll(body, "$$", "$")
}

// coversRepo builds a checkout holding one released tag, one commit past it, and
// runs the gate in it. The tag is what the gate measures against, and HEAD^ is
// how it reaches it, so the tree needs a second commit for the base to be the
// tag rather than nothing.
func coversRepo(t *testing.T, files map[string]string, tag string) string {
	t.Helper()
	for _, tool := range []string{"git", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "release")
	run("tag", tag)
	// The release tag is on the first commit, so the work under test needs a
	// commit of its own for `git describe HEAD^` to reach the tag.
	run("commit", "-q", "--allow-empty", "-m", "post-release")
	return dir
}

// runCoversGate runs the gate in dir and reports whether it refused.
func runCoversGate(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", changelogCoversRecipe(t))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestChangelogCoversGate pins the gate to the change it exists for: a surface
// a reader or a caller reads moving since the last release with the changelog
// untouched is a change that ships unannounced, and every other gate in the
// tree reads the changelog's shape rather than the diff it describes, so
// nothing else would have seen it.
func TestChangelogCoversGate(t *testing.T) {
	base := map[string]string{
		"README.md":         "what toktop is\n",
		"CHANGELOG.md":      "# Changelog\n",
		"internal/ui/x.go":  "package ui\n",
		"docs/openapi.yaml": "openapi: 3.1.0\n",
	}

	t.Run("a README change with no changelog entry is refused", func(t *testing.T) {
		dir := coversRepo(t, base, "v0.1.0")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("what toktop is, and now more\n"), 0o644); err != nil {
			t.Fatalf("write README.md: %v", err)
		}
		commitAll(t, dir, "edit the README")
		out, err := runCoversGate(t, dir)
		if err == nil {
			t.Fatalf("gate passed a README change with no CHANGELOG.md edit; a consumer reads that file and not the commit:\n%s", out)
		}
		if !strings.Contains(out, "README.md") {
			t.Errorf("refusal does not name the file that moved:\n%s", out)
		}
	})

	t.Run("the same change with a changelog entry passes", func(t *testing.T) {
		dir := coversRepo(t, base, "v0.1.0")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("what toktop is, and now more\n"), 0o644); err != nil {
			t.Fatalf("write README.md: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# Changelog\n\n- an entry\n"), 0o644); err != nil {
			t.Fatalf("write CHANGELOG.md: %v", err)
		}
		commitAll(t, dir, "edit the README and the changelog")
		if out, err := runCoversGate(t, dir); err != nil {
			t.Fatalf("gate refused a documented change:\n%s", out)
		}
	})

	t.Run("a change to unwatched code passes", func(t *testing.T) {
		dir := coversRepo(t, base, "v0.1.0")
		if err := os.WriteFile(filepath.Join(dir, "internal/ui/x.go"), []byte("package ui\n\nfunc f() {}\n"), 0o644); err != nil {
			t.Fatalf("write internal/ui/x.go: %v", err)
		}
		commitAll(t, dir, "change an internal package")
		if out, err := runCoversGate(t, dir); err != nil {
			t.Fatalf("gate asked for an entry for a change no reader sees:\n%s", out)
		}
	})

	t.Run("a checkout with no released tag passes", func(t *testing.T) {
		dir := coversRepo(t, base, "v0.1.0")
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		run("tag", "-d", "v0.1.0")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed with no release to measure against\n"), 0o644); err != nil {
			t.Fatalf("write README.md: %v", err)
		}
		commitAll(t, dir, "edit the README")
		if out, err := runCoversGate(t, dir); err != nil {
			t.Fatalf("gate refused a tree with no released tag to compare against:\n%s", out)
		}
	})
}

// commitAll stages and commits everything in dir, with an author the temp
// checkout's missing config would otherwise refuse.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
}
