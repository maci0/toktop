// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzAgyIndex drives the two files that map an agy conversation to its
// workspace with arbitrary bytes. Neither file is written by this package, and
// last_conversations.json can name one conversation under several workspaces,
// so an accepted index must record a workspace for every id it returns, never
// an empty one, and must come back identical when the same bytes are read
// again: a workspace chosen by map order would move between reads and hand the
// same conversation to a different process.
func FuzzAgyIndex(f *testing.F) {
	f.Add("", "")
	f.Add(`{"conversationId":"c1","workspace":"/home/dev/a"}`+"\n", "")
	f.Add(`{"conversationId":"c1","workspace":"/a"}
{"conversationId":"c1","workspace":"/b"}
`, "")
	f.Add(`{"conversationId":"","workspace":"/a"}`, `{"":"c1"}`)
	f.Add("not json", "not json")
	f.Add("", `{"\/a":"c1","\/b":"c1","\/c":"c1"}`)
	f.Add("", `{"\/a":"c1","\/b":"c2","\/c":""}`)
	f.Add("[]", "{}")
	f.Add("\xef\xbb\xbf"+`{"conversationId":"c1","workspace":"/a"}`, "\xef\xbb\xbf"+`{"\/z":"c2"}`)
	f.Add(`{"conversationId":"c1","workspace":`+strings.Repeat("[", 32)+strings.Repeat("]", 32)+`}`, `{"\/a":1}`)

	f.Fuzz(func(t *testing.T, history, last string) {
		root := t.TempDir()
		lastPath := filepath.Join(root, filepath.Join(agyLastDir, agyLastFile))
		histPath := filepath.Join(root, agyHistoryFile)
		writeAgyFuzzFile(t, histPath, history)
		writeAgyFuzzFile(t, lastPath, last)

		first, ok := agyIndex(root)
		// Rewriting the same bytes restamps the files, so the second read is
		// a second parse rather than the cached answer to the first.
		writeAgyFuzzFile(t, histPath, history)
		writeAgyFuzzFile(t, lastPath, last)
		second, ok2 := agyIndex(root)
		if ok != ok2 || !stringMapsEqual(first, second) {
			t.Fatalf("agyIndex not deterministic for history %q and last %q:\nfirst  %v %v\nsecond %v %v", history, last, ok, first, ok2, second)
		}
		if !ok {
			return
		}
		for id, ws := range first {
			if id == "" || ws == "" {
				t.Fatalf("agyIndex: empty id or workspace in %v for history %q and last %q", first, history, last)
			}
		}

		// The readers below agyIndex run unchecked by it:
		// last_conversations.json is a JSON object whose iteration order Go
		// randomizes per range, so an unstable winner over a repeated id
		// shows here and nowhere else.
		hist, histOK := loadAgyHistory(histPath)
		if histOK && hist == nil {
			t.Fatalf("loadAgyHistory: nil index for a readable file, history %q", history)
		}
		// A file that is not a JSON object is refused, and refusing it has to
		// be as repeatable as reading it: a caller retries on the next poll.
		first, firstOK := loadAgyLast(lastPath)
		for run := range 8 {
			ids, lastOK := loadAgyLast(lastPath)
			if lastOK != firstOK || !stringMapsEqual(ids, first) {
				t.Fatalf("loadAgyLast run %d disagrees for last %q:\nfirst %v %v\nrun   %v %v", run, last, firstOK, first, lastOK, ids)
			}
		}
		if _, missing := loadAgyHistory(filepath.Join(root, "not-here.jsonl")); missing {
			t.Fatalf("loadAgyHistory reported a file that is not there")
		}
	})
}

// FuzzGrokSessionCwd drives the reader that recovers a working directory from
// the percent-encoded session directory name grok writes for it. The two are
// one encoding, so a directory the encoder names must decode back to itself,
// and only an absolute one: a session is only ever written for a working
// directory, so a relative or bare name read out of the store is a record
// naming a directory that was never one.
func FuzzGrokSessionCwd(f *testing.F) {
	dirs := []string{
		"/home/dev/proj", "/", "/tmp/a b", "/tmp/ünïcode", "/C:/Users/dev",
		"/tmp/100%", "/tmp/q?a#b", "/tmp/tab\there", "/tmp/emoji/🎉",
		"/tmp/" + strings.Repeat("a", 300), "/tmp/../escape", "relative/path", "", ".",
	}
	for _, d := range dirs {
		f.Add(d)
	}

	f.Fuzz(func(t *testing.T, dir string) {
		name := grokDirName(dir)
		if name == "" {
			return
		}
		// Concatenated rather than joined, so a name the encoder wrote as one
		// component survives as one and the reader is given what was written.
		cwd, ok := grokSessionCwd(name + "/session-id/updates.jsonl")
		if !filepath.IsAbs(dir) {
			if ok {
				t.Fatalf("grokSessionCwd: read %q from %q, which is not a working directory", cwd, name)
			}
			return
		}
		if !ok {
			t.Fatalf("grokSessionCwd refused a directory its own encoder wrote: %q", dir)
		}
		if cwd != dir {
			t.Fatalf("grok round trip: encoded %q to %q and read back %q", dir, name, cwd)
		}
	})
}

// FuzzGrokSessionPath drives the same reader from the session path rather than
// from a directory, so the decoder is reached with names no encoder produced:
// percent escapes the CLI never writes, half-decodable ones, and path
// components that try to climb out of the store.
func FuzzGrokSessionPath(f *testing.F) {
	paths := []string{
		"/home/dev/.grok/sessions/%2Fhome%2Fdev%2Fproj/id/updates.jsonl",
		"/store/%2Fetc%2Fpasswd/id/updates.jsonl",
		"/store/..%2F..%2Fetc/id/updates.jsonl",
		"/store/%2F/id/updates.jsonl",
		"/store/%zz/id/updates.jsonl",
		"/store/%/id/updates.jsonl",
		"/store/%00/id/updates.jsonl",
		"/store//id/updates.jsonl",
		"/store/./id/updates.jsonl",
		"/store/%2E%2E/id/updates.jsonl",
		"updates.jsonl",
		"",
		"/store/" + strings.Repeat("%2F", 200) + "/id/updates.jsonl",
		"/store/" + strings.Repeat("a", 300) + "/id/updates.jsonl",
	}
	for _, p := range paths {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, path string) {
		cwd, ok := grokSessionCwd(path)
		if !ok {
			return
		}
		if cwd == "" || !filepath.IsAbs(cwd) {
			t.Fatalf("grokSessionCwd: %q is not a working directory for %q", cwd, path)
		}
		if strings.ContainsRune(cwd, 0) {
			t.Fatalf("grokSessionCwd: %q carries a NUL byte for %q", cwd, path)
		}
		// A directory is only ever spelled by this encoder, so one that does
		// not survive a second pass through it came out of a store this
		// package did not write. Attributing a session to a directory the
		// tool never recorded is worse than leaving it undecided.
		again, err := url.PathUnescape(grokDirName(cwd))
		if err != nil || again != cwd {
			t.Fatalf("grokSessionCwd: read %q from %q, which this package never encodes", cwd, path)
		}
	})
}

func writeAgyFuzzFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
