// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ingest

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The README's event table is the schema a sender programs against: it names
// every field, its type, its default, and what an out-of-bound value does.
// The decoder is the other half of the same contract, and nothing connects
// them, so a field added to the struct reaches a sender as an accepted input
// no document describes, and a field dropped from the struct keeps its README
// row and silently stops working. Both are wire breaks, and a sender that
// found either from a 202 alone would have no way to tell.

const eventFieldsHeading = "Event fields are all optional"

var backticked = regexp.MustCompile("`([^`]+)`")

// wireFields are the JSON names the decoder accepts, in declaration order.
func wireFields(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeFor[agentEventWire]()
	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" {
			t.Errorf("agentEventWire.%s has no json tag, so no README row can name what the decoder reads", typ.Field(i).Name)
			continue
		}
		names = append(names, tag)
	}
	return names
}

// documentedFields are the names the README's event table names. Only the
// first cell of a row carries a field; the rest holds its type, its default
// and prose that names other things (`400`, `anonymous`, `turn`). The three
// token counts share one cell, so a cell contributes every name in it.
func documentedFields(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	start := slices.Index(lines, eventFieldsHeading+"; anything omitted gets the default:")
	if start < 0 {
		t.Fatalf("README.md has no %q line, so nothing documents the ingest schema", eventFieldsHeading)
	}
	var out []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cell, _, ok := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		if !ok || strings.TrimSpace(cell) == "field" || strings.HasPrefix(cell, "-") {
			continue
		}
		for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatalf("README.md's %q line is followed by no table", eventFieldsHeading)
	}
	return out
}

func TestWireFieldsAreDocumented(t *testing.T) {
	documented := documentedFields(t)
	for _, name := range wireFields(t) {
		if !slices.Contains(documented, name) {
			t.Errorf("POST /v1/events accepts %q, which no row of README.md's event table names; a sender reading the table cannot send it", name)
		}
	}
}

func TestDocumentedFieldsAreAccepted(t *testing.T) {
	wire := wireFields(t)
	for _, name := range documentedFields(t) {
		if !slices.Contains(wire, name) {
			t.Errorf("README.md's event table names %q, which agentEventWire does not decode; a sender's value is dropped and the row lies about it", name)
		}
	}
}
