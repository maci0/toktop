// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"strings"
	"testing"
)

// FuzzPathSlug drives the working-directory-to-directory-name mapping over
// arbitrary paths. The path comes from a transcript or a running process, so
// separators, drive letters, dot runs and NUL bytes are all attacker-chosen,
// and the slug is joined onto a store path to find the session directories:
// anything that survives as a path component, or that still names a parent,
// is a session directory the reader was not meant to open.
//
// What has to hold: the slug is one component with no separator and no colon
// left in it, it is never "." or "..", it is stable under a second pass, and
// the harness directory name is either empty or the slug wrapped in the
// markers, so it names the same one directory it did the first time.
func FuzzPathSlug(f *testing.F) {
	for _, seed := range []string{
		"",
		".",
		"..",
		"../..",
		"/",
		"//",
		"///",
		"/tmp/work",
		"/home/dev/Desktop/fastrouter",
		`C:\Users\me\proj`,
		`..\..\Windows`,
		"./a/../..",
		"/tmp/./work/",
		"a:b",
		":",
		"C:",
		"\\",
		"\x00",
		"a\x00/b",
		"-",
		"--a--",
		"...",
		".hidden",
		"日本語",
		strings.Repeat("../", 500),
		strings.Repeat("a/", 1000) + "..",
		"/tmp/" + strings.Repeat("x/", 500),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, dir string) {
		slug := pathSlug(dir)

		if strings.ContainsAny(slug, "/\\:") {
			t.Fatalf("pathSlug(%q) = %q, which still carries a separator or a drive colon", dir, slug)
		}
		if slug == "." || slug == ".." {
			t.Fatalf("pathSlug(%q) = %q, a component that still names a directory", dir, slug)
		}
		if again := pathSlug(slug); again != slug {
			t.Fatalf("pathSlug is not stable: pathSlug(%q) = %q, which maps again to %q", dir, slug, again)
		}

		name := dshDirName(dir)
		switch {
		case slug == "" && name != "":
			t.Fatalf("dshDirName(%q) = %q for a directory with no slug", dir, name)
		case slug != "" && name != "--"+slug+"--":
			t.Fatalf("dshDirName(%q) = %q, want the slug %q wrapped in markers", dir, name, slug)
		}
		// The name is joined onto a store path in place of the slug, so it
		// has to stay a single component that lands in the same place.
		if strings.ContainsAny(name, "/\\:") || name == "." || name == ".." {
			t.Fatalf("dshDirName(%q) = %q, which is not one safe component", dir, name)
		}
	})
}
