// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The agents named for this package have to be readable. crush and opencode
// are covered with the sqlite tag, because that is what links their driver.
func TestNamedFileAgentsAreReadable(t *testing.T) {
	for _, tool := range []string{"dsh", "kimi", "agy", "claude", "grok", "codex", "gemini", "qwen"} {
		if !Supported(tool) {
			t.Errorf("Supported(%s) = false", tool)
		}
		if Watch(tool, t.TempDir(), time.Now()) == nil {
			t.Errorf("Watch(%s) = nil", tool)
		}
	}
}

func TestGeminiCountsThisProjectAndSkipsTheRepeatedTurn(t *testing.T) {
	store := withStore(t, "gemini")
	work, other := t.TempDir(), t.TempDir()
	mine := filepath.Join(store, "mine", "chats")
	theirs := filepath.Join(store, "theirs", "chats")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(theirs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "mine", ".project_root"), []byte(work+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "theirs", ".project_root"), []byte(other+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := Watch("gemini", work, time.Now())
	if w == nil {
		t.Fatal("gemini watcher")
	}
	turn := `{"type":"gemini","tokens":{"input":100,"output":7,"cached":40,"thoughts":3,"total":110}}`
	again := `{"type":"gemini","toolCalls":[{"name":"read_file"}],"tokens":{"input":100,"output":7,"cached":40,"thoughts":3,"total":110}}`
	append_(t, filepath.Join(mine, "session.jsonl"), turn, again)
	append_(t, filepath.Join(theirs, "session.jsonl"),
		`{"type":"gemini","tokens":{"input":9000,"output":8000,"cached":0,"thoughts":0,"total":17000}}`)
	// A chat line with no tokens is not a usage record.
	append_(t, filepath.Join(mine, "session.jsonl"), `{"type":"user","content":"hello"}`)

	s := w.Poll()
	if s.Input != 100 || s.Output != 7 || s.Thinking != 3 {
		t.Fatalf("sample = %+v, want input 100 output 7 thinking 3", s)
	}
}

func TestGeminiUsageMetadataIsCounted(t *testing.T) {
	store := withStore(t, "gemini")
	work := t.TempDir()
	proj := filepath.Join(store, "proj", "chats")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "proj", ".project_root"), []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}
	w := Watch("gemini", work, time.Now())
	append_(t, filepath.Join(proj, "session.jsonl"),
		`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":1,"cachedContentTokenCount":3,"totalTokenCount":15}}`)
	s := w.Poll()
	// thoughts fold into output, and the cached share is already in the prompt
	// because the total equals prompt + candidates + thoughts.
	if s.Input != 10 || s.Output != 5 || s.Thinking != 1 {
		t.Fatalf("sample = %+v, want input 10 output 5 thinking 1", s)
	}
}

func TestAgyCountsUsageAndIgnoresAStepWithoutIt(t *testing.T) {
	store := withStore(t, "agy")
	work, other := t.TempDir(), t.TempDir()
	w := Watch("agy", work, time.Now())
	if w == nil {
		t.Fatal("agy watcher")
	}
	if err := os.MkdirAll(filepath.Join(store, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	append_(t, filepath.Join(store, "mine", "transcript.jsonl"),
		`{"workspace":`+jsonPath(work)+`}`,
		`{"type":"PLANNER_RESPONSE","content":"looking"}`,
		`{"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":6,"thoughtsTokenCount":2,"cachedContentTokenCount":5,"totalTokenCount":28}}`)
	append_(t, filepath.Join(store, "other", "transcript.jsonl"),
		`{"workspace":`+jsonPath(other)+`}`,
		`{"usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":400,"totalTokenCount":4400}}`)
	s := w.Poll()
	if s.Input != 20 || s.Output != 8 || s.Thinking != 2 {
		t.Fatalf("sample = %+v, want input 20 output 8 thinking 2", s)
	}
}

func TestAgyFileWithoutUsageContributesNothing(t *testing.T) {
	store := withStore(t, "agy")
	work := t.TempDir()
	w := Watch("agy", work, time.Now())
	append_(t, filepath.Join(store, "transcript.jsonl"),
		`{"workspace":`+jsonPath(work)+`,"type":"PLANNER_RESPONSE","content":"no tokens here"}`)
	if s := w.Poll(); !s.Empty() {
		t.Fatalf("a step with no usage counted %+v", s)
	}
}

func TestGrokUsageIsThisProject(t *testing.T) {
	store := withStore(t, "grok")
	work, other := t.TempDir(), t.TempDir()
	w := Watch("grok", work, time.Now())
	if w == nil {
		t.Fatal("grok watcher")
	}
	writeGrokUsage(t, store, work, 80, 9, 30, 4)
	writeGrokUsage(t, store, other, 5000, 400, 0, 0)
	s := w.Poll()
	if s.Input != 80 || s.Output != 9 || s.Thinking != 4 {
		t.Fatalf("sample = %+v, want input 80 output 9 thinking 4", s)
	}

	// A rewrite of the same file is the new total, not a second copy of it.
	writeGrokUsage(t, store, work, 100, 14, 40, 6)
	s = w.Poll()
	if s.Input != 100 || s.Output != 14 || s.Thinking != 6 {
		t.Fatalf("after rewrite sample = %+v, want input 100 output 14 thinking 6", s)
	}
}

func TestGrokUsageWithoutCountersContributesNothing(t *testing.T) {
	store := withStore(t, "grok")
	work := t.TempDir()
	w := Watch("grok", work, time.Now())
	dir := filepath.Join(store, grokDirName(work), "sess")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := w.Poll(); !s.Empty() {
		t.Fatalf("empty usage.json counted %+v", s)
	}
}

func writeGrokUsage(t *testing.T, store, dir string, in, out, cached, think int) {
	t.Helper()
	sess := filepath.Join(store, grokDirName(dir), "sess")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "{\n  \"session\": {\n    \"inputTokens\": " + itoa(in) +
		",\n    \"outputTokens\": " + itoa(out) +
		",\n    \"cachedReadTokens\": " + itoa(cached) +
		",\n    \"cacheCreationTokens\": 0,\n    \"reasoningTokens\": " + itoa(think) +
		",\n    \"totalTokens\": " + itoa(in+out) + "\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(sess, "usage.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
