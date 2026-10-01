// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// microagentRecord is the record microagent appends for one model response,
// taken from a live run: this response's own counters, the directory the run
// works in, and how long the model spent on it.
func microagentRecord(cwd string, out, think, in, total, elapsedMS int) string {
	return `{"ts":1790608347342,"cwd":` + jsonPath(cwd) + `,"model":"deepseek/deepseek-v4-flash",` +
		`"elapsed_ms":` + strconv.Itoa(elapsedMS) +
		`,"usage":{"prompt_tokens":` + strconv.Itoa(in) +
		`,"completion_tokens":` + strconv.Itoa(out) +
		`,"reasoning_tokens":` + strconv.Itoa(think) +
		`,"total_tokens":` + strconv.Itoa(total) + `}}`
}

func TestParseMicroagentReadsResponseCountersCwdAndModelTime(t *testing.T) {
	v, cwd, ok := parseMicroagent([]byte(microagentRecord("/home/dev/proj", 19, 16, 998, 1017, 1448)))
	if !ok {
		t.Fatal("a live microagent record was rejected")
	}
	if v.output != 19 || v.thinking != 16 || v.input != 998 || v.total != 1017 {
		t.Fatalf("got %+v, want output 19 thinking 16 input 998 total 1017", v)
	}
	if cwd != "/home/dev/proj" {
		t.Fatalf("cwd = %q, want /home/dev/proj", cwd)
	}
	if v.span != 1448*time.Millisecond {
		t.Fatalf("span = %v, want 1448ms", v.span)
	}
}

// A record whose elapsed_ms is absent, negative, or not a number still counts:
// the counters are the reading, and only the span is missing. A count near
// MaxInt64 must not wrap the span negative and report the rate backwards.
func TestParseMicroagentSpanIsOptionalAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want time.Duration
	}{
		{"absent", `{"cwd":"/w","usage":{"completion_tokens":3,"total_tokens":4}}`, 0},
		{"zero", `{"cwd":"/w","elapsed_ms":0,"usage":{"completion_tokens":3,"total_tokens":4}}`, 0},
		{"negative", `{"cwd":"/w","elapsed_ms":-5,"usage":{"completion_tokens":3,"total_tokens":4}}`, 0},
		{"not a number", `{"cwd":"/w","elapsed_ms":"soon","usage":{"completion_tokens":3,"total_tokens":4}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, ok := parseMicroagent([]byte(tc.line))
			if !ok {
				t.Fatalf("%s was rejected, but its counters are a reading", tc.line)
			}
			if v.output != 3 || v.span != tc.want {
				t.Fatalf("got output %d span %v, want output 3 span %v", v.output, v.span, tc.want)
			}
		})
	}

	v, _, ok := parseMicroagent([]byte(
		`{"cwd":"/w","elapsed_ms":9223372036854775807,"usage":{"completion_tokens":3,"total_tokens":4}}`))
	if !ok {
		t.Fatal("a record with an absurd elapsed_ms still carries counters")
	}
	if v.span != time.Duration(maxTurnMs)*time.Millisecond {
		t.Fatalf("span = %v, want the bound %v", v.span, time.Duration(maxTurnMs)*time.Millisecond)
	}
}

// Two responses add up, and the rate comes from the model's own time rather
// than from the gap between the two readings: the second response's 5 tokens
// over its recorded 500ms is 10 tokens per second, whatever the wall clock
// between the two polls says.
func TestMicroagentSumsResponsesAndRatesOverModelTime(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(withStore(t, "microagent"), "1790608345894873547.jsonl")
	w := Watch("microagent", work, time.Now())
	if w == nil {
		t.Fatal("microagent should be supported")
	}

	append_(t, path, microagentRecord(work, 19, 16, 998, 1017, 1448))
	first := w.Poll()
	if first.Output != 19 || first.Thinking != 16 || first.Input != 998 || first.Total != 1017 {
		t.Fatalf("first response: got %+v", first)
	}
	if first.Span != 1448*time.Millisecond {
		t.Fatalf("first response span = %v, want 1448ms", first.Span)
	}

	append_(t, path, microagentRecord(work, 5, 0, 1200, 1205, 500))
	second := w.Poll()
	if second.Output != 24 || second.Thinking != 16 || second.Input != 2198 || second.Total != 1205 {
		t.Fatalf("after the second response: got %+v", second)
	}
	if second.Span != 1948*time.Millisecond {
		t.Fatalf("span = %v, want 1948ms", second.Span)
	}
	if rate, ok := second.RateFrom(first); !ok || rate != 10 {
		t.Fatalf("rate = %v (ok=%v), want 10 tokens per second over the model's own time", rate, ok)
	}
}

// The store is machine-wide, so another run's session file belongs to another
// project and must not be billed to this one. Each run writes its own file.
func TestMicroagentSkipsAnotherWorkingDirectory(t *testing.T) {
	work, other := t.TempDir(), t.TempDir()
	store := withStore(t, "microagent")
	w := Watch("microagent", work, time.Now())
	if w == nil {
		t.Fatal("microagent should be supported")
	}

	append_(t, filepath.Join(store, "theirs.jsonl"), microagentRecord(other, 100, 0, 500, 600, 1000))
	if s := w.Poll(); !s.Empty() {
		t.Fatalf("another directory's response was counted: %+v", s)
	}

	append_(t, filepath.Join(store, "mine.jsonl"), microagentRecord(work, 7, 0, 60, 67, 700))
	s := w.Poll()
	if s.Output != 7 || s.Input != 60 {
		t.Fatalf("this directory's response was not counted: %+v", s)
	}
}

// A store file whose records name no directory names no run, and the store is
// machine-wide: attributing it to the watcher that happened to read it would
// bill one project's tokens to every project on the machine.
func TestMicroagentSkipsARecordWithNoDirectory(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(withStore(t, "microagent"), "1790608345894873547.jsonl")
	w := Watch("microagent", work, time.Now())
	if w == nil {
		t.Fatal("microagent should be supported")
	}

	append_(t, path, `{"usage":{"completion_tokens":50,"total_tokens":60}}`)
	if s := w.Poll(); !s.Empty() {
		t.Fatalf("a record with no cwd was counted: %+v", s)
	}
}
