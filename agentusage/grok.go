// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Grok writes one updates.jsonl per session. A turn's token counts arrive
// once, on the turn_completed record, with apiDurationMs for how long the
// model spent on it. Turns are minutes apart, so a rate taken from the gap
// between two of them is not the model's speed. The rate is that turn's
// tokens over the model's own time; elapsed_ms, the whole turn with its
// tools, is the fallback for a record that carries no API duration.
//
// The sessions for one working directory live in the directory named for
// that path, percent-encoded with the slashes encoded too:
// ~/.grok/sessions/%2Fhome%2Fme%2Fproj/<id>/updates.jsonl.
//
// usage.inputTokens already includes the cached share when usage.totalTokens
// equals input plus output. The cached fields are added only when the total
// is larger than that sum by the cache.

func grokRoots(dir string, _ time.Time) []string {
	store := home(".grok", "sessions")
	if store == "" {
		return nil
	}
	var out []string
	for _, s := range dirSpellings(dir) {
		name := grokDirName(s)
		if name == "" {
			continue
		}
		out = append(out, filepath.Join(store, name))
	}
	return uniqueRoots(out)
}

// grokDirName is the directory name the CLI uses for one working directory.
// url.PathEscape encodes every separator a path segment would otherwise be
// split on, so the whole working directory is one component. It leaves ':'
// alone, and ':' is encoded separately because a Windows directory name cannot
// contain it: C:\Users\... would otherwise be the component C:%5CUsers%5C...,
// which Windows refuses to create.
func grokDirName(dir string) string {
	if dir == "" {
		return ""
	}
	return strings.ReplaceAll(url.PathEscape(dir), ":", "%3A")
}

func grokSessionCwd(path string) (string, bool) {
	session := filepath.Dir(path)
	encoded := filepath.Base(filepath.Dir(session))
	if encoded == "" || encoded == "." || encoded == string(filepath.Separator) {
		return "", false
	}
	decoded, err := url.PathUnescape(encoded)
	// A NUL byte survives the decode as %00 and names no directory on any
	// platform, so a session under one is not attributed to a working
	// directory at all.
	if err != nil || !filepath.IsAbs(decoded) || strings.ContainsRune(decoded, 0) {
		return "", false
	}
	return decoded, true
}

// maxTurnMS is the largest turn length that converts to a duration. The
// counters arrive as milliseconds and the span is nanoseconds, so a value past
// this wraps the product negative and a rate taken over it reports tokens per
// second with the wrong sign.
const maxTurnMS = math.MaxInt64 / int64(time.Millisecond)

// turnSpan converts one record's millisecond turn length into the span a rate
// is taken over. A count of zero or less is no reading at all: a zero span is
// what the caller falls back to the gap between records on.
//
// The clamp is the same rule for every adapter publishing a turn length (grok's
// apiDurationMs and elapsed_ms, microagent's elapsed_ms), so it lives here
// rather than once per parser: an adapter spelling the conversion its own way
// is a wrapped span and a rate with the wrong sign.
func turnSpan(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	n := min(int64(ms), maxTurnMS)
	return time.Duration(n) * time.Millisecond
}

// parseGrokUpdate reads one updates.jsonl line. Only a completed turn
// carries counts. Anything else in the log, including the same word inside
// a tool result, contributes nothing.
func parseGrokUpdate(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(bytes.TrimSpace(line), utf8BOM)
	var rec struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				ElapsedMS     int    `json:"elapsed_ms"`
				Usage         struct {
					InputTokens         int `json:"inputTokens"`
					OutputTokens        int `json:"outputTokens"`
					CachedReadTokens    int `json:"cachedReadTokens"`
					CacheCreationTokens int `json:"cacheCreationTokens"`
					ReasoningTokens     int `json:"reasoningTokens"`
					TotalTokens         int `json:"totalTokens"`
					APIDurationMS       int `json:"apiDurationMs"`
				} `json:"usage"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return values{}, "", false
	}
	if rec.Method != "_x.ai/session/update" || rec.Params.Update.SessionUpdate != "turn_completed" {
		return values{}, "", false
	}
	u := rec.Params.Update.Usage
	// reasoningTokens here sit outside output, and both cached counters are
	// reads of the prompt, so this is the same fold Gemini's two shapes use.
	v, ok := foldCounters(u.InputTokens, u.OutputTokens, u.ReasoningTokens,
		satAdd(counter(u.CachedReadTokens), counter(u.CacheCreationTokens)), u.TotalTokens)
	if !ok {
		return values{}, "", false
	}
	// apiDurationMs is time spent in the model. elapsed_ms is the whole
	// turn, tools included, and a turn that mostly ran tools would otherwise
	// report a few tokens per second for a model that was much faster.
	ms := u.APIDurationMS
	if ms <= 0 {
		ms = rec.Params.Update.ElapsedMS
	}
	v.span = turnSpan(ms)
	return v, "", true
}
