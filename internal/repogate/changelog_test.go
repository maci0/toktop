// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	headerRe = regexp.MustCompile(`^## \[([^\]]+)\](?: - (\d{4}-\d{2}-\d{2}))?$`)
	linkRe   = regexp.MustCompile(`^\[([^\]]+)\]:\s*(\S+)`)
)

// changelogRelease is one dated release section of CHANGELOG.md.
type changelogRelease struct {
	version semver
	date    string
}

// changelogReleases returns the release sections of CHANGELOG.md newest first,
// each with the date its heading carries. It lives beside the changelog
// regexes it reads with rather than in whichever test called it first: the
// section list is the changelog's own shape, and openapi_test.go only consumes
// it to date a document against the releases.
func changelogReleases(t *testing.T) []changelogRelease {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	var out []changelogRelease
	for _, line := range strings.Split(string(raw), "\n") {
		m := headerRe.FindStringSubmatch(line)
		if m == nil || m[1] == "Unreleased" {
			continue
		}
		v, ok := parseSemver(m[1])
		if !ok {
			t.Fatalf("CHANGELOG.md section [%s] is not a version this gate can compare", m[1])
		}
		out = append(out, changelogRelease{version: v, date: m[2]})
	}
	return out
}

// semver is a parsed three-number version with its pre-release tag.
type semver struct {
	major int
	minor int
	patch int
	extra string
}

func (v semver) String() string {
	s := strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor) + "." + strconv.Itoa(v.patch)
	if v.extra != "" {
		return s + "-" + v.extra
	}
	return s
}

func parseSemver(s string) (semver, bool) {
	parts := strings.SplitN(s, "-", 2)
	extra := ""
	if len(parts) == 2 {
		extra = parts[1]
	}
	nums := strings.Split(parts[0], ".")
	if len(nums) != 3 {
		return semver{}, false
	}
	maj, err1 := strconv.Atoi(nums[0])
	min, err2 := strconv.Atoi(nums[1])
	pat, err3 := strconv.Atoi(nums[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return semver{}, false
	}
	return semver{major: maj, minor: min, patch: pat, extra: extra}, true
}

func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	if v.patch != o.patch {
		return v.patch < o.patch
	}
	if v.extra == "" && o.extra != "" {
		return false
	}
	if v.extra != "" && o.extra == "" {
		return true
	}
	return v.extra < o.extra
}

// findChangelogPath returns the repository's changelog, or the empty string
// when it cannot be found. The path is spelled through moduleRoot like every
// other file this package reads: the earlier list of relative candidates also
// probed a sibling package layout repogate does not have (it sat beside
// CHANGELOG.md before it moved under internal/), and a candidate naming a
// directory that does not exist is one more way to read whichever file
// answers first.
func findChangelogPath() string {
	candidates := []string{
		filepath.Join(moduleRoot, "CHANGELOG.md"),
		"CHANGELOG.md",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func TestChangelogIntegrity(t *testing.T) {
	path := findChangelogPath()
	if path == "" {
		t.Skip("CHANGELOG.md not found relative to test runner")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open CHANGELOG.md: %v", err)
	}
	defer f.Close()

	var (
		versions []string
		links    = make(map[string]string)
		inLinks  bool
		sawUnrel bool
		scanner  = bufio.NewScanner(f)
		lineNum  int
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		if m := headerRe.FindStringSubmatch(line); m != nil {
			v := m[1]
			date := m[2]
			if v == "Unreleased" {
				if sawUnrel {
					t.Errorf("line %d: duplicate [Unreleased] section", lineNum)
				}
				sawUnrel = true
				continue
			}
			if date == "" {
				t.Errorf("line %d: release [%s] missing release date YYYY-MM-DD", lineNum, v)
			}
			versions = append(versions, v)
			continue
		}

		if m := linkRe.FindStringSubmatch(line); m != nil {
			inLinks = true
			tag := m[1]
			url := m[2]
			if _, exists := links[tag]; exists {
				t.Errorf("line %d: duplicate link reference for [%s]", lineNum, tag)
			}
			links[tag] = url
			continue
		}

		if inLinks && strings.TrimSpace(line) != "" && !linkRe.MatchString(line) {
			t.Errorf("line %d: unexpected text after link block: %q", lineNum, line)
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}

	if !sawUnrel {
		t.Error("missing ## [Unreleased] section")
	}

	if len(versions) == 0 {
		t.Fatal("no release sections found in CHANGELOG.md")
	}

	// Verify SemVer ordering (must be strictly descending).
	for i := 0; i < len(versions); i++ {
		cur, ok := parseSemver(versions[i])
		if !ok {
			t.Errorf("version %q is not valid SemVer", versions[i])
			continue
		}
		if i > 0 {
			prev, okPrev := parseSemver(versions[i-1])
			if okPrev && !cur.less(prev) {
				t.Errorf("version %s on line is not ordered after earlier version %s", versions[i], versions[i-1])
			}
		}
	}

	// Verify all versions have link definitions.
	for _, v := range versions {
		if _, ok := links[v]; !ok {
			t.Errorf("missing compare link reference for release [%s]", v)
		}
	}

	// Verify [Unreleased] link exists and points to latest release compare.
	unrelLink, ok := links["Unreleased"]
	if !ok {
		t.Error("missing [Unreleased] link reference")
	} else {
		wantSuffix := "compare/v" + versions[0] + "...HEAD"
		if !strings.HasSuffix(unrelLink, wantSuffix) {
			t.Errorf("[Unreleased] link %q want suffix %q", unrelLink, wantSuffix)
		}
	}

	// Verify each release compare link points to prev...cur.
	for i := 0; i < len(versions)-1; i++ {
		cur := versions[i]
		prev := versions[i+1]
		curLink, ok := links[cur]
		if !ok {
			continue
		}
		wantSuffix := "compare/v" + prev + "...v" + cur
		if !strings.HasSuffix(curLink, wantSuffix) {
			t.Errorf("[%s] link %q want suffix %q", cur, curLink, wantSuffix)
		}
	}
}

// gitRuns runs one read-only git command in the module root and reports
// whether it answered. A source export, a tagless checkout or a host without
// git is not a failure here: what this guards is a tree with a release to
// compare against, and a tree without one has nothing to compare to.
func gitRuns(args ...string) ([]string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, true
	}
	return strings.Split(trimmed, "\n"), true
}

// nonConsumerTypes are the Conventional Commit types whose work this file
// does not describe: a change to the source's shape, to its tests, or to the
// tree it is built and documented from. A perf commit is deliberately not one
// of them, since its result is something a user times.
var nonConsumerTypes = map[string]bool{
	"build": true, "chore": true, "ci": true,
	"docs": true, "merge": true, "refactor": true, "test": true,
}

// owedToChangelog returns the subjects of the commits after ref that an entry
// is owed for. The rule is the commit subject's own type, falling back to
// owing an entry for anything it does not name: a subject spelled without a
// type is more likely a change a user meets than a change to the tree.
func owedToChangelog(ref string) []string {
	lines, ok := gitRuns("log", "--format=%s", ref+"..HEAD")
	if !ok {
		return nil
	}
	var owed []string
	for _, subject := range lines {
		if subject == "" || strings.HasPrefix(subject, "Merge ") {
			continue
		}
		typ, _, split := strings.Cut(subject, ":")
		if !split {
			owed = append(owed, subject)
			continue
		}
		if before, _, scoped := strings.Cut(typ, "("); scoped {
			typ = before
		}
		if !nonConsumerTypes[strings.TrimSpace(typ)] {
			owed = append(owed, subject)
		}
	}
	return owed
}

// unreleasedEntries counts the bullets under ## [Unreleased], the rule the
// Makefile's CHECK_CHANGELOG applies when it refuses a cut that left any
// behind.
func unreleasedEntries(changelog string) int {
	var entries int
	in := false
	for _, line := range strings.Split(changelog, "\n") {
		if strings.HasPrefix(line, "## [") {
			in = line == "## [Unreleased]"
			continue
		}
		if in && strings.HasPrefix(line, "- ") {
			entries++
		}
	}
	return entries
}

// TestChangelogRecordsWorkSinceLastRelease refuses a merge that moves the
// tree past a release without saying, under [Unreleased], what a user of the
// next release meets.
//
// The release gates cannot catch this: CHECK_CHANGELOG reads CHANGELOG.md
// against the version being cut, so it says whether the last cut was written
// down, never whether the work since it was. A fix that lands with no entry
// moves into the next release's section as whatever the next author had time
// for, or into the auto-generated GitHub notes, which carry commit subjects
// rather than what changed for the reader.
//
// A CHANGELOG.md that moved since the tag is let past, which is what leaves
// the release cut alone: emptying [Unreleased] is what moves the file, and
// the commit carrying that move runs before the tag it will be cut under
// exists. An edit made for any other reason while work landed unrecorded
// slips by too; the check is a merge gate, not a proof.
func TestChangelogRecordsWorkSinceLastRelease(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable")
	}
	lines, ok := gitRuns("describe", "--tags", "--abbrev=0", "HEAD")
	if !ok || len(lines) == 0 {
		t.Skip("no released tag before HEAD; nothing for the changelog to owe an entry to")
	}
	tag := lines[0]

	changed, ok := gitRuns("diff", "--name-only", tag+"..HEAD", "--", "CHANGELOG.md")
	if !ok {
		t.Skipf("git diff unavailable")
	}
	if len(changed) > 0 {
		return
	}

	owed := owedToChangelog(tag)
	if len(owed) == 0 {
		return
	}

	path := findChangelogPath()
	if path == "" {
		t.Skip("CHANGELOG.md not found relative to test runner")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	if n := unreleasedEntries(string(raw)); n > 0 {
		return
	}

	t.Errorf("CHANGELOG.md has moved %d commits past %s and records none of them under [Unreleased]:",
		len(owed), tag)
	for _, subject := range owed {
		t.Errorf("  %s", subject)
	}
	t.Errorf("  whoever upgrades reads that section, not the commit log; write the entry before merging")
}
