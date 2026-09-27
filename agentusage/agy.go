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
// The workspace is recorded in two indexes at the store root, never on the
// step. history.jsonl is one line per event, with conversationId and
// workspace, and the last line for an id wins. cache/last_conversations.json
// maps a workspace to its current conversation id, including conversations
// the history log never names. The brain/<id> directory is looked up in
// history first, then in last_conversations. A conversation neither index
// has mentioned yet is left undecided and retried: refusing it would drop a
// usage step that arrives after the index.

const (
	agyHistoryFile = "history.jsonl"
	agyLastDir     = "cache"
	agyLastFile    = "last_conversations.json"
)

// agyHistoryDepth is how far above a transcript the store root can sit.
// brain/<id>/.system_generated/logs is four levels.
const agyHistoryDepth = 8

func parseAgy(line []byte) (values, string, bool) {
	return parseGeminiRecord(line)
}

// agySessionCwd is the workspace either index records for the conversation
// whose transcript this path is. ok is false when neither index names this
// conversation yet.
func agySessionCwd(path string) (string, bool) {
	id := agyConversationID(path)
	if id == "" {
		return "", false
	}
	root := agyStoreRoot(path)
	if root == "" {
		return "", false
	}
	ids, ok := agyIndex(root)
	if !ok {
		return "", false
	}
	ws := ids[id]
	if ws == "" {
		return "", false
	}
	return ws, true
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

// agyStoreRoot is the directory that holds history.jsonl or
// cache/last_conversations.json above this transcript.
func agyStoreRoot(path string) string {
	dir := filepath.Dir(path)
	for range agyHistoryDepth {
		if agyHasIndex(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func agyHasIndex(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, agyHistoryFile)); err == nil && !st.IsDir() {
		return true
	}
	st, err := os.Stat(filepath.Join(dir, agyLastDir, agyLastFile))
	return err == nil && !st.IsDir()
}

type agyStamp struct {
	size  int64
	mtime int64
	ok    bool
}

type agyHist struct {
	root string
	hist agyStamp
	last agyStamp
	ids  map[string]string
}

var (
	agyHistMu    sync.Mutex
	agyHistCache agyHist
)

func agyFileStamp(path string) agyStamp {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return agyStamp{}
	}
	return agyStamp{size: st.Size(), mtime: st.ModTime().UnixNano(), ok: true}
}

// agyIndex maps a conversation id to the workspace that owns it. A line in
// history.jsonl wins when last_conversations.json names the same id. A file
// that is present but cannot be read is left out of the cache, so the next
// poll tries it again. ok is false when neither index could be read.
func agyIndex(root string) (map[string]string, bool) {
	histPath := filepath.Join(root, agyHistoryFile)
	lastPath := filepath.Join(root, agyLastDir, agyLastFile)
	hs, ls := agyFileStamp(histPath), agyFileStamp(lastPath)
	agyHistMu.Lock()
	defer agyHistMu.Unlock()
	c := agyHistCache
	if c.root == root && c.hist == hs && c.last == ls && c.ids != nil {
		return c.ids, true
	}
	ids := map[string]string{}
	failed := false
	if hs.ok {
		loaded, ok := loadAgyHistory(histPath)
		if !ok {
			failed = true
		} else {
			for id, ws := range loaded {
				ids[id] = ws
			}
		}
	}
	if ls.ok {
		loaded, ok := loadAgyLast(lastPath)
		if !ok {
			failed = true
		} else {
			for id, ws := range loaded {
				if _, seen := ids[id]; !seen {
					ids[id] = ws
				}
			}
		}
	}
	if !hs.ok && !ls.ok {
		return nil, false
	}
	if !failed {
		agyHistCache = agyHist{root: root, hist: hs, last: ls, ids: ids}
	}
	return ids, true
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

// agyLastCap bounds the current-conversation index. Past this the JSON object
// is not parsed: a truncated object would drop the ids at the end, which are
// the ones this file is read for.
const agyLastCap = 32 << 20

// loadAgyLast inverts cache/last_conversations.json, workspace path to
// conversation id, into conversation id to workspace.
func loadAgyLast(path string) (map[string]string, bool) {
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
	b, err := io.ReadAll(io.LimitReader(f, agyLastCap+1))
	if err != nil || len(b) > agyLastCap {
		return nil, false
	}
	var raw map[string]string
	if err := json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), &raw); err != nil {
		return nil, false
	}
	ids := make(map[string]string, len(raw))
	for ws, id := range raw {
		if ws == "" || id == "" {
			continue
		}
		if _, seen := ids[id]; seen {
			continue
		}
		ids[id] = ws
	}
	return ids, true
}
