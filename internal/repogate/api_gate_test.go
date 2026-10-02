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

// The prefix CHECK_API is assigned to, the same way changelog_gate_test.go
// finds CHECK_CHANGELOG.
const apiGate = "CHECK_API = "

// apiGateRecipe returns the Makefile's exported-surface gate with $(VERSION),
// $(PUBLIC_PKGS), $(TAR) and $(GO) resolved and the recipe's `$$` escapes
// collapsed to `$`, ready to run as a shell script inside a checkout holding a
// real Go module.
//
// The substitution of $(PUBLIC_PKGS), $(TAR) and $(GO) is what the Makefile
// does before the shell ever sees the line, and it is spelled here rather than
// re-derived: a gate driven with the wrong package list, the wrong tar or an
// unresolved $(GO) compares nothing and passes, which is the shape of the
// failure this file exists to catch.
func apiGateRecipe(t *testing.T, version string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, apiGate) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("Makefile has no CHECK_API recipe; the exported-surface gate is gone")
	}
	var recipe strings.Builder
	for _, line := range lines[start:] {
		recipe.WriteString(line)
		if !strings.HasSuffix(line, `\`) {
			break
		}
		recipe.WriteString("\n")
	}
	body := recipe.String()
	body = body[strings.Index(body, apiGate)+len(apiGate):]
	body = strings.ReplaceAll(body, "$(VERSION)", version)
	body = strings.ReplaceAll(body, "$(PUBLIC_PKGS)", "./agentusage")
	body = strings.ReplaceAll(body, "$(TAR)", tarCommand())
	body = strings.ReplaceAll(body, "$(GO)", "go")
	return strings.ReplaceAll(body, "$$", "$")
}

// tarCommand resolves the Makefile's TAR the way its own assignment does:
// gtar when it is on PATH, tar otherwise, so the gate under test extracts the
// tagged tree with the same tool the release uses.
func tarCommand() string {
	if _, err := exec.LookPath("gtar"); err == nil {
		return "gtar"
	}
	return "tar"
}

// apiGateRepo builds a checkout holding a real Go module that publishes
// ./agentusage, tagged as the last release, plus a commit past the tag for the
// gate to measure HEAD^ against.
//
// The module is a real one rather than a fixture because the gate's whole
// input is `go doc -all ./agentusage`: a stub tree would let a broken
// extraction, a missing scripts/api-surface.awk or a wrong awk invocation
// come back "nothing was removed" and pass a gate that is not comparing
// anything. That is the failure this file exists to catch, so the tree it
// drives has to be one the real gate really reads.
func apiGateRepo(t *testing.T, tag, exported string) string {
	t.Helper()
	for _, tool := range []string{"git", "sh", "awk", "tar", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	dir := t.TempDir()
	// The gate runs `go doc` in the extracted tree and in the checkout, so the
	// module cache and the build cache have to be writable wherever the host
	// put them; a test that cannot build skips rather than failing the
	// package on a read-only HOME.
	if _, err := os.Stat(filepath.Join(moduleRoot, "scripts", "api-surface.awk")); err != nil {
		t.Skipf("scripts/api-surface.awk not readable: %v", err)
	}
	awk, err := os.ReadFile(filepath.Join(moduleRoot, "scripts", "api-surface.awk"))
	if err != nil {
		t.Skipf("read scripts/api-surface.awk: %v", err)
	}
	files := map[string]string{
		"go.mod":                  "module " + modulePath + "\n\ngo 1.24\n",
		"agentusage/doc.go":       "package agentusage\n",
		"agentusage/api.go":       exported,
		"scripts/api-surface.awk": string(awk),
		"CHANGELOG.md":            "# Changelog\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	apiGit(t, dir, "init", "-q", "-b", "main")
	apiCommit(t, dir, "release")
	apiGit(t, dir, "tag", tag)
	// The gate reads the last release out of HEAD^, so the tree needs a
	// commit past the tag for that to reach it.
	apiGit(t, dir, "commit", "-q", "--allow-empty", "-m", "post-release")
	return dir
}

// apiGateRepoBroken is apiGateRepo with a release tag that sits on a package
// `go doc` cannot read, which is the one state in which the gate has no
// declarations to compare on either side: the tree it extracts the release
// from is the same tree that does not parse.
func apiGateRepoBroken(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"git", "sh", "awk", "tar", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	awk, err := os.ReadFile(filepath.Join(moduleRoot, "scripts", "api-surface.awk"))
	if err != nil {
		t.Skipf("read scripts/api-surface.awk: %v", err)
	}
	files := map[string]string{
		"go.mod":                  "module " + modulePath + "\n\ngo 1.24\n",
		"agentusage/doc.go":       "package agentusage\n",
		"agentusage/api.go":       "package agentusage\n\nfunc Broken( {\n",
		"scripts/api-surface.awk": string(awk),
		"CHANGELOG.md":            "# Changelog\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	apiGit(t, dir, "init", "-q", "-b", "main")
	apiCommit(t, dir, "release")
	apiGit(t, dir, "tag", tag)
	apiGit(t, dir, "commit", "-q", "--allow-empty", "-m", "post-release")
	return dir
}

// apiCommit stages and commits everything in dir with an author the temp
// checkout's missing git config would otherwise refuse.
func apiCommit(t *testing.T, dir, message string) {
	t.Helper()
	apiGit(t, dir, "add", "-A")
	apiGit(t, dir, "commit", "-q", "-m", message)
}

func apiGit(t *testing.T, dir string, args ...string) {
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

// runAPIGate runs the gate in dir and reports what it refused with.
func runAPIGate(t *testing.T, dir, version string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", apiGateRecipe(t, version))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeAPI replaces the published package's declarations in the checkout.
func writeAPI(t *testing.T, dir, exported string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agentusage", "api.go"), []byte(exported), 0o644); err != nil {
		t.Fatalf("write agentusage/api.go: %v", err)
	}
}

// TestCheckAPIGate pins the release gate that stands between an exported
// removal and a published tag.
//
// This is the one gate in the tree that answers "does this version break a Go
// caller", and it is the only place a caller learns about a break from the
// release rather than from their own compiler. The gate's comment records
// that an exported removal once reached a published release with every other
// gate reporting green, which is exactly the shape of failure a gate nobody
// runs cannot be caught by: a recipe that stopped matching the file it reads,
// or a comparison that quietly found nothing, would still exit 0 and still be
// cited as the reason a release is safe.
const (
	apiWithBoth = "package agentusage\n\n// Sample is an exported struct.\ntype Sample struct {\n\tIn int\n}\n\n// Parse reads a sample.\nfunc Parse() Sample { return Sample{} }\n"
	apiWithOne  = "package agentusage\n\n// Sample is an exported struct.\ntype Sample struct {\n\tIn int\n}\n"
)

func TestCheckAPIGate(t *testing.T) {
	t.Run("a removal on a patch bump is refused", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithOne)
		apiCommit(t, dir, "drop an exported declaration")
		out, err := runAPIGate(t, dir, "0.9.1")
		if err == nil {
			t.Fatalf("gate passed a patch release that removes an exported declaration:\n%s", out)
		}
		if !strings.Contains(out, "func Parse() Sample") {
			t.Errorf("refusal does not name the declaration that went:\n%s", out)
		}
	})

	t.Run("a removal on a minor bump is refused with no Breaking entry", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithOne)
		apiCommit(t, dir, "drop an exported declaration")
		out, err := runAPIGate(t, dir, "0.10.0")
		if err == nil {
			t.Fatalf("gate passed a minor release whose removal the changelog does not admit:\n%s", out)
		}
		if !strings.Contains(out, "Breaking") {
			t.Errorf("refusal does not point at the missing Breaking heading:\n%s", out)
		}
	})

	t.Run("a removal on a minor bump with a Breaking entry passes", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithOne)
		if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(
			"# Changelog\n\n## [Unreleased]\n\n## [0.10.0] - 2026-09-28\n\n### Breaking\n\n- `Parse` is gone; call `Sample` directly.\n\n"+
				"[Unreleased]: https://example.invalid/compare/v0.10.0...HEAD\n"+
				"[0.10.0]: https://example.invalid/compare/v0.9.0...v0.10.0\n"), 0o644); err != nil {
			t.Fatalf("write CHANGELOG.md: %v", err)
		}
		apiCommit(t, dir, "drop an exported declaration and record it")
		if out, err := runAPIGate(t, dir, "0.10.0"); err != nil {
			t.Fatalf("gate refused a minor release that records its break in the changelog:\n%s", out)
		}
	})

	t.Run("a release that changes nothing passes", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithBoth+"\n// Extra is a new declaration.\nfunc Extra() int { return 0 }\n")
		apiCommit(t, dir, "add an exported declaration")
		if out, err := runAPIGate(t, dir, "0.9.1"); err != nil {
			t.Fatalf("gate refused a release that removed nothing:\n%s", out)
		}
	})

	t.Run("a dev build compares nothing", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithOne)
		apiCommit(t, dir, "drop an exported declaration")
		if out, err := runAPIGate(t, dir, "dev"); err != nil {
			t.Fatalf("gate refused a dev build, which carries no version to judge:\n%s", out)
		}
	})

	t.Run("a package go doc cannot read is refused rather than compared as empty", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		// A published package that does not parse: `go doc` exits 1 and prints
		// nothing, which is the shape a moved or half-written file takes on a
		// release tag. The gate used to run it through a pipeline whose exit
		// status was sort's, so both sides came out empty, `comm -23` found
		// nothing removed, and the release was declared free of removals over
		// a comparison that never happened. It has to refuse here instead, and
		// say that it could not read the surface.
		writeAPI(t, dir, "package agentusage\n\nfunc Broken( {\n")
		apiCommit(t, dir, "leave the published package unparseable")
		out, err := runAPIGate(t, dir, "0.9.1")
		if err == nil {
			t.Fatalf("gate passed a release whose exported surface it could not read:\n%s", out)
		}
		if !strings.Contains(out, "could not read the exported surface") {
			t.Errorf("refusal does not say the surface was unreadable, so a reader cannot tell it from a real removal:\n%s", out)
		}
	})

	t.Run("an unreadable package on both sides is refused rather than compared empty", func(t *testing.T) {
		// The silent pass this gate used to be able to give, and the one shape
		// the other cases above cannot reach: `go doc` fails on the release
		// tree AND on the checkout, so both sides come out empty together and
		// `comm -23` over two empty sets reports nothing removed. That is the
		// release declared free of removals over a comparison that never
		// happened, with every gate green.
		//
		// The tag itself sits on a package `go doc` cannot read, which is what
		// makes both sides fail: a gate that reaches back to that commit for
		// its base tree finds nothing to print there either. A removal that
		// emptied the surface, a rename that moved the file off the path, and a
		// package that stopped parsing all reach this same place, and the last
		// two are what a release carries without anything failing the build.
		dir := apiGateRepoBroken(t, "v0.9.0")
		writeAPI(t, dir, "package agentusage\n\nfunc AlsoBroken(\n")
		apiCommit(t, dir, "break the working package the same way")
		out, err := runAPIGate(t, dir, "0.9.1")
		if err == nil {
			t.Fatalf("gate passed a release whose exported surface it could not read on either side:\n%s", out)
		}
		if !strings.Contains(out, "could not read the exported surface") {
			t.Errorf("refusal does not say the surface was unreadable, so a reader cannot tell a failed comparison from a clean one:\n%s", out)
		}
	})

	t.Run("a package that exports nothing is refused rather than compared as empty", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		// The other fail-open shape: `go doc` succeeds on a package left with
		// no exported declarations, so both files parse, are empty, and a diff
		// of two empty sets reports no change. A removal that emptied the
		// surface is exactly the break this gate exists to refuse, so it cannot
		// be the one it waves through.
		writeAPI(t, dir, "package agentusage\n\nfunc unexported() int { return 0 }\n")
		apiCommit(t, dir, "remove every exported declaration")
		out, err := runAPIGate(t, dir, "0.9.1")
		if err == nil {
			t.Fatalf("gate passed a release that emptied the exported surface:\n%s", out)
		}
	})

	t.Run("a shallow checkout is refused rather than passing on nothing", func(t *testing.T) {
		dir := apiGateRepo(t, "v0.9.0", apiWithBoth)
		writeAPI(t, dir, apiWithOne)
		apiCommit(t, dir, "drop an exported declaration")
		// A clone without the history has the removal in its HEAD^ and no
		// release behind it, so the comparison would find nothing to
		// complain about and exit 0. The release workflow checks out at
		// fetch-depth: 0 for this reason, and the gate refuses the shallow
		// tree rather than reporting it clean.
		shallow := t.TempDir()
		apiGit(t, dir, "clone", "--depth", "1", "file://"+dir, shallow)
		out, err := runAPIGate(t, shallow, "0.9.1")
		if err == nil {
			t.Fatalf("gate passed a shallow checkout that carries no release to compare against:\n%s", out)
		}
		if !strings.Contains(out, "shallow") {
			t.Errorf("refusal does not name the shallow clone as the cause:\n%s", out)
		}
	})
}
