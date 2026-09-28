// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Kimi Code CLI writes one event log per session and per agent:
//
//	~/.kimi-code/sessions/<workDirKey>/<session>/agents/<agentId>/wire.jsonl
//
// A model call appends one `usage.record` event carrying that call's own
// counts, so they are added up. The fields are the CLI's own canonical form,
// mapped from whichever provider answered: inputOther is uncached prompt,
// inputCacheRead and inputCacheCreation are the cached shares, and output is
// everything generated (the CLI reports no reasoning share of its own, and a
// provider that bills thinking counts it as output).
//
// The store is machine-wide, one directory per working directory, and holds
// hundreds of thousands of files, so it is never walked whole: the directory a
// working directory writes to is derived from its name.

// kimiUsage is one model call's counts, as usage.record spells them.
type kimiUsage struct {
	InputOther         int `json:"inputOther"`
	Output             int `json:"output"`
	InputCacheRead     int `json:"inputCacheRead"`
	InputCacheCreation int `json:"inputCacheCreation"`
}

// parseKimi reads one line of a Kimi Code CLI wire log. Every usage.record is
// one call's own usage; the CLI's own session total is the sum of them, so
// they are added up rather than rebased. token_counting.measured records the
// context size beside them and is not usage, so it is not read.
func parseKimi(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(line, utf8BOM)
	var rec struct {
		Type  string    `json:"type"`
		Usage kimiUsage `json:"usage"`
	}
	if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "usage.record" {
		return values{}, "", false
	}
	u := rec.Usage
	out := counter(u.Output)
	// Billed prompt tokens: cached input was charged for too, the same fold
	// parseClaude and parseDsh apply.
	in := satAdd(counter(u.InputOther), satAdd(counter(u.InputCacheRead), counter(u.InputCacheCreation)))
	v := values{output: out, input: in, total: satAdd(in, out)}
	if !v.present() {
		return values{}, "", false
	}
	return v, "", true
}

// KimiStorePath is the directory kimi's session logs are read from, after
// KIMI_CODE_HOME is applied. Exported so the startup warning that names a
// variable pointing at no session store resolves the path the same way the
// reader does, the way logcfg.LevelEnv and remote.PasswordEnv are shared with
// the top-level command.
func KimiStorePath() string { return kimiStore() }

// kimiStore is the directory Kimi Code CLI keeps its sessions in.
//
// KIMI_CODE_HOME is honored only when absolute, the rule the XDG base
// directories and GAUNTLET_HOME get: a relative value would place the store
// under whatever directory the run started in, where it is simply empty and
// every kimi session reads as an agent producing no tokens.
func kimiStore() string {
	if h := os.Getenv("KIMI_CODE_HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, "sessions")
	}
	return home(".kimi-code", "sessions")
}

// kimiWorkDirKey is the hash Kimi Code CLI names a working directory's session
// directory by: the first twelve hex digits of the SHA-256 of the path. A slug
// of that path is spelled in front of it, and nothing here reconstructs the
// slug: only the hash has to match.
func kimiWorkDirKey(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:6])
}

// kimiRoots lists the session directories this working directory owns. One
// directory per project is what makes this derivable at all, and reading the
// store's own listing, not the store, is what keeps a watcher's walk to a
// project's sessions instead of the machine's: the store holds thousands of
// projects and walking it takes minutes, while its listing is one directory.
//
// Several spellings are tried (dirVariants), because a path with an accent or
// a decomposable character hashes differently in each Unicode form while the
// file system treats them as one directory, and the agent hashes whichever
// form it was started with.
func kimiRoots(dir string, now time.Time) []string {
	store := kimiStore()
	names := kimiStoreDirs(store, now)
	if len(names) == 0 {
		return nil // no store yet: nothing to read, and nothing to report either
	}
	keys := make(map[string]bool, 2)
	for _, s := range dirSpellings(dir) {
		keys[kimiWorkDirKey(s)] = true
	}
	var out []string
	for _, name := range names {
		i := strings.LastIndexByte(name, '_')
		if i < 0 || !keys[name[i+1:]] {
			continue
		}
		out = append(out, filepath.Join(store, name))
	}
	return out
}

// kimiSessionCwd reads the working directory a session was started in, from
// the state.json the wire log's session directory holds. An unreadable or
// unwritten state.json yields no verdict, so the transcript is retried on a
// later poll rather than being refused for good.
//
// The wire log sits at <session>/agents/<agentId>/wire.jsonl, so the session
// directory is above the agents/ directory rather than beside it. The walk up
// is bounded and looks for the file instead of counting levels, since how deep
// the agent id nests is the CLI's choice: a fixed count reads the wrong
// directory the moment it changes, and a session with no state.json anywhere
// then reports nothing forever. Each candidate is opened through os.Root, so a
// state.json swapped for a symlink out of the session directory is refused the
// same way a transcript symlink is.
func kimiSessionCwd(wirePath string) (string, bool) {
	dir := filepath.Dir(wirePath)
	for range kimiSessionDepth {
		if cwd, ok := readKimiState(dir); ok {
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

// kimiSessionDepth bounds the walk from a wire log to the session directory
// holding its state.json. A store holds a workDirKey above the session, so a
// walk that reaches the filesystem root is looking too far.
const kimiSessionDepth = 4

// kimiStateCap bounds the state.json read. The file holds one cwd, so
// anything past this is not a state file toktop can use, and the store is
// writable by the agent: an unbounded ReadFile turns a padded file into
// memory the watcher holds on every poll.
const kimiStateCap = 1 << 20

// readKimiState reads the cwd state.json records in dir.
func readKimiState(dir string) (string, bool) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return "", false
	}
	defer r.Close()
	f, err := r.Open("state.json")
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, kimiStateCap+1))
	if err != nil || len(b) > kimiStateCap {
		return "", false
	}
	var st struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), &st); err != nil || st.Cwd == "" {
		return "", false
	}
	return st.Cwd, true
}

// kimiStoreEvery bounds how often the store's own listing is read. Every
// watcher derives its roots on every rescan window, and each derivation would
// read that listing: thousands of entries, read once per window process-wide
// instead of once per window per watcher. What the delay costs is the sight of
// a project directory that appears while the watcher runs, and nothing else: a
// transcript found late is still read from its start, its usage being this
// attach's either way.
//
// ponytail: one process-wide listing per store per window. Per-project
// indexing (the CLI's own session_index.jsonl) if a profile ever asks for it.
const kimiStoreEvery = 5 * time.Second

// kimiListMax bounds the listing cache. A process reads one store; a test that
// points the adapter at several does not need them all kept.
const kimiListMax = 8

var (
	kimiListMu  sync.Mutex
	kimiListMap = map[string]kimiListing{}
)

// kimiListing is one store's directory names, and when they were read.
type kimiListing struct {
	dirs []string
	at   time.Time
}

// kimiStoreDirs returns the directory names under store, from the shared
// listing when it is fresh. A store that cannot be read is not remembered, so
// the next caller tries again rather than serving an empty store for a window.
func kimiStoreDirs(store string, now time.Time) []string {
	kimiListMu.Lock()
	defer kimiListMu.Unlock()
	if c, ok := kimiListMap[store]; ok && core.Age(now, c.at) < kimiStoreEvery {
		return c.dirs
	}
	entries, err := os.ReadDir(store)
	if err != nil {
		delete(kimiListMap, store)
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(kimiListMap) >= kimiListMax {
		clear(kimiListMap)
	}
	kimiListMap[store] = kimiListing{dirs: dirs, at: now}
	return dirs
}
