// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README's install section is the first thing a `go install` user reads,
// and the go.mod floor is the one version they cannot discover from a failed
// build without reading the compiler output. A floor bump has to land there
// too, or the documented minimum and the enforced one drift apart.
func TestReadmeNamesTheGoFloor(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var floor string
	for line := range strings.SplitSeq(string(mod), "\n") {
		if after, ok := strings.CutPrefix(line, "go "); ok {
			floor = strings.TrimSpace(after)
			break
		}
	}
	if floor == "" {
		t.Fatal("go.mod has no go directive")
	}
	parts := strings.Split(floor, ".")
	if len(parts) < 2 {
		t.Fatalf("go.mod go directive %q has no minor version", floor)
	}
	majorMinor := strings.Join(parts[:2], ".")

	readme, err := os.ReadFile(filepath.Join(moduleRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Go " + majorMinor; !strings.Contains(string(readme), want) {
		t.Errorf("README.md install section must name %q (the go.mod floor is %s)", want, floor)
	}
}
