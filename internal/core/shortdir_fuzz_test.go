// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzShortDir drives the path shortener with arbitrary strings. The input is
// sender-shaped: a pushed event's note is a bare working directory a client
// chose, and it reaches ShortDir through core.ShortDir before the note is
// stored and later printed into a cell of the feed, the dashboard and the
// --json report. So what this asserts is not that the answer is a particular
// path but that the three promises the note rests on hold for every input.
//
// Home is the one that matters. A note naming a path under the operator's
// home is rewritten to ~, so the account name never reaches the report; a
// result that keeps the home's own basename, or a spelling of it, is the leak
// the rewrite exists to prevent. The other two are the shape of the answer:
// it is at most two components, and the second shortening of it is a no-op,
// which is what makes the stored note stable no matter how many times the
// pipeline renders it.
func FuzzShortDir(f *testing.F) {
	// A real directory tree, so the symlink walk in resolvePath has something
	// to resolve and a path that does not exist to walk up from. Both are
	// inputs: a directory removed mid-session and one an agent names before
	// creating it must shorten the same way as one on disk.
	// One tree for the whole target, with the home inside it, so the paths
	// checked against the home-stripping rule are real directories rather
	// than names that happen to sit under whatever HOME the run starts with.
	// Both on-disk and not-on-disk spellings are inputs: a directory removed
	// mid-session and one an agent names before creating it must shorten the
	// same way as one that exists.
	root := f.TempDir()
	home := filepath.Join(root, "home")
	deep := filepath.Join(home, "src", "toktop")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		f.Fatalf("fixture: %v", err)
	}
	var link string
	if err := os.Symlink(filepath.Join(home, "src"), filepath.Join(home, "link")); err == nil {
		link = filepath.Join(home, "link", "toktop")
	}
	missing := filepath.Join(home, "not", "there", "yet")
	away := filepath.Join(root, "other", "proj")
	if err := os.MkdirAll(away, 0o755); err != nil {
		f.Fatalf("fixture: %v", err)
	}

	for _, seed := range []string{
		"", " ", ".", "..", "/", "//", "///", "\\", `C:\`, `C:\Users\dev`,
		"project", "a/b", "a/b/c", "a/b/c/d",
		"/var/log/", "/var/log", "/var/log//", "/home/dev", "/home/dev/",
		"/home/dev/src/toktop", "/home/dev/../other",
		"/home/dev/./src", "/home//dev//x", "/home/devx", "/home/developer",
		"/Users/dev/Library/Caches", "/home/me\u0301/x", "/home/\u212a/x",
		home, deep, deep + string(filepath.Separator), home + string(filepath.Separator),
		filepath.Join(home, "src"), missing, away, away + string(filepath.Separator),
		filepath.Join(home, "..", "other", "proj"),
		"~", "~/", "~/src", "~user/x",
		"\x00", "\x00/x", "a\x00b", "\u2028/x", "\ufeff/x",
		"x" + strings.Repeat("/y", 500),
		strings.Repeat("/", 300) + "dev",
		strings.Repeat("a/", maxPathWalk+50) + "b",
		`\\server\share\deep\path`,
		"dir with space/x", "note: prose with spaces", "~/dir with space",
		strings.Repeat("\U0001F1E9", 40) + "/x",
		"café/x", "cafe\u0301/x", "e\u0301/x",
	} {
		f.Add(seed)
	}
	if link != "" {
		f.Add(link)
	}

	f.Fuzz(func(t *testing.T, dir string) {
		// HOME is pinned to the fixture, so the home-stripping rule is under
		// test rather than whatever account the run happens under, and the
		// leak check has a fixed name to look for. The home is a real
		// directory, so the symlink walk in resolvePath resolves it, and the
		// paths checked against it include one it never had to create, which
		// is the case that walk exists for.
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		got := ShortDir(dir)
		if again := ShortDir(dir); again != got {
			t.Fatalf("ShortDir(%q) is not deterministic: %q then %q", dir, got, again)
		}
		// The cut is by byte, so a shortened path can end mid-rune. That is
		// not a property of the input: invalid bytes can be cut away whole
		// ("\xe9//0" keeps the two components after the bad byte, which are
		// valid), and a valid input must not be turned into an invalid one.
		if utf8Valid(dir) && !utf8Valid(got) {
			t.Fatalf("ShortDir(%q) = %q is not valid UTF-8", dir, got)
		}
		if got == "" && dir != "" {
			t.Fatalf("ShortDir(%q) = %q emptied a non-empty path", dir, got)
		}
		// The second shortening of a shortened path is a no-op: the feed
		// stores what this returns, and the --agents report shortens again
		// when it renders. Anything else means the answer moves under the
		// reader.
		if twice := ShortDir(got); twice != got {
			t.Fatalf("ShortDir(%q) = %q is not stable, second pass gave %q", dir, got, twice)
		}
		if n := components(got); n > 2 && got != "/" {
			t.Fatalf("ShortDir(%q) = %q kept %d components", dir, got, n)
		}
		// The account name is what the rewrite exists to keep out of a
		// report, so the check is scoped to the paths that actually name it:
		// a path under home, spelled through the symlink and through a
		// directory that does not exist. A path outside home may legitimately
		// end in a component with the same name, since two directories under
		// one parent can be spelled alike, and that is a different directory.
		//
		// What the rewrite owes is the account name, not the tilde. Home
		// itself is the one input that has to read as "~": it is the only path
		// whose last two components are the account, and a checker that
		// required "~" of everything under home would reject the documented
		// answer for a nested checkout, which is the two components below the
		// home, not a tilde and one.
		for _, under := range []string{home, deep, link, missing} {
			if under == "" {
				continue
			}
			stripped := ShortDir(under)
			if base := filepath.Base(home); containsComponent(stripped, base) {
				t.Fatalf("ShortDir(%q) = %q names home's own component %q", under, stripped, base)
			}
		}
		if got := ShortDir(home); got != "~" {
			t.Fatalf("ShortDir(home) = %q, want ~", got)
		}
		// A path outside home is not rewritten, and a tilde in the result is
		// then the sender's own spelling passed through. That is correct and
		// is not checked here: "~user" names another account, and the
		// shortener resolves no shell spelling, so it has nothing to say
		// about either. What matters is the account name above, and a tilde
		// is not one.
		// lastTwoComponents is the whole rule once home is out of the way, and
		// it is checked on the pre-strip input so the two halves are pinned
		// apart rather than only in composition.
		if kept := lastTwoComponents(dir); kept != "" {
			if n := components(kept); n > 2 && kept != "/" {
				t.Fatalf("lastTwoComponents(%q) = %q kept %d components", dir, kept, n)
			}
		}
	})
}

// components counts the non-empty path components of s, treating both
// separators as one. lastTwoComponents scans for both regardless of platform,
// so the count has to as well or a backslash path reads as one component.
func components(s string) int {
	trimmed := strings.TrimRight(s, `/\`)
	if trimmed == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '/' || trimmed[i] == '\\' {
			n++
		}
	}
	return n
}

// containsComponent reports whether s holds want as a whole path component,
// on either separator. A substring test would flag "/home/developer" for the
// component "dev", which is a different account.
func containsComponent(s, want string) bool {
	if want == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == want {
			return true
		}
	}
	return false
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
