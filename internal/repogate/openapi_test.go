// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var openAPIVersionRe = regexp.MustCompile(`(?m)^\s{2}version:\s*"?([^"\s]+)"?\s*$`)

// TestOpenAPIVersionNamesTheReleaseItDescribes holds the OpenAPI document's
// `info.version` to the release its content shipped in.
//
// A client generator reads that field to decide whether its generated code
// still matches the server, so it has to move when the feed does. Nothing else
// in the tree watches it: internal/ingest/openapi_test.go holds the document
// to the handlers, and make check-changelog-covers holds the file to a
// changelog entry, but a changelog entry for a changed feed is compatible with
// a version that stood still, and the document said 0.1.0 through a string of
// changes to the answers, the headers and the event caps.
//
// The rule reads the file's last committed change and the release dates in
// CHANGELOG.md. The release that shipped that change is the newest section
// dated on or after it; where the change is not in a release yet, the newest
// section is the floor, so unreleased work is held to the version it will ship
// as. A version above the newest section is refused too, since it names a
// release no one has cut.
func TestOpenAPIVersionNamesTheReleaseItDescribes(t *testing.T) {
	path := filepath.Join(moduleRoot, "docs", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}
	m := openAPIVersionRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("docs/openapi.yaml has no info.version line to read")
	}
	docVersion, ok := parseSemver(string(m[1]))
	if !ok {
		t.Fatalf("docs/openapi.yaml info.version is %q, which is not a version", m[1])
	}

	releases := changelogReleases(t)
	if len(releases) == 0 {
		t.Skip("CHANGELOG.md carries no dated release section to compare against")
	}
	newest := releases[0].version

	out, err := exec.Command("git", "-C", moduleRoot, "log", "-1", "--format=%cI", "--", "docs/openapi.yaml").Output()
	if err != nil {
		t.Skipf("cannot read the history of docs/openapi.yaml: %v", err)
	}
	changed := strings.TrimSpace(string(out))
	if len(changed) < 10 {
		t.Skipf("no committed change to docs/openapi.yaml to compare a release against (%q)", changed)
	}
	changed = changed[:10]

	floor := newest
	shipped := false
	for _, r := range releases {
		if r.date >= changed {
			floor = r.version
			shipped = true
			break
		}
	}

	if docVersion.less(floor) {
		if shipped {
			t.Errorf("docs/openapi.yaml info.version is %s; the file last changed on %s, which %s shipped",
				docVersion, changed, floor)
		} else {
			t.Errorf("docs/openapi.yaml info.version is %s; the file last changed on %s, after %s was cut",
				docVersion, changed, newest)
		}
		t.Errorf("  a client generator pins that field, so it has to name the release the served feed is documented as")
		t.Errorf("  set it to the version this change ships as (currently at least %s)", floor)
	}
	if floor.less(docVersion) && newest.less(docVersion) {
		t.Errorf("docs/openapi.yaml info.version is %s, above the newest CHANGELOG.md release (%s)", docVersion, newest)
		t.Errorf("  it has to name a release that exists; a version nothing has been cut under describes nothing")
	}
}
