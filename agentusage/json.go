// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/maci0/toktop/internal/core"
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
	// Cache is the cached share of the prompt, kept apart from Input because
	// it adds to it rather than competing with it. An agent reporting the
	// uncached, cache-read and cache-write shares bills all three, so folding
	// them by maximum (as the other counters are folded) would report the
	// largest share alone and lose the rest.
	Cache int
}

func (u jsonUsage) Has() bool {
	return values{output: u.Output, thinking: u.Thinking, total: u.Total, input: satAdd(u.Input, u.Cache)}.present()
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
		// Gemini's tokens.thoughts, the bare spelling of thoughtsTokenCount
		// beside the counters it writes. A turn billed on thought tokens
		// alone reads as no usage at all without it.
		"thoughts": true,
	}
	totalKeys = map[string]bool{
		"total_tokens": true, "totaltokens": true, "totaltokencount": true,
		// The bare spelling, which is what the Gemini CLI writes under
		// "tokens" and what opencode writes in a message's data column. Left
		// out, a definition pointed at one of those logs read a total it
		// could not recognize and reported no context size at all.
		"total": true,
	}
	inputKeys = map[string]bool{
		"input_tokens": true, "inputtokens": true, "prompt_tokens": true,
		"prompttokens": true, "prompttokencount": true, "input": true,
		// Kimi Code CLI's spelling of the uncached prompt share, so a
		// definition pointed at one of its logs reads what the agent read.
		"inputother": true,
	}
	// The cached shares of a prompt, in the spellings the supported agents
	// use. These add to inputKeys rather than joining it: a log carrying only
	// cache fields carries a billable prompt, and reading it through
	// inputKeys alone reported no prompt at all or, once the keys were added
	// there, the largest share instead of the sum.
	cacheKeys = map[string]bool{
		"cache_read_input_tokens": true, "cache_creation_input_tokens": true,
		"cache_read_tokens": true, "cache_creation_tokens": true,
		// Kimi Code CLI's own spelling of the same two shares.
		"inputcacheread": true, "inputcachecreation": true,
		// pi and the agents built on it (prime-agent, feynman, omp) spell the
		// two billed cache shares cacheRead and cacheWrite. They are the
		// bulk of a turn's prompt: input alone is the uncached remainder.
		"cacheread": true, "cachewrite": true,
		// dsh writes them as cacheReadTokens and cacheWriteTokens, and Grok
		// as cachedReadTokens and cacheCreationTokens. Both report the
		// uncached prompt beside these rather than inside it, so a definition
		// pointed at one of those logs read the uncached remainder alone and
		// under-reported the prompt by the bulk of a turn.
		"cachereadtokens": true, "cachewritetokens": true,
		"cachedreadtokens": true, "cachecreationtokens": true,
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
// TrimSpace slices it, and the decoder neither retains nor modifies its input.
//
// Numbers decode as json.Number rather than float64. Decoding into a float64
// fails the whole record when any number in it is out of range, whatever the
// number is and wherever it sits: a "toolParameters" that escaped its digits
// fails the turn's own counters beside it, and the record is lost whole,
// while the typed per-agent decoders skip the field they do not model and
// read the counters anyway. asInt applies the range, and refuses a value
// outside it, once a number is in a field this package recognizes.
func parseJSON(line []byte) (jsonEvent, bool) {
	line = bytes.TrimPrefix(line, utf8BOM)
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return jsonEvent{}, false
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return jsonEvent{}, false
	}
	// A Decoder stops at the end of the first value and leaves whatever
	// follows it unread, where json.Unmarshal rejected the line outright.
	// Nothing here reads a second value, so a record with trailing content is
	// as malformed as one that fails to parse, and stays rejected.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return jsonEvent{}, false
	}
	var ev jsonEvent
	walk(doc, &ev, 0)
	return ev, true
}

// mayCarryUsage reports whether line holds any key the walk recognizes, without
// decoding it. Keys are folded with core.FoldASCII, which only lowers A-Z, so
// comparing case-insensitively over the raw bytes cannot miss a key that would
// have matched.
//
// The markers below are the shortest substrings that together cover every key
// in outputKeys, thinkingKeys, totalKeys, inputKeys, cacheKeys and cwdKeys. A
// match is a filter, not a lookup: it only means the record has to be decoded,
// so a false positive costs the decode it would have paid anyway, while a key
// holding none of them is never read again. TestUsageMarkersCoverEveryKey is
// what keeps the two lists in step, and fails the build when a key is added to
// a table without one. Grouped by first byte so a record that carries nothing
// costs one compare per byte.
func mayCarryUsage(line []byte) bool {
	for i := range len(line) {
		switch lowerASCII(line[i]) {
		case 't':
			if hasPrefixFold(line[i:], "token") || hasPrefixFold(line[i:], "thinking") || hasPrefixFold(line[i:], "total") || hasPrefixFold(line[i:], "thought") {
				return true
			}
		case 'o':
			if hasPrefixFold(line[i:], "output") {
				return true
			}
		case 'r':
			if hasPrefixFold(line[i:], "reasoning") {
				return true
			}
		case 'i':
			if hasPrefixFold(line[i:], "input") {
				return true
			}
		case 'c':
			if hasPrefixFold(line[i:], "cache") || hasPrefixFold(line[i:], "cwd") {
				return true
			}
		case 'w':
			if hasPrefixFold(line[i:], "work") {
				return true
			}
		case 'p':
			if hasPrefixFold(line[i:], "project") {
				return true
			}
		}
	}
	return false
}

func hasPrefixFold(line []byte, prefix string) bool {
	if len(line) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lowerASCII(line[i]) != prefix[i] {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// maxDepth bounds the walk. Agent envelopes nest a few levels; anything deeper
// is a tool result payload, whose contents are not this package's business.
const maxDepth = 8

// inlineKids is the per-level scratch for the members a level still has to
// descend into. Records are small and most envelopes hold fewer than this many
// non-scalar members, so the array covers them without touching the heap: this
// runs for every key of every record, on every poll, per transcript.
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
		// is what keeps the loop allocation-free: a directory found here is
		// never overwritten from below, so only the choice within this one map
		// needs an order.
		var (
			stack  [inlineKids]any
			kids   = stack[:0]
			kstack [inlineKids]string
			kkids  = kstack[:0]
			cwdKey string
			// cwdValue is the value the winning key carries. An empty one
			// names no directory at all, so it never wins: a record spelling
			// `{"cwd":"", "project_dir":"/w"}` reports the working directory
			// under the shorter key and loses the only path it has.
			cwdValue string
		)
		for k, child := range v {
			lower := core.FoldASCII(k)
			if str, isString := child.(string); isString {
				if str != "" && cwdKeys[lower] && (cwdKey == "" || k < cwdKey) {
					cwdKey, cwdValue = k, str
				}
				continue // other string fields are not content, not a counter
			}
			if isNumberKey(lower) {
				assign(ev, lower, child)
			}
			switch child.(type) {
			case nil, bool, json.Number: // no subtree to descend into
			default:
				if !payloadKeys[lower] {
					kids = append(kids, child)
					kkids = append(kkids, k)
				}
			}
		}
		// Subtrees are descended in key order, so ev.Cwd below is settled by
		// the first one that names a directory: the subtree under the smaller
		// key wins. The compared key is the subtree's own, not the working
		// directory it carries (which is not known until it is walked), so the
		// winner is fixed before any descent and cannot alternate the way Go's
		// random map order would. The sort is bounded by inlineKids and
		// allocates nothing.
		for i := 1; i < len(kids); i++ {
			for j := i; j > 0 && kkids[j-1] > kkids[j]; j-- {
				kkids[j-1], kkids[j] = kkids[j], kkids[j-1]
				kids[j-1], kids[j] = kids[j], kids[j-1]
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
	return outputKeys[lower] || thinkingKeys[lower] || totalKeys[lower] || inputKeys[lower] || cacheKeys[lower]
}

// utf8BOM is the byte-order mark a transcript may open a record with. A
// package-level slice so trimming it costs no allocation per line.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// assign records a counter. Every field but Cache keeps the largest value
// seen on this line, because an agent repeats a total in a nested summary;
// Cache is summed instead, for the reason its field carries.
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
	case cacheKeys[lower]:
		ev.Usage.Cache = satAdd(ev.Usage.Cache, n)
	}
}

func asInt(v any) (int, bool) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	// A number too large for a float64 arrives here as +Inf with an error, and
	// fails the range test below either way.
	n, _ := num.Float64()
	// The conversion below is only defined within the range of int, and
	// out-of-range results differ by platform (amd64 gives the minimum,
	// arm64 saturates to the maximum). A counter outside int, or past
	// maxSaneTokens, is not a measurement: report nothing rather than
	// a platform-dependent lie or a total that later wraps.
	if !(n >= 1) || n > float64(maxSaneTokens) || n > float64(math.MaxInt) {
		return 0, false
	}
	// A fractional remainder (1.5, 99.9) would become 1 or 99 through a
	// truncating conversion; ingest already refuses those, and a transcript
	// line is the same kind of counter.
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
	// A record carrying no key the walk knows is nothing this function can
	// report, whatever it holds, and most records in a generic store are a user
	// message, a tool result or a session notice. Decoding one into an any tree
	// allocates a map and a boxed value per key for all of that payload before
	// the walk drops it; mayCarryUsage rules those records out off the raw
	// bytes. A false positive costs the decode it would have paid anyway.
	if !mayCarryUsage(line) {
		return values{}, "", false
	}
	ev, ok := parseJSON(line)
	if !ok || !ev.Usage.Has() {
		return values{}, "", false
	}
	out := counter(ev.Usage.Output)
	// Cached prompt tokens were billed too, so they add to the uncached share
	// the same fold parseClaude and parseDsh apply.
	in := satAdd(counter(ev.Usage.Input), counter(ev.Usage.Cache))
	tot := counter(ev.Usage.Total)
	// No total on the record: the turn billed what it read and wrote, the
	// same fallback grok and gemini apply. Unconditional, or a record that
	// reports only output is stored with a total of zero against a real
	// output count.
	if tot == 0 {
		tot = satAdd(in, out)
	}
	// A record carrying a total below the output it reports is as unsupported a
	// reading as one that omits it, so both are floored the way foldCounters,
	// parseQwen, parseCodex, parseDsh and the opencode sqlite reader floor
	// theirs.
	v := values{
		output:   out,
		thinking: counter(ev.Usage.Thinking),
		total:    floorTotal(tot, out),
		input:    in,
	}
	if !v.present() {
		return values{}, "", false
	}
	return v, ev.Cwd, true
}

// genericSessionCwd finds the working directory in a session header, whatever
// the record is called: the first line that names one wins. The header scan
// reads the first records of a session, most of which are the session's own
// content, so the same prefilter as parseGeneric applies.
func genericSessionCwd(line []byte) (string, bool) {
	if !mayCarryUsage(line) {
		return "", false
	}
	ev, ok := parseJSON(line)
	if !ok || ev.Cwd == "" {
		return "", false
	}
	return ev.Cwd, true
}
