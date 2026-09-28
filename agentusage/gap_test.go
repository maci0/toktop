// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPathSlugMatchesTheStoresOnDisk(t *testing.T) {
	// Shaped like the directory names a ~/.dsh/sessions and ~/.cursor/projects
	// actually hold, with the account and the machine they were read from left
	// out: a committed fixture is published with the repository, and a home
	// path in it names whoever ran the session that produced it. The slug is
	// the absolute path with separators folded to '-', and dsh wraps that in
	// '--'.
	const dir = "/home/dev/Desktop/vllm-spark-0731"
	if got, want := pathSlug(dir), "home-dev-Desktop-vllm-spark-0731"; got != want {
		t.Fatalf("pathSlug = %q, want %q", got, want)
	}
	if got, want := dshDirName("/home/dev/Desktop/fastrouter"), "--home-dev-Desktop-fastrouter--"; got != want {
		t.Fatalf("dshDirName = %q, want %q", got, want)
	}
	// A drive letter cannot remain a colon: that is not a directory name.
	if got := pathSlug(`C:\Users\me\proj`); got != "C--Users-me-proj" {
		t.Fatalf("pathSlug(windows) = %q", got)
	}
}

func TestDshNativeSessionCountsOnlyThisProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	other := filepath.Join(home, "other")
	w := Watch("dsh", work, time.Now())
	if w == nil {
		t.Fatal("dsh watcher")
	}
	// No session header. The project directory is the attribution, which is
	// how ~/.dsh-native/sessions is written.
	writeDshNative(t, home, work, dshMessage(8, 2, 20))
	writeDshNative(t, home, other, dshMessage(900, 0, 900))
	s := w.Poll()
	if s.Output != 8 || s.Thinking != 2 || s.Input != 20 {
		t.Fatalf("sample = %+v, want output 8 thinking 2 input 20", s)
	}
}

func TestDshWebCountsEveryProjectItWrites(t *testing.T) {
	if !dshHosts([]string{"bun", "dsh", "web"}) {
		t.Fatal("bun dsh web was not recognized as the server")
	}
	if dshHosts([]string{"bun", "dsh"}) {
		t.Fatal("a one-project dsh run was treated as the server")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	server := filepath.Join(home, "harness")
	proj := filepath.Join(home, "proj")
	// The server's cwd is the harness. The sessions it fills belong to
	// other projects. A project watch must not see those; the server must.
	wide := (Process{Tool: "dsh", Dir: server, AllDirs: true}).Watch(time.Now())
	narrow := Watch("dsh", server, time.Now())
	if wide == nil || narrow == nil {
		t.Fatal("dsh watcher")
	}
	writeDshSession(t, home, proj, dshMessage(8, 1, 20))
	writeDshSession(t, home, server, dshMessage(3, 0, 4))
	if s := wide.Poll(); s.Output != 11 || s.Input != 24 || s.Thinking != 1 {
		t.Fatalf("web sample = %+v, want output 11 input 24 thinking 1", s)
	}
	if s := narrow.Poll(); s.Output != 3 || s.Input != 4 || s.Thinking != 0 {
		t.Fatalf("project sample = %+v, want output 3 input 4", s)
	}
}

func writeDshSession(t *testing.T, home, dir, line string) {
	t.Helper()
	sess := filepath.Join(home, ".dsh", "sessions", dshDirName(dir), "sess")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(sess, "session.jsonl"), line)
}

func TestDshRootsNamesBothStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	name := dshDirName(work)
	got := dshRoots(work)
	want := []string{
		filepath.Join(home, ".dsh", "sessions", name),
		filepath.Join(home, ".dsh-native", "sessions", name),
	}
	if len(got) != len(want) {
		t.Fatalf("dshRoots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dshRoots[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func writeDshNative(t *testing.T, home, dir, line string) {
	t.Helper()
	sess := filepath.Join(home, ".dsh-native", "sessions", dshDirName(dir), "sess")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(sess, "session.jsonl"), line)
}

func TestPrimeSessionCountsCacheForItsOwnCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	other := filepath.Join(home, "other")
	w := Watch("prime-agent", work, time.Now())
	if w == nil {
		t.Fatal("prime-agent watcher")
	}
	dir := filepath.Join(home, ".prime", "agent", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// cacheRead is the billed cached prompt. input is only the uncached
	// remainder. Another session's header keeps its tokens out.
	append_(t, filepath.Join(dir, "mine.jsonl"),
		`{"type":"session","id":"m","cwd":`+jsonPath(work)+`}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":40,"output":7,"cacheRead":1000,"cacheWrite":3,"totalTokens":1050}}}`)
	append_(t, filepath.Join(dir, "theirs.jsonl"),
		`{"type":"session","id":"t","cwd":`+jsonPath(other)+`}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":9,"output":900,"cacheRead":5000,"cacheWrite":0,"totalTokens":5909}}}`)
	s := w.Poll()
	if s.Input != 1043 || s.Output != 7 {
		t.Fatalf("sample = %+v, want input 1043 output 7", s)
	}
}

func TestOmpSessionCountsCacheForItsOwnCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	other := filepath.Join(home, "other")
	w := Watch("omp", work, time.Now())
	if w == nil {
		t.Fatal("omp watcher")
	}
	dir := filepath.Join(home, ".omp", "agent", "sessions", "-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(dir, "session.jsonl"),
		`{"type":"session","id":"m","cwd":`+jsonPath(work)+`}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":10,"output":4,"cacheRead":50,"cacheWrite":1,"totalTokens":65}}}`)
	otherDir := filepath.Join(home, ".omp", "agent", "sessions", "-other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(otherDir, "session.jsonl"),
		`{"type":"session","id":"t","cwd":`+jsonPath(other)+`}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":1,"output":800,"cacheRead":9,"cacheWrite":0,"totalTokens":810}}}`)
	s := w.Poll()
	if s.Input != 61 || s.Output != 4 {
		t.Fatalf("sample = %+v, want input 61 output 4", s)
	}
}

func TestCursorAgentCountsUsageForThisProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	other := filepath.Join(home, "other")
	w := Watch("cursor-agent", work, time.Now())
	if w == nil {
		t.Fatal("cursor-agent watcher")
	}
	writeCursor(t, home, work, `{"type":"assistant","message":{"usage":{"input_tokens":20,"cache_read_input_tokens":5,"output_tokens":8,"output_tokens_details":{"thinking_tokens":3}}}}`)
	writeCursor(t, home, other, `{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":700}}}`)
	// A transcript line with no usage contributes nothing.
	writeCursor(t, home, work, `{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)
	s := w.Poll()
	if s.Input != 25 || s.Output != 8 || s.Thinking != 3 {
		t.Fatalf("sample = %+v, want input 25 output 8 thinking 3", s)
	}
}

func writeCursor(t *testing.T, home, dir, line string) {
	t.Helper()
	sess := filepath.Join(home, ".cursor", "projects", pathSlug(dir), "agent-transcripts", "chat")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(sess, "chat.jsonl"), line)
}
