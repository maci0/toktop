// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"os"
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

type semver struct {
	major int
	minor int
	patch int
	extra string
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

func findChangelogPath() string {
	candidates := []string{
		"../../CHANGELOG.md",
		"../CHANGELOG.md",
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
