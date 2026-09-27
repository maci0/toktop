// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
// The store is machine-wide, so ownership follows the session's recorded
// working directory. The wire log names no directory: the CLI keeps it in
// state.json beside the session's agents/ directory, which is what
// kimiSessionCwd reads.

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

// kimiStateBytes bounds the state.json read. A session's file holds its title,
// approval flags and subagent metadata; anything past this is not the small
// record the CLI writes, and is not read into memory to be rejected.
const kimiStateBytes = 1 << 20

// kimiSessionCwd reads the working directory a session was started in, from
// the state.json beside the agents/ directory the wire log lives under. An
// unreadable or unwritten state.json yields no verdict, so the transcript is
// retried on a later poll rather than being refused for good.
//
// The sibling path is derived from the transcript's own, and opened through
// os.Root so a state.json swapped for a symlink out of the session directory
// is refused the same way a transcript symlink is.
func kimiSessionCwd(wirePath string) (string, bool) {
	// .../<session>/agents/<agentId>/wire.jsonl, so the session directory is
	// three levels up; two would land on agents/, which holds no state.json.
	session := filepath.Dir(filepath.Dir(filepath.Dir(wirePath)))
	r, err := os.OpenRoot(session)
	if err != nil {
		return "", false
	}
	defer r.Close()
	f, err := r.Open("state.json")
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, kimiStateBytes))
	if err != nil {
		return "", false
	}
	var st struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &st); err != nil {
		return "", false
	}
	if st.Cwd == "" {
		return "", false
	}
	return st.Cwd, true
}
