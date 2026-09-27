// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// Grok writes usage.json once per session and rewrites it with that session's
// totals. The sessions for one working directory live in the directory named
// for that path, percent-encoded with the slashes encoded too:
// ~/.grok/sessions/%2Fhome%2Fme%2Fproj/<id>/usage.json.
//
// session.inputTokens already includes the cached share when session.totalTokens
// equals input plus output. The cached fields are added only when the total is
// larger than that sum by the cache, which is the uncached-input split.

func grokRoots(dir string) []string {
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
	return out
}

// grokDirName is the directory name the CLI uses for one working directory.
// PathEscape leaves '/' and ':' alone. '/' is encoded so the working
// directory is one path component. ':' is encoded because a Windows
// directory name cannot contain it: C:\Users\... would otherwise be the
// component C:%5CUsers%5C..., which Windows refuses to create.
func grokDirName(dir string) string {
	if dir == "" {
		return ""
	}
	name := strings.ReplaceAll(url.PathEscape(dir), "/", "%2F")
	return strings.ReplaceAll(name, ":", "%3A")
}

func grokSessionCwd(path string) (string, bool) {
	session := filepath.Dir(path)
	encoded := filepath.Base(filepath.Dir(session))
	if encoded == "" || encoded == "." || encoded == string(filepath.Separator) {
		return "", false
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil || !filepath.IsAbs(decoded) {
		return "", false
	}
	return decoded, true
}

func parseGrokUsage(data []byte) (values, string, bool) {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, utf8BOM))
	var doc struct {
		Session struct {
			InputTokens         int `json:"inputTokens"`
			OutputTokens        int `json:"outputTokens"`
			CachedReadTokens    int `json:"cachedReadTokens"`
			CacheCreationTokens int `json:"cacheCreationTokens"`
			ReasoningTokens     int `json:"reasoningTokens"`
			TotalTokens         int `json:"totalTokens"`
		} `json:"session"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return values{}, "", false
	}
	s := doc.Session
	in := counter(s.InputTokens)
	out := counter(s.OutputTokens)
	think := counter(s.ReasoningTokens)
	cache := satAdd(counter(s.CachedReadTokens), counter(s.CacheCreationTokens))
	tot := counter(s.TotalTokens)
	parts := satAdd(in, out)
	if cache > 0 && tot >= satAdd(parts, cache) && tot != parts {
		in = satAdd(in, cache)
	}
	v := values{output: out, thinking: think, input: in, total: tot}
	if v.total == 0 {
		v.total = satAdd(in, out)
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, "", true
}
