// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"math"
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
	// Payload keys hold user or tool text. Counters live beside
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
	line = bytes.TrimPrefix(line, utf8BOM)
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

// inlineKids is the per-level scratch for the members a level still has to
// descend into. Records are small and most envelopes hold fewer than this many
// non-scalar members, so the array covers them without touching the heap: this
// runs for every key of every record, four times a second, per transcript.
const inlineKids = 8

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
		// This level is read before any subtree, and among the working
		// directories it names the smallest key wins. Both fix the same
		// defect: Go randomizes map order, so a record naming two working
		// directories used to pick a winner at random and at random depth,
		// and transcript.go caches that winner for the life of the session.
		// A directory named at the top of the record is the record's own;
		// one buried in a wrapper is a fallback. Ownership is judged per
		// record, so one unchanged line must not be attributed to a
		// different directory on each poll.
		//
		// Deciding the winner by comparison rather than by sorting the keys
		// is what keeps the loop allocation-free: the subtree order the sort
		// used to impose is unobservable, since counters merge by maximum and
		// a directory found here is never overwritten from below.
		var (
			stack    [inlineKids]any
			kids     = stack[:0]
			cwdKey   string
			cwdValue string
		)
		for k, child := range v {
			lower := foldKey(k)
			if str, isString := child.(string); isString {
				if cwdKeys[lower] && (cwdKey == "" || k < cwdKey) {
					cwdKey, cwdValue = k, str
				}
				continue // other string fields are not content, not a counter
			}
			if isNumberKey(lower) {
				assign(ev, lower, child)
			}
			switch child.(type) {
			case nil, bool, float64: // no subtree to descend into
			default:
				if !payloadKeys[lower] {
					kids = append(kids, child)
				}
			}
		}
		if ev.Cwd == "" {
			ev.Cwd = cwdValue
		}
		for _, child := range kids {
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

// utf8BOM is the byte-order mark a transcript may open a record with. A
// package-level slice so trimming it costs no allocation per line.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// foldKey lowercases a JSON key for the tables above, which hold ASCII names
// only. Folding with strings.ToLower would also fold runes whose lowercase
// form is ASCII: U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) becomes "i"
// and U+212A (KELVIN SIGN) becomes "k", so a key spelled in either would
// satisfy an entry like "input_tokens" that its producer never wrote. Only
// the ASCII letters move; every byte of a multi-byte rune is left alone.
//
// A key that is already folded, which is nearly every key in every envelope
// these agents write, is returned as it came in: walk asks for this once per
// key of every record, and a copy per key is the allocation this path is
// measured on.
func foldKey(k string) string {
	for i := 0; i < len(k); i++ {
		if k[i] >= 'A' && k[i] <= 'Z' {
			return foldUpperASCII(k)
		}
	}
	return k
}

// foldUpperASCII is foldKey's path for a key that does carry an uppercase
// letter. Short keys fold in a stack buffer; only an unusually long one
// allocates.
func foldUpperASCII(k string) string {
	var stack [64]byte
	var b []byte
	if len(k) <= len(stack) {
		b = stack[:len(k)]
	} else {
		b = make([]byte, len(k))
	}
	copy(b, k)
	for i := range b {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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
