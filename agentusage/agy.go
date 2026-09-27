// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// agy (Antigravity CLI) appends one step per line to transcript.jsonl at
// ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/.
// A step carries step_index, source, type and content. It does not name the
// working directory or, usually, any tokens. A step that carries a tokens
// object or usageMetadata is counted; every other step contributes nothing.
//
// The workspace is recorded on history.jsonl at the store root, one line per
// event, with conversationId and workspace. The transcript itself never has
// that field, so the conversation id in the brain/ path is looked up there.
// A conversation the history has not mentioned yet is left undecided and
// retried: refusing it would drop a usage step that arrives after the index.

const agyHistoryFile = "history.jsonl"

// agyHistoryDepth is how far above a transcript the store root, and its
// history.jsonl, can sit. brain/<id>/.system_generated/logs is four levels.
const agyHistoryDepth = 8

func parseAgy(line []byte) (values, string, bool) {
	return parseGeminiRecord(line)
}

// agySessionCwd is the workspace history.jsonl records for the conversation
// whose transcript this path is. ok is false when the index is missing or
// does not name this conversation yet.
func agySessionCwd(path string) (string, bool) {
	id := agyConversationID(path)
	if id == "" {
		return "", false
	}
	hist := agyHistoryPath(path)
	if hist == "" {
		return "", false
	}
	return agyWorkspace(hist, id)
}

// agyConversationID is the brain/<id> directory a transcript lives under.
func agyConversationID(path string) string {
	dir := filepath.Dir(path)
	for range agyHistoryDepth {
		parent := filepath.Dir(dir)
		if filepath.Base(parent) == "brain" {
			id := filepath.Base(dir)
			if id == "" || id == "." || id == string(filepath.Separator) {
				return ""
			}
			return id
		}
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func agyHistoryPath(path string) string {
	dir := filepath.Dir(path)
	for range agyHistoryDepth {
		candidate := filepath.Join(dir, agyHistoryFile)
		st, err := os.Stat(candidate)
		if err == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

type agyHist struct {
	path  string
	size  int64
	mtime int64
	ids   map[string]string
}

var (
	agyHistMu    sync.Mutex
	agyHistCache agyHist
)

func agyWorkspace(histPath, id string) (string, bool) {
	st, err := os.Stat(histPath)
	if err != nil {
		return "", false
	}
	agyHistMu.Lock()
	defer agyHistMu.Unlock()
	c := agyHistCache
	if c.path != histPath || c.size != st.Size() || c.mtime != st.ModTime().UnixNano() {
		ids, ok := loadAgyHistory(histPath)
		if !ok {
			return "", false
		}
		c = agyHist{path: histPath, size: st.Size(), mtime: st.ModTime().UnixNano(), ids: ids}
		agyHistCache = c
	}
	ws := c.ids[id]
	if ws == "" {
		return "", false
	}
	return ws, true
}

// agyHistoryCap bounds the index read. The file is one line per prompt, and
// past this it is no longer a history this package will scan on a poll.
const agyHistoryCap = 32 << 20

func loadAgyHistory(path string) (map[string]string, bool) {
	dir := filepath.Dir(path)
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer r.Close()
	f, err := r.Open(filepath.Base(path))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, agyHistoryCap))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	ids := map[string]string{}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec struct {
			ConversationID string `json:"conversationId"`
			Workspace      string `json:"workspace"`
		}
		if err := json.Unmarshal(line, &rec); err != nil || rec.ConversationID == "" || rec.Workspace == "" {
			continue
		}
		ids[rec.ConversationID] = rec.Workspace
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return nil, false
	}
	return ids, true
}
