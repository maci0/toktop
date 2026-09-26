// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
)

// parseClaude reads one line of a Claude Code transcript. Assistant messages
// carry per-message usage, so the values are added up. A negative counter is
// not a measurement: it is clamped to absent, the same rule the generic
// walker applies, so a corrupted or hostile line cannot subtract from a total.
func parseClaude(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	var rec struct {
		Type    string `json:"type"`
		Cwd     string `json:"cwd"`
		Message struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				Details                  struct {
					ThinkingTokens int `json:"thinking_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "assistant" {
		return values{}, "", false
	}
	u := rec.Message.Usage
	out := counter(u.OutputTokens)
	in := counter(u.InputTokens)
	think := counter(u.Details.ThinkingTokens)
	prompt := satAdd(in, satAdd(counter(u.CacheReadInputTokens), counter(u.CacheCreationInputTokens)))
	v := values{
		output:   out,
		thinking: think,
		total:    satAdd(prompt, out),
		input:    prompt,
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, rec.Cwd, true
}
