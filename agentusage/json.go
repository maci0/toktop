// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

// Transcript JSON is walked by key rather than modeled per agent: envelopes
// change between releases, so a renamed wrapper still works and a renamed
// usage field degrades to no numbers instead of wrong ones.

type jsonEvent struct {
	// Usage is any token counters found on the line. Absent counters stay at
	// zero, and Has reports whether anything was found at all.
	Usage jsonUsage
	// Cwd is the working directory the record was produced in, when the agent
	// records one. Transcripts use it to attribute a session to a process.
	Cwd string
}

// jsonUsage holds token counters found on one line. Agents report a mix of
// per-message and cumulative values; the adapter's valueKind decides how
// they combine.
type jsonUsage struct {
	Output   int
	Thinking int
	Total    int
	Input    int
}

func (u jsonUsage) Has() bool {
	return values{output: u.Output, thinking: u.Thinking, total: u.Total, input: u.Input}.present()
}

// Keys recognized as token counters, mapped onto the fields above. These are
// the names used by the Anthropic, OpenAI, and Gemini shaped APIs, which every
// supported agent's output follows in one dialect or another.
var (
	outputKeys = map[string]bool{
		"output_tokens": true, "outputtokens": true, "completion_tokens": true,
		"completiontokens": true, "candidatestokencount": true, "output": true,
		"outputtokencount": true,
	}
	thinkingKeys = map[string]bool{
		"thinking_tokens": true, "thinkingtokens": true, "reasoning_tokens": true,
		"reasoning_output_tokens": true, "thoughtstokencount": true,
		"reasoningtokens": true, "reasoning": true, "thinking": true,
	}
	totalKeys = map[string]bool{
		"total_tokens": true, "totaltokens": true, "totaltokencount": true,
	}
	inputKeys = map[string]bool{
		"input_tokens": true, "inputtokens": true, "prompt_tokens": true,
		"prompttokens": true, "prompttokencount": true, "input": true,
	}
	// Fields naming the working directory a record belongs to.
	cwdKeys = map[string]bool{
		"cwd": true, "working_directory": true, "workingdirectory": true,
		"workdir": true, "project_dir": true, "projectdir": true,
	}
	// Payload keys hold user, model, or tool text. Counters live beside
	// these fields, not inside them; descending would inspect prompts
	// and could pick a cwd or a number out of user content.
	payloadKeys = map[string]bool{
		"content": true, "parts": true, "delta": true,
		"tool_result": true, "toolresult": true,
		"tool_use": true, "tooluse": true,
		"tool_calls": true, "toolcalls": true,
		"arguments": true,
		"prompt":    true, "messages": true, "choices": true,
		"system": true, "text": true, "query": true,
		"input_text": true, "inputtext": true,
		"output_text": true, "outputtext": true,
		"system_instruction": true, "systeminstruction": true,
	}
)

// parseJSON reads one line of an agent's transcript. ok is false when the line
// is not a JSON object at all, which is how a caller knows the line carries
// nothing this package reads.
//
// This runs once per transcript record, so nothing here copies the line:
// TrimSpace slices it, and json.Unmarshal neither retains nor modifies its
// input.
func parseJSON(line []byte) (jsonEvent, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return jsonEvent{}, false
	}
	var doc any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return jsonEvent{}, false
	}
	var ev jsonEvent
	walk(doc, &ev, 0)
	return ev, true
}

// maxDepth bounds the walk. Agent envelopes nest a few levels; anything deeper
// is a tool result payload, whose contents are not this package's business.
const maxDepth = 8

// walk descends one decoded line collecting usage counters and the recorded
// working directory. The rule that makes this envelope-agnostic: recognized
// keys contribute their values wherever they sit in the tree, so "message"
// wrapping "usage" in one agent's dialect and a flat "usageMetadata" in
// another's both work.
func walk(node any, ev *jsonEvent, depth int) {
	if depth > maxDepth {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			lower := strings.ToLower(k)
			if str, isString := child.(string); isString {
				if cwdKeys[lower] && ev.Cwd == "" {
					ev.Cwd = str
				}
				continue // other string fields are not content, not a counter
			}
			if isNumberKey(lower) {
				assign(ev, lower, child)
				continue
			}
			if payloadKeys[lower] {
				continue
			}
			walk(child, ev, depth+1)
		}
	case []any:
		for _, child := range v {
			walk(child, ev, depth+1)
		}
	}
}

func isNumberKey(lower string) bool {
	return outputKeys[lower] || thinkingKeys[lower] || totalKeys[lower] || inputKeys[lower]
}

// assign records a counter, keeping the largest value seen for that field on
// this line: agents sometimes repeat a total in a nested summary.
func assign(ev *jsonEvent, lower string, val any) {
	n, ok := asInt(val)
	if !ok || n <= 0 {
		return
	}
	switch {
	case thinkingKeys[lower]:
		ev.Usage.Thinking = max(ev.Usage.Thinking, n)
	case outputKeys[lower]:
		ev.Usage.Output = max(ev.Usage.Output, n)
	case totalKeys[lower]:
		ev.Usage.Total = max(ev.Usage.Total, n)
	case inputKeys[lower]:
		ev.Usage.Input = max(ev.Usage.Input, n)
	}
}

func asInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok {
		return 0, false
	}
	// The conversion below is only defined within the range of int, and
	// out-of-range results differ by platform (amd64 gives the minimum,
	// arm64 saturates to the maximum). A counter outside int, or past
	// maxSaneTokens, is not a measurement: report nothing rather than
	// a platform-dependent lie or a total that later wraps.
	if !(n >= 1) || n > float64(maxSaneTokens) || n > float64(math.MaxInt) {
		return 0, false
	}
	// JSON numbers are float64. A fractional remainder (1.5, 99.9) would
	// become 1 or 99 through a truncating conversion; ingest already
	// refuses those, and a transcript line is the same kind of counter.
	i := int(n)
	if n != float64(i) {
		return 0, false
	}
	return i, true
}

// parseGeneric reads usage out of an unknown JSONL record by key, the same way
// the stream parser does. It is what makes a defined agent's transcript
// readable without a bespoke adapter.
func parseGeneric(line []byte) (values, string, bool) {
	ev, ok := parseJSON(line)
	if !ok || !ev.Usage.Has() {
		return values{}, "", false
	}
	out := counter(ev.Usage.Output)
	in := counter(ev.Usage.Input)
	tot := counter(ev.Usage.Total)
	if tot == 0 && in > 0 {
		tot = satAdd(in, out)
	}
	v := values{
		output:   out,
		thinking: counter(ev.Usage.Thinking),
		total:    tot,
		input:    in,
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, ev.Cwd, true
}

// genericSessionCwd finds the working directory in a session header, whatever
// the record is called: the first line that names one wins.
func genericSessionCwd(line []byte) (string, bool) {
	ev, ok := parseJSON(line)
	if !ok || ev.Cwd == "" {
		return "", false
	}
	return ev.Cwd, true
}
