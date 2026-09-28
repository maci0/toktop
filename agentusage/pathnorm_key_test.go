// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// DirKey is what a caller keys a map by, SameDir what it compares with, and a
// caller doing both needs the two to agree: a map keyed by DirKey holds one
// entry exactly when SameDir calls the two spellings one directory. The test
// states that in terms of SameDir, so it holds on every platform, and the
// per-platform files pin what folding each one does on top of it.
func TestDirKeyAgreesWithSameDir(t *testing.T) {
	spellings := [][2]string{
		{"/Users/dev/proj", "/Users/dev/proj"},
		{"/Users/dev/proj/", "/Users/dev/proj"},
		{"/Users/dev/café", "/Users/dev/café"}, // NFC against NFD
		{"/Users/Foo/proj", "/Users/foo/proj"},
		{"/Users/dev/proj", "/Users/dev/other"},
	}
	for _, s := range spellings {
		same, oneKey := SameDir(s[0], s[1]), DirKey(s[0]) == DirKey(s[1])
		if same != oneKey {
			if same {
				t.Errorf("SameDir(%q, %q) is true but DirKey differs: a map keyed by DirKey would count one directory twice", s[0], s[1])
			} else {
				t.Errorf("DirKey(%q) == DirKey(%q) but SameDir is false: a map keyed by DirKey would count one directory as two", s[0], s[1])
			}
		}
	}
}

// Folding has to settle in one pass: a key built from a directory that a
// caller already folded must be the same key again.
func TestDirKeyIsIdempotent(t *testing.T) {
	for _, p := range []string{"/Users/dev/proj/", "/Users/Dev/proj", "C:\\Users\\dev\\proj", "/home/dev/café"} {
		once := DirKey(p)
		if twice := DirKey(once); twice != once {
			t.Errorf("DirKey(%q) = %q, then DirKey of that = %q", p, once, twice)
		}
	}
}

// TestSameDirRejectsLostBytes pins the guard on a recorded directory that has
// lost bytes in transit. encoding/json replaces every ill-formed byte and
// every unpaired surrogate escape with U+FFFD, so two directories Linux keeps
// apart, one ending in the raw byte 0xff and one in 0xfe, reach the comparison
// as the same string. Matching it would bill one checkout's tokens to the
// other, which is the misattribution pathnorm_other.go's exact comparison
// exists to prevent, so the only safe answer is to match nothing.
func TestSameDirRejectsLostBytes(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "proj")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{dir: real, dirVerdict: map[string]dirVerdict{}}

	// One real byte each, and both are invalid UTF-8, so a rune walk decodes
	// either to U+FFFD. The first is what the JSON decoder would have handed
	// sameDir after decoding the second's transcript.
	if w.sameDir(filepath.Join(dir, "proj\xff")) {
		t.Error("sameDir matched a directory whose bytes the decode had collapsed")
	}
	if w.sameDir(filepath.Join(dir, "proj\uFFFD")) {
		t.Error("sameDir matched a recorded directory carrying a replacement character")
	}
	// The unpaired surrogate a JSON "\\ud800" escape decodes to reaches
	// sameDir as the three bytes a UTF-8 encoder would never produce, and a
	// rune walk decodes them to U+FFFD as well.
	if w.sameDir(filepath.Join(dir, "proj") + string([]byte{0xed, 0xa0, 0x80})) {
		t.Error("sameDir matched a recorded directory carrying an unpaired surrogate")
	}
	if !w.sameDir(real) {
		t.Error("sameDir stopped matching the directory the watcher watches")
	}
	// An accented name is intact text and must still match, which is what
	// keeps the guard from refusing every non-ASCII path.
	accented := filepath.Join(dir, "café")
	if err := os.Mkdir(accented, 0o755); err != nil {
		t.Fatal(err)
	}
	w.dir = accented
	w.dirVerdict = map[string]dirVerdict{}
	if !w.sameDir(accented) {
		t.Error("sameDir stopped matching an accented directory name")
	}
}

// TestSameDirRetriesACwdThatDidNotResolve pins the retry on an unresolvable
// recorded directory. EvalSymlinks fails on a path the store has not created
// yet, and memoizing that failure as "not this project" decides the question
// for the watcher's life: every session of the checkout that appears next goes
// uncounted with nothing on screen to say why.
func TestSameDirRetriesACwdThatDidNotResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege windows does not grant by default")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	cur := time.Now()
	w := &Watcher{dir: real, dirVerdict: map[string]dirVerdict{}, now: func() time.Time { return cur }}

	if w.sameDir(link) {
		t.Fatal("sameDir matched a path that does not exist")
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Inside the retry window the memo still answers, so the window is what
	// bounds the symlink walk rather than every record of every session.
	cur = cur.Add(dirVerdictRetry - time.Second)
	if w.sameDir(link) {
		t.Error("sameDir re-walked a cwd inside the retry window")
	}
	cur = cur.Add(2 * time.Second)
	if !w.sameDir(link) {
		t.Error("sameDir kept refusing a cwd that resolved to the watched directory")
	}
}
