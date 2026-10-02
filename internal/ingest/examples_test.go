// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ingest

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The documented ingest examples are the copy-paste source for the harnesses
// that push the feed, and a harness that retries a POST is the normal case: a
// lost answer looks exactly like a request that never arrived. The endpoint
// is duplicate-safe only when the sender keys its events, so an example that
// posts without an id teaches the one shape whose replay double-counts.
//
// A key built from the shell is the subtler version of the same mistake:
// `$(date +%s)` is evaluated per attempt, so a retry presents a key the feed
// has never seen and every event lands a second time. The header must carry
// a value the sender already knows: the turn it is reporting.

var codeElement = regexp.MustCompile(`(?s)<code>(.*?)</code>`)

// unkeyedExample reports why a block's POST cannot be deduped on replay, or ""
// when it can. A body naming neither an event id nor a request key is the shape
// whose second execution is counted a second time; a key the shell expands is
// the subtler one, where every attempt presents a value the feed has never seen.
//
// The check is a pure function of the block text so the guard below stays a
// one-line call and the shapes that defeat it can be pinned on their own,
// without a document in the tree having to carry a bad example for a test to
// prove the guard would catch it.
func unkeyedExample(ex string) string {
	if !strings.Contains(ex, "Idempotency-Key:") && !strings.Contains(ex, `"id":`) {
		return "it names no event id and no Idempotency-Key"
	}
	for line := range strings.SplitSeq(ex, "\n") {
		_, key, found := strings.Cut(line, "Idempotency-Key:")
		if !found {
			continue
		}
		// Anything the shell expands is a different value on the next
		// attempt, which is a new operation to the feed.
		if idx := strings.IndexAny(key, "$`"); idx >= 0 {
			return "it builds an Idempotency-Key from the shell: " + strings.TrimSpace(key[idx:])
		}
	}
	return ""
}

// ingestExamples returns every code block in a document that posts to the
// ingest endpoint, and a block that names the path is the one a reader copies.
//
// fenced says whether blocks is a ``` split, in which case only the odd
// halves are code. That parity is a property of the split, not of the caller:
// applying it to the site's <code> elements dropped every even-indexed one,
// and the feed example site/worker.js carries sits at index 0, so both guards
// below stopped inspecting it entirely. A later edit that dropped the
// Idempotency-Key from the published example would then have passed, and the
// one document a visitor copies their harness from would double-count its
// events on every retry while the tests written to catch exactly that stayed
// green.
func ingestExamples(t *testing.T, name string, blocks []string, fenced bool) []string {
	t.Helper()
	var out []string
	for i, chunk := range blocks {
		if fenced && i%2 == 0 {
			continue // only the fenced halves of a ``` split are code
		}
		if strings.Contains(chunk, eventsPath) {
			out = append(out, chunk)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s documents no curl example posting to %s", name, eventsPath)
	}
	return out
}

func readmeBlocks(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "```")
}

func siteBlocks(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../site/worker.js")
	if err != nil {
		t.Fatal(err)
	}
	return codeElement.FindAllString(string(b), -1)
}

func TestDocumentedExamplesKeyTheirEvents(t *testing.T) {
	for _, doc := range []struct {
		name   string
		blocks []string
		fenced bool
	}{
		{"README.md", readmeBlocks(t), true},
		{"site/worker.js", siteBlocks(t), false},
	} {
		for _, ex := range ingestExamples(t, doc.name, doc.blocks, doc.fenced) {
			if why := unkeyedExample(ex); why != "" {
				t.Errorf("%s posts an event a retry would count again, because %s:\n%s", doc.name, why, ex)
			}
		}
	}
}

// TestDocumentedExampleRejectsReplayUnsafeShapes pins the two shapes the guard
// above has to refuse, so it cannot go quiet without a document having to carry
// a bad example to show it. The unkeyed body at index 0 is the site's own
// layout: it is the block that parity-skipping the even-indexed halves once
// dropped on the floor, which is what let this guard pass a site whose example
// had stopped naming a key at all.
func TestDocumentedExampleRejectsReplayUnsafeShapes(t *testing.T) {
	const keyed = "curl -X POST localhost:8420/v1/events \\\n" +
		`  -H "Idempotency-Key: coder-turn-1042" -d '` +
		`{"agent":"coder","output_tokens":310}'`
	const clockKeyed = "curl -X POST localhost:8420/v1/events \\\n" +
		`  -H "Idempotency-Key: turn-$(date +%s)" -d '` +
		`{"agent":"coder","output_tokens":310}'`
	const unkeyed = "curl -X POST localhost:8420/v1/events \\\n" +
		`  -d '{"agent":"coder","output_tokens":310}'`
	const byID = "curl -X POST localhost:8420/v1/events \\\n" +
		`  -d '{"id":"turn-1042","agent":"coder","output_tokens":310}'`

	for _, tc := range []struct {
		name string
		ex   string
		bad  bool
	}{
		{"a request key names one turn", keyed, false},
		{"an event id names one turn", byID, false},
		{"a clock key is fresh on every attempt", clockKeyed, true},
		{"no key at all replays as a second event", unkeyed, true},
	} {
		if got := unkeyedExample(tc.ex) != ""; got != tc.bad {
			t.Errorf("%s: rejected=%v, want %v (why=%q)", tc.name, got, tc.bad, unkeyedExample(tc.ex))
		}
	}

	// The site layout: the block sits at index 0, and the extraction is the
	// one siteBlocks feeds. A fenced split must still skip its even halves,
	// which is what the parity is for; raw <code> elements must not.
	site := codeElement.FindAllString("<code>"+unkeyed+"</code><code>docs</code>", -1)
	if got := len(ingestExamples(t, "site", site, false)); got != 1 {
		t.Errorf("site extraction kept %d blocks, want 1: the parity skip does not apply to <code> elements", got)
	}
	// A fenced split still skips its even halves, which is what the parity is
	// for; here the code is the second half, at index 1 of a real split.
	fenced := strings.Split("```sh\nls\n```\n```sh\n"+unkeyed+"\n```", "```")
	if got := len(ingestExamples(t, "README.md", fenced, true)); got != 1 {
		t.Errorf("fenced extraction kept %d blocks, want 1: the odd halves are the code", got)
	}
}
