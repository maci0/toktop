// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
)

// microagent writes one session log per run under
// ~/.microagent/sessions/<unix-ns>.jsonl, one JSON object per model response:
//
//	{"ts":1790608347342,"cwd":"/work","model":"deepseek/deepseek-v4-flash",
//	 "elapsed_ms":1448,"usage":{"prompt_tokens":998,"completion_tokens":19,
//	 "reasoning_tokens":16,"total_tokens":1017}}
//
// The counters are that response's own, not the run's cumulative ones, so the
// adapter folds them per message; the directory is on every record, and the
// store is machine-wide, so it is the attribution.
//
// elapsed_ms is how long the model spent on the response, which the rate is
// taken over. The counts are written when the response ends, and the gap to
// the previous one covers the tool calls in between, so dividing by that gap
// would report a generation that was never continuous.
func parseMicroagent(line []byte) (values, string, bool) {
	v, cwd, ok := parseGeneric(line)
	if !ok {
		return values{}, "", false
	}
	v.span = turnSpan(microagentElapsedMS(line))
	return v, cwd, true
}

// microagentElapsedMS reads elapsed_ms, the model time of one response. The
// decode is behind the key so a record without one — an older build, a line
// that merely mentions the word — pays a byte scan rather than a second JSON
// decode. A count that does not fit an int, or a negative one, is no reading.
func microagentElapsedMS(line []byte) int {
	if !bytes.Contains(line, microagentElapsedKey) {
		return 0
	}
	var rec struct {
		ElapsedMS int `json:"elapsed_ms"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &rec); err != nil {
		return 0
	}
	return rec.ElapsedMS
}

// microagentElapsedKey is the field name, as bytes, so the scan above does not
// build a string per record.
var microagentElapsedKey = []byte("elapsed_ms")
