// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// snapshotDefs copies the registry so a load can be compared against the
// state it started from: a rejected file must leave nothing behind, and a
// successful one must register names that SpecFor can find again.
func snapshotDefs() map[string]Spec {
	defsMu.RLock()
	defer defsMu.RUnlock()
	out := make(map[string]Spec, len(defs))
	for k, v := range defs {
		out[k] = v
	}
	return out
}

func restoreDefs(saved map[string]Spec) {
	defsMu.Lock()
	defer defsMu.Unlock()
	defs = saved
}

// FuzzLoadDefinitions drives the agents.json config parser with arbitrary
// bytes. The file is written by another program (gauntlet) or by hand, and
// every field it accepts is a path a watcher will later walk, so a hostile
// document must not panic, must not register a name nothing can look up, and
// must not leave half a file behind when it is refused: a rejected load
// registers none of the names the file carried, and an accepted one registers
// only names that keep at least one non-blank root.
func FuzzLoadDefinitions(f *testing.F) {
	for _, seed := range []string{
		`{"myagent":{"usage":{"roots":["~/.myagent/sessions"]}}}`,
		`{"myagent":{"usage":{"roots":["{dir}/state"],"suffix":".jsonl"}}}`,
		`{"myagent":{"usage":{"roots":[""],"suffixes":[".jsonl",".jsonl.zst"]}}}`,
		`{"myagent":{"usage":{"roots":["   "]}}}`,
		`{"myagent":{"usage":{"roots":"not a list"}}}`,
		`{"myagent":{"usage":null}}`,
		`{"myagent":null}`,
		`{"myagent":{}}`,
		`{"  spaced  ":{"usage":{"roots":["~"]}}}`,
		`{"":{"usage":{"roots":["~"]}}}`,
		`{"café":{"usage":{"roots":["~"]}}}`,
		`{"café":{"usage":{"roots":["~"]}}}`,
		`{"pi":{"usage":{"roots":["~/other"]}}}`,
		`{}`,
		`null`,
		`[]`,
		`[1,2,3]`,
		`"a string"`,
		`42`,
		`{"a":{"usage":{"roots":["~"]}},"b":{"usage":{"roots":["~"]}}}`,
		`{"a":{"usage":{"roots":["~"],"cumulative":true,"header_cwd":true}}}`,
		`{"a":{"usage":{"suffixes":[]}}}`,
		"{\"a\":{\"usage\":{\"roots\":[\"\xff\xfe\"]}}}",
		"\xef\xbb\xbf{\"a\":{\"usage\":{\"roots\":[\"~\"]}}}",
		`{"a":`,
		`{`,
		`}`,
		``,
		`nul`,
		`{"a":{"usage":{"roots":["~"]}},"a":{"usage":{"roots":["~"]}}}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "agents.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}

		// The load writes into the package map, so give it a copy of its own
		// and keep the pristine snapshot for the restore: handing the load
		// the snapshot would leave nothing to roll back to.
		saved := snapshotDefs()
		restoreDefs(snapshotDefs())
		t.Cleanup(func() { restoreDefs(saved) })

		err := LoadDefinitions(path)
		loaded := snapshotDefs()

		if err != nil {
			if !errors.Is(err, ErrInvalidDefinitions) {
				t.Fatalf("LoadDefinitions(%q) failed with %v, want ErrInvalidDefinitions", data, err)
			}
			// Atomic: a refusal registers nothing from the file.
			if len(loaded) != len(saved) {
				t.Fatalf("rejected file %q still changed the registry: %d -> %d entries", data, len(saved), len(loaded))
			}
			// A second load of the same bytes must fail the same way.
			if err2 := LoadDefinitions(path); !errors.Is(err2, ErrInvalidDefinitions) {
				t.Fatalf("LoadDefinitions not deterministic for %q: first %v, second %v", data, err, err2)
			}
			return
		}

		for name, spec := range loaded {
			if name == "" {
				t.Fatalf("file %q registered an empty agent name", data)
			}
			if canonicalTool(name) != name {
				t.Fatalf("file %q registered %q uncanonicalized (want %q)", data, name, canonicalTool(name))
			}
			if len(specRoots(spec)) == 0 {
				t.Fatalf("file %q registered %q with no usable root: %+v", data, name, spec)
			}
			got, ok := SpecFor(name)
			if !ok {
				t.Fatalf("file %q registered %q but SpecFor does not find it", data, name)
			}
			if !slices.Equal(got.Roots, spec.Roots) || got.Suffix != spec.Suffix ||
				!slices.Equal(got.Suffixes, spec.Suffixes) ||
				got.Cumulative != spec.Cumulative || got.HeaderCwd != spec.HeaderCwd {
				t.Fatalf("file %q: SpecFor(%q) = %+v, stored %+v", data, name, got, spec)
			}
			// The slices handed out are copies: a caller that edits them
			// must not reach into the registry.
			got.Roots[0] = "clobbered"
			got.Suffixes = append(got.Suffixes, "clobbered")
			again, _ := SpecFor(name)
			if again.Roots[0] == "clobbered" || slices.Contains(again.Suffixes, "clobbered") {
				t.Fatalf("file %q: SpecFor(%q) returned aliased slices: %+v", data, name, again)
			}
		}

		// Loading the same file again must not change anything.
		restoreDefs(loaded)
		if err2 := LoadDefinitions(path); err2 != nil {
			t.Fatalf("LoadDefinitions(%q) not deterministic: first nil, second %v", data, err2)
		}
		if again := snapshotDefs(); len(again) != len(loaded) {
			t.Fatalf("LoadDefinitions(%q) not idempotent: %d -> %d entries", data, len(loaded), len(again))
		}
	})
}
