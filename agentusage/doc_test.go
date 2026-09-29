// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestPackageDocNamesEveryAdapter holds the package comment's list of
// transcript agents to the adapters table. A caller reads that sentence to
// learn which agents this package ships, and a name added to the table and
// left out of the sentence is a document that understates the surface: the
// adapter works, so the sentence is what is wrong.
//
// The list is read from the sources rather than from the map because tests
// register and remove adapters of their own, and a doc pinned to whatever
// the map holds when the test happened to run states nothing.
func TestPackageDocNamesEveryAdapter(t *testing.T) {
	listed := docTranscriptAgents(t)
	if len(listed) == 0 {
		t.Fatal("doc.go no longer names the transcript agents; this test cannot tell a stale list from a deleted sentence")
	}
	shipped := builtInAdapters(t)
	for name := range shipped {
		if !listed[name] {
			t.Errorf("agentusage ships adapter %q; the package comment in doc.go does not name it among the transcript agents", name)
		}
	}
	for name := range listed {
		if !shipped[name] {
			t.Errorf("the package comment names %q among the transcript agents; registry.go declares no such adapter", name)
		}
	}
}

// docTranscriptAgents reads the parenthesized name list out of the sentence in
// the package comment that introduces the transcript agents.
func docTranscriptAgents(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "doc.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse doc.go: %v", err)
	}
	doc := file.Doc.Text()
	const marker = "Transcript agents ("
	start := strings.Index(doc, marker)
	if start < 0 {
		t.Fatal("doc.go no longer carries the 'Transcript agents (...)' sentence")
	}
	rest := doc[start+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatal("doc.go's transcript agent list is not closed by a ')'")
	}
	listed := make(map[string]bool)
	for _, name := range strings.Split(rest[:end], ",") {
		listed[strings.TrimSpace(name)] = true
	}
	return listed
}

// builtInAdapters reads the keys the adapters table declares at the source
// level, which is the set this package ships.
func builtInAdapters(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "registry.go", nil, 0)
	if err != nil {
		t.Fatalf("parse registry.go: %v", err)
	}
	var table *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			val, ok := spec.(*ast.ValueSpec)
			if !ok || len(val.Names) != 1 || val.Names[0].Name != "adapters" || len(val.Values) != 1 {
				continue
			}
			lit, ok := val.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("registry.go's adapters is a %T, not a composite literal the keys can be read from", val.Values[0])
			}
			table = lit
		}
	}
	if table == nil {
		t.Fatal("registry.go declares no `adapters` composite literal to read the shipped names from")
	}
	found := make(map[string]bool)
	for _, elt := range table.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		name, err := strconv.Unquote(key.Value)
		if err != nil {
			continue
		}
		found[name] = true
	}
	return found
}
