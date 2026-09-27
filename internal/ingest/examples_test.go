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

// ingestExamples returns every code block in a document that posts to the
// ingest endpoint. README fences its examples, the site carries them in
// <code> elements, and a block that names the path is the one a reader
// copies.
func ingestExamples(t *testing.T, name string, blocks []string) []string {
	t.Helper()
	var out []string
	for i, chunk := range blocks {
		if i%2 == 0 {
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
	}{
		{"README.md", readmeBlocks(t)},
		{"site/worker.js", siteBlocks(t)},
	} {
		for _, ex := range ingestExamples(t, doc.name, doc.blocks) {
			if !strings.Contains(ex, "Idempotency-Key:") && !strings.Contains(ex, `"id":`) {
				t.Errorf("%s posts an event with no id and no Idempotency-Key; a retried copy of that request is counted again:\n%s", doc.name, ex)
			}
		}
	}
}

func TestDocumentedExampleKeyIsNotRebuiltPerAttempt(t *testing.T) {
	for _, doc := range []struct {
		name   string
		blocks []string
	}{
		{"README.md", readmeBlocks(t)},
		{"site/worker.js", siteBlocks(t)},
	} {
		for _, ex := range ingestExamples(t, doc.name, doc.blocks) {
			for line := range strings.SplitSeq(ex, "\n") {
				_, key, found := strings.Cut(line, "Idempotency-Key:")
				if !found {
					continue
				}
				// Anything the shell expands is a different value on the next
				// attempt, which is a new operation to the feed.
				if idx := strings.IndexAny(key, "$`"); idx >= 0 {
					t.Errorf("%s builds an Idempotency-Key from the shell (%q); every retry is a fresh key and every event is counted again:\n%s",
						doc.name, key[idx:], ex)
				}
			}
		}
	}
}
