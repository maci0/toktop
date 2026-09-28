// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// packSpec lays a spec out as the one string the fuzzer can mutate: the roots
// and the alternate suffixes in NUL-separated lines, the single suffix and the
// two flags as one field each. Nothing in specAdapter reads the separators, so
// the mutations land inside the fields a definitions file would carry.
func packSpec(roots []string, suffix string, suffixes []string, cumulative, headerCwd bool) string {
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	return strings.Join([]string{
		strings.Join(roots, "\x00"),
		suffix,
		strings.Join(suffixes, "\x00"),
		flag(cumulative),
		flag(headerCwd),
	}, "\n")
}

// unpackSpec reads back what packSpec wrote. A field that split into more
// parts than the layout allows keeps its first, so a mutated string decodes
// into a spec rather than into an error.
func unpackSpec(packed string) Spec {
	parts := strings.SplitN(packed, "\n", 5)
	field := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	list := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, "\x00")
	}
	return Spec{
		Roots:      list(field(0)),
		Suffix:     field(1),
		Suffixes:   list(field(2)),
		Cumulative: field(3) == "1",
		HeaderCwd:  field(4) == "1",
	}
}

// FuzzSpecAdapter drives the definition-to-adapter compiler with arbitrary
// specs. Every field it reads is a line out of an agents.json written by
// another program, and every field it produces is a pattern a watcher walks
// recursively: a blank root would be the current directory, and a blank
// suffix would point the JSON parser at the agent's own config. So a hostile
// spec must compile to an adapter that walks nothing blank, must say so when
// it cannot be used, and must derive the same roots every time it is asked,
// for any working directory and at any instant.
func FuzzSpecAdapter(f *testing.F) {
	for _, seed := range []string{
		packSpec(nil, "", nil, false, false),
		packSpec([]string{}, "", nil, false, false),
		packSpec([]string{"  ", "\t", ""}, ".jsonl", nil, false, false),
		packSpec([]string{"~/.claude/projects"}, "", nil, false, false),
		packSpec([]string{"~/.codex/sessions"}, "", nil, true, true),
		packSpec([]string{"{dir}/state"}, DefaultSuffix, nil, false, false),
		packSpec([]string{"{dir}"}, "", []string{".jsonl", "", "   "}, false, false),
		packSpec([]string{"{dir}", "~/.x", "  ", "{dir}/a", "{dir}/b"}, "  .jsonl.zstd  ", nil, true, false),
		packSpec([]string{" "}, "", []string{" "}, false, false),
		packSpec([]string{"{dir}"}, "x", []string{"  ", ""}, false, false),
		packSpec([]string{"/abs", "{dir}/rel", "~", "~/x", `C:\Users\me\.gemini`}, " .jsonl ", []string{" .jsonl ", ".ndjson"}, true, true),
		packSpec([]string{strings.Repeat("a", 512)}, strings.Repeat(" ", 64), []string{strings.Repeat("\t", 64)}, false, false),
		packSpec([]string{"{dir}", "{dir}", "{dir}"}, "", nil, false, false),
		packSpec([]string{"{dir}"}, "\n", []string{"\n\n"}, false, false),
		"",
		"only-one-field",
	} {
		f.Add(seed)
	}

	now := time.Unix(1_700_000_000, 0)
	later := now.Add(72 * time.Hour)

	f.Fuzz(func(t *testing.T, packed string) {
		spec := unpackSpec(packed)
		patterns := specRoots(spec)
		ad, ok := specAdapter(spec)

		if ok != (len(patterns) > 0) {
			t.Fatalf("specAdapter usable=%v for %d non-blank roots out of %#v", ok, len(patterns), spec.Roots)
		}
		if !ok {
			// A spec with nothing to search is refused by handing back the
			// zero adapter, and a caller that registers it anyway must not end
			// up walking an empty pattern.
			if ad.roots != nil || ad.suffix != "" || len(ad.suffixes) != 0 || ad.parse != nil {
				t.Fatalf("refused spec built an adapter: %#v", ad)
			}
			return
		}

		if ad.parse == nil || ad.roots == nil {
			t.Fatalf("usable spec left the adapter without a parser or roots: %#v", ad)
		}
		if _, _, ok := ad.parse([]byte(`{"usage":{"input_tokens":1}}`)); !ok {
			t.Fatal("adapter parser cannot read a usage record")
		}
		if (ad.sessionCwd != nil) != spec.HeaderCwd {
			t.Fatalf("header_cwd=%v set sessionCwd=%v", spec.HeaderCwd, ad.sessionCwd != nil)
		}
		want := perMessage
		if spec.Cumulative {
			want = cumulative
		}
		if ad.kind != want {
			t.Fatalf("cumulative=%v produced kind %v", spec.Cumulative, ad.kind)
		}

		// A blank suffix matches every file under the root, so at most one of
		// the two filters is set and neither one is ever blank or padded.
		if len(ad.suffixes) > 0 {
			if ad.suffix != "" {
				t.Fatalf("suffixes set alongside suffix %q", ad.suffix)
			}
			for _, got := range ad.suffixes {
				if got == "" || got != strings.TrimSpace(got) {
					t.Fatalf("unusable suffix %q among %#v", got, ad.suffixes)
				}
			}
			if len(ad.suffixes) != countNonBlank(spec.Suffixes) {
				t.Fatalf("kept %d of %#v suffixes", len(ad.suffixes), spec.Suffixes)
			}
		} else {
			want := strings.TrimSpace(spec.Suffix)
			if want == "" {
				want = DefaultSuffix
			}
			if ad.suffix != want {
				t.Fatalf("suffix %q from %q and %#v", ad.suffix, spec.Suffix, spec.Suffixes)
			}
		}

		for _, dir := range []string{"", "/home/dev/proj", `C:\Users\me\proj`, strings.Repeat("d", 300), "{dir}", "~/x"} {
			out := ad.roots(dir, now)
			if len(out) != len(patterns) {
				t.Fatalf("dir %q produced %d roots, want %d", dir, len(out), len(patterns))
			}
			for i, got := range out {
				want := core.ExpandHome(strings.ReplaceAll(patterns[i], "{dir}", dir))
				if got != want {
					t.Fatalf("root %d of %#v is %q, want %q", i, patterns, got, want)
				}
				// A blank root is the current directory to WalkDir, and a
				// watcher never hands this function a blank one (Watch
				// resolves it to an absolute path), so a root can only come
				// back empty when the spec is a bare "{dir}" and there was no
				// directory to put in it.
				if got == "" && dir != "" {
					t.Fatalf("root %d of %#v is blank: a blank root walks the working directory", i, patterns)
				}
			}
			// A watcher re-derives the adapter on every poll, so a clock that
			// moved and a second call must both come back with the same roots.
			if again := ad.roots(dir, later); !slices.Equal(again, out) {
				t.Fatalf("dir %q produced different roots at a later instant: %q then %q", dir, out, again)
			}
		}
	})
}

func countNonBlank(in []string) int {
	n := 0
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}
