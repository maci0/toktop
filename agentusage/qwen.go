// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
)

// parseQwen reads one line of a qwen-code chat transcript. Usage is recorded
// per assistant message, and thinking tokens are output tokens too.
func parseQwen(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	var rec struct {
		Type  string `json:"type"`
		Cwd   string `json:"cwd"`
		Usage struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "assistant" {
		return values{}, "", false
	}
	u := rec.Usage
	thoughts := counter(u.ThoughtsTokenCount)
	v := values{
		output:   satAdd(counter(u.CandidatesTokenCount), thoughts),
		thinking: thoughts,
		total:    counter(u.TotalTokenCount),
		input:    counter(u.PromptTokenCount),
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, rec.Cwd, true
}
