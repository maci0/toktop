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

// changelogGate is the CHECK_CHANGELOG recipe: the shell the `make
// check-changelog` and `make release` gates run over CHANGELOG.md.
const changelogGate = "CHECK_CHANGELOG = "

// changelogGateRecipe returns the Makefile's changelog gate with $(VERSION)
// resolved and the recipe's `$$` escapes collapsed to `$`, ready to run as a
// shell script with the working directory holding a CHANGELOG.md.
func changelogGateRecipe(t *testing.T, version string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, changelogGate) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("Makefile has no CHECK_CHANGELOG recipe; the release gate is gone")
	}
	var recipe strings.Builder
	for _, line := range lines[start:] {
		recipe.WriteString(line)
		if !strings.HasSuffix(line, `\`) {
			break
		}
		recipe.WriteString("\n")
	}
	// The assignment itself is not shell: everything after `=` is.
	body := recipe.String()
	body = body[strings.Index(body, changelogGate)+len(changelogGate):]
	return strings.ReplaceAll(strings.ReplaceAll(body, "$(VERSION)", version), "$$", "$")
}

// releaseChangelog is a CHANGELOG.md holding one new release, cut the way
// CONTRIBUTING describes: the section moves out of Unreleased, an empty
// Unreleased stub is left behind, and both compare links are updated. stubFirst
// puts the stub above the new section, the layout this file has carried since
// 0.5.0; the other puts it below, which the gate accepts and the bump rule has
// to read past.
func releaseChangelog(newVersion, prevVersion, heading string, stubFirst bool) string {
	stub := "## [Unreleased]\n\n"
	section := "## [" + newVersion + "] - 2026-09-28\n\n" + heading + "\n\n- an entry\n\n"
	if !stubFirst {
		stub, section = section, stub
	}
	return "# Changelog\n\n" +
		stub + section +
		"## [" + prevVersion + "] - 2026-09-28\n\n### Fixed\n\n- older\n\n" +
		"[Unreleased]: https://github.com/maci0/toktop/compare/v" + newVersion + "...HEAD\n" +
		"[" + newVersion + "]: https://github.com/maci0/toktop/compare/v" + prevVersion + "...v" + newVersion + "\n" +
		"[" + prevVersion + "]: https://github.com/maci0/toktop/compare/v0.0.0...v" + prevVersion + "\n"
}

// runChangelogGate writes changelog into a temp tree beside a copy of the
// tree's Makefile and runs the gate there, the way `make check-changelog
// VERSION=...` does.
func runChangelogGate(t *testing.T, version, changelog string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatalf("write CHANGELOG.md: %v", err)
	}
	cmd := exec.Command("sh", "-c", changelogGateRecipe(t, version))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("gate output: %s", out)
	}
	return err
}

// TestChangelogGateVersionBumps pins which bumps each impact heading may ride.
// The rule takes the previous version from the heading below the new section,
// and `## [Unreleased]` is a heading too: a cut that leaves the stub there
// rather than at the top of the file must still be told 0.18.2 came before
// 0.18.3, or a breaking change rides a patch.
func TestChangelogGateVersionBumps(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		prev       string
		heading    string
		stubFirst  bool
		wantCutOff bool
	}{
		{name: "breaking in a patch", version: "0.18.3", prev: "0.18.2", heading: "### Breaking", stubFirst: true, wantCutOff: true},
		{name: "breaking in a patch, stub below", version: "0.18.3", prev: "0.18.2", heading: "### Breaking", wantCutOff: true},
		{name: "breaking in a minor", version: "0.19.0", prev: "0.18.2", heading: "### Breaking", stubFirst: true},
		{name: "breaking in a minor, stub below", version: "0.19.0", prev: "0.18.2", heading: "### Breaking"},
		{name: "fix in a patch", version: "0.18.3", prev: "0.18.2", heading: "### Fixed", stubFirst: true},
		{name: "fix in a minor", version: "0.19.0", prev: "0.18.2", heading: "### Fixed", stubFirst: true},
		{name: "added in a patch", version: "0.18.3", prev: "0.18.2", heading: "### Added", stubFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changelog := releaseChangelog(tt.version, tt.prev, tt.heading, tt.stubFirst)
			err := runChangelogGate(t, tt.version, changelog)
			if tt.wantCutOff && err == nil {
				t.Errorf("VERSION=%s cut a %s section; the gate allows it", tt.version, tt.heading)
			}
			if !tt.wantCutOff && err != nil {
				t.Errorf("VERSION=%s refused a %s section: %v", tt.version, tt.heading, err)
			}
		})
	}
}
