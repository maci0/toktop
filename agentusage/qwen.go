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
	line = bytes.TrimPrefix(line, utf8BOM)
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
	out := satAdd(counter(u.CandidatesTokenCount), thoughts)
	in := counter(u.PromptTokenCount)
	// The context total, floored at the output as foldCounters floors it: a
	// line carrying a total below what it wrote is a reading nothing supports,
	// and the gemini adapter over this same shape reports the output instead.
	v := values{
		output:   out,
		thinking: thoughts,
		total:    floorTotal(counter(u.TotalTokenCount), out),
		input:    in,
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, rec.Cwd, true
}
