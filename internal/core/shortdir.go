// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"strings"
)

// ShortDir keeps the last two components of a working directory, which is
// what identifies a checkout without filling the row. A path under the
// operator's home is rewritten with ~ first, so a username sitting in those
// last two components (home itself, or a project directly in it) never
// becomes the note. Separators are folded to '/' so a Windows path is
// shortened the same way as a Unix one.
//
// Every place a working directory becomes a note goes through this, watched
// locally and pushed to the ingest endpoint alike: a directory name above
// the checkout is where a client's name and a project index live, and the
// two components below it are all the feed needs.
func ShortDir(dir string) string {
	if dir == "" {
		return ""
	}
	if stripped, ok := stripHome(dir); ok {
		dir = stripped
	}
	return lastTwoComponents(dir)
}

func stripHome(dir string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	cleanDir, cleanHome := resolvePath(dir), resolvePath(home)
	rel, err := filepath.Rel(cleanHome, cleanDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "~", true
	}
	return "~/" + filepath.ToSlash(rel), true
}

// maxPathWalk bounds how many ancestors resolvePath climbs. A path of
// one-byte components would otherwise recurse thousands of times; a real
// directory is far shorter.
const maxPathWalk = 256

// resolvePath cleans p and resolves the symlinks in it. A path that is not on
// disk resolves too: symlinks are evaluated on the deepest ancestor that does
// exist and the rest is appended, so a directory removed mid-session (or one
// an agent names before creating it) still compares equal to the same
// directory spelled another way.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	var missing []string
	cur := p
	for range maxPathWalk {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return joinMissing(resolved, missing)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return joinMissing(cur, missing)
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
	return joinMissing(cur, missing)
}

// joinMissing rebuilds a path from an existing ancestor and the components
// that did not resolve, deepest first as resolvePath collected them.
func joinMissing(root string, missing []string) string {
	if len(missing) == 0 {
		return root
	}
	parts := make([]string, 0, 1+len(missing))
	parts = append(parts, root)
	for i := len(missing) - 1; i >= 0; i-- {
		parts = append(parts, missing[i])
	}
	return filepath.Join(parts...)
}

func lastTwoComponents(dir string) string {
	dir = filepath.ToSlash(dir)
	cut := 0
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			cut++
			if cut == 2 {
				return dir[i+1:]
			}
		}
	}
	return dir
}
