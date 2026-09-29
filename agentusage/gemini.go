// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Gemini CLI writes one JSONL chat per session under
// ~/.gemini/tmp/<project>/chats. A record whose type is "gemini" carries that
// turn's tokens. The CLI writes the same counters again once the turn's tool
// calls finish; that second copy is the one with toolCalls, and counting it
// would bill the turn twice. A record with no tokens is not a usage record.
//
// tokens.input already includes tokens.cached: the record's total is
// input + output + thoughts, so the cached share is not added again.
// usageMetadata is the other shape this CLI and the Gemini API have shipped,
// with the same relationship between promptTokenCount and
// cachedContentTokenCount.

const geminiProjectFile = ".project_root"

// geminiRootDepth bounds the walk from a chat log up to the project directory
// that holds .project_root. The log sits in <project>/chats/, so two levels
// is the layout; the bound is what keeps a missing file from walking out of
// the store.
const geminiRootDepth = 4

// geminiRootCap bounds the .project_root read. The file holds one directory
// path, so the cap is the same size a stored cwd gets; a longer file is not a
// path this module can use. The store is writable by the agent, and this read
// runs once per transcript per rescan, so an uncapped one is memory an
// operator's own padding holds for the whole run.
const geminiRootCap = 1 << 20

func parseGeminiRecord(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(bytes.TrimSpace(line), utf8BOM)
	var rec struct {
		Cwd       string          `json:"cwd"`
		Workspace string          `json:"workspace"`
		ToolCalls json.RawMessage `json:"toolCalls"`
		Tokens    struct {
			Input    int `json:"input"`
			Output   int `json:"output"`
			Cached   int `json:"cached"`
			Thoughts int `json:"thoughts"`
			Total    int `json:"total"`
		} `json:"tokens"`
		Usage struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
			TotalTokenCount         int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return values{}, "", false
	}
	if geminiToolCallsRepeat(rec.ToolCalls) {
		return values{}, "", false
	}
	cwd := rec.Cwd
	if cwd == "" {
		cwd = rec.Workspace
	}
	if v, ok := foldCounters(rec.Tokens.Input, rec.Tokens.Output, rec.Tokens.Thoughts, rec.Tokens.Cached, rec.Tokens.Total); ok {
		return v, cwd, true
	}
	if v, ok := foldCounters(rec.Usage.PromptTokenCount, rec.Usage.CandidatesTokenCount, rec.Usage.ThoughtsTokenCount, rec.Usage.CachedContentTokenCount, rec.Usage.TotalTokenCount); ok {
		// usageMetadata's thoughts are part of the billed output, the same
		// fold parseQwen applies to this shape. tokens.thoughts is already
		// outside tokens.output, which is why that branch leaves them apart.
		v.output = satAdd(v.output, v.thinking)
		return v, cwd, true
	}
	return values{}, "", false
}

// geminiToolCallsRepeat reports the second copy of a turn, the one written
// after its tool calls, which repeats the counters already on the first copy.
func geminiToolCallsRepeat(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("[]")) {
		return false
	}
	return raw[0] == '['
}

// geminiSessionCwd reads the project directory .project_root names. The file
// sits beside chats/, not in it, and the walk looks for the file rather than
// counting levels.
func geminiSessionCwd(path string) (string, bool) {
	dir := filepath.Dir(path)
	for range geminiRootDepth {
		if cwd, ok := readGeminiRoot(dir); ok {
			return cwd, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func readGeminiRoot(dir string) (string, bool) {
	b, ok := readRootedCapped(dir, geminiProjectFile, geminiRootCap)
	if !ok {
		return "", false
	}
	// Only the line terminator comes off. strings.TrimSpace trims every rune
	// Unicode calls a space, U+00A0 and U+3000 among them, and a directory
	// may legally end in one: a checkout at "/work/toktop\u00a0" is a
	// directory that exists, and the trimmed spelling is a different path on
	// every platform, so every record read from that chat was dropped as
	// belonging to another directory, with nothing logged. Same for a plain
	// trailing space.
	cwd := strings.Trim(string(b), "\r\n")
	if cwd == "" {
		return "", false
	}
	return cwd, true
}
