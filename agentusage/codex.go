// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
)

// codexSessionCwd reads the working directory from a codex session header.
func codexSessionCwd(line []byte) (string, bool) {
	line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "session_meta" {
		return "", false
	}
	if rec.Payload.Cwd == "" {
		return "", false
	}
	return rec.Payload.Cwd, true
}

// parseCodex reads one line of a codex rollout. Its token_count events carry
// the session total, so the values are absolute.
func parseCodex(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Cwd  string `json:"cwd"`
			Info struct {
				TotalTokenUsage struct {
					InputTokens           int `json:"input_tokens"`
					OutputTokens          int `json:"output_tokens"`
					ReasoningOutputTokens int `json:"reasoning_output_tokens"`
					TotalTokens           int `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return values{}, "", false
	}
	// session_meta names the directory; token_count carries the numbers.
	if rec.Payload.Type == "token_count" {
		u := rec.Payload.Info.TotalTokenUsage
		total := counter(u.TotalTokens)
		out := counter(u.OutputTokens)
		think := counter(u.ReasoningOutputTokens)
		in := counter(u.InputTokens)
		if remain := satSub(total, out); remain > in {
			in = remain
		}
		v := values{
			output:   out,
			thinking: think,
			total:    total,
			input:    in,
		}
		if !v.present() {
			return values{}, "", false
		}
		return v, "", true
	}
	return values{}, "", false
}
