// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The --ingest default is written by hand in four files that a reader trusts
// and that only one of them is: cmd/toktop/flags.go registers it, and the
// README, docs/openapi.yaml and site/worker.js each name the address in prose
// a user copies a curl line out of. Nothing held the four together, so
// changing the default moved the flag and left every document describing a
// port the binary no longer binds. The three symptoms are one failure: an
// agent that posts to 127.0.0.1:8420 and gets connection refused, a
// generated OpenAPI client that dials a port nothing listens on, and a
// landing page whose working example does not work.
//
// The default is read out of the flag registration with go/parser rather than
// a regexp, so the gate follows a rename, a reformat or a move of the
// registration, and fails loudly on the parse instead of reporting an empty
// default as a real one.
func TestIngestDefaultMatchesEveryDocumentThatNamesIt(t *testing.T) {
	addr := ingestFlagDefault(t)
	port := addr[strings.LastIndex(addr, ":")+1:]

	for _, rel := range []string{
		"README.md",
		filepath.Join("docs", "openapi.yaml"),
		filepath.Join("site", "worker.js"),
	} {
		raw, err := os.ReadFile(filepath.Join(moduleRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		doc := string(raw)
		// The port is the part every copy spells. The host is spelled three
		// ways across the three documents (127.0.0.1, localhost, and behind
		// a URL scheme) while the port is spelled the same way in all of
		// them, so the port is what the four are held to.
		if !strings.Contains(doc, port) {
			t.Errorf("%s never names the --ingest default port %q; the flag binds %q", rel, port, addr)
		}
		// A document that kept the old port is the failure itself, and the
		// check above cannot see it once the new one is also present, so a
		// stale ingest address is named rather than left for a reader to
		// discover as a refused connection.
		for _, stale := range staleIngestPorts(doc, port) {
			t.Errorf("%s still names ingest port %q, which --ingest no longer binds (it binds %q)", rel, stale, addr)
		}
	}
}

// ingestFlagDefault is the address cmd/toktop/flags.go registers --ingest
// with: the third argument of the topFS.StringVar call that binds
// cli.ingest. Parsed rather than matched, so a registration this reader does
// not understand fails here instead of yielding "".
func ingestFlagDefault(t *testing.T) string {
	t.Helper()
	path := filepath.Join(moduleRoot, "cmd", "toktop", "flags.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/toktop/flags.go: %v", err)
	}
	found := ""
	ast.Inspect(file, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "StringVar" || len(call.Args) != 4 {
			return true
		}
		x, ok := ast.Unparen(sel.X).(*ast.Ident)
		if !ok || x.Name != "topFS" {
			return true
		}
		if !bindsIngest(call.Args[0]) || !stringLit(call.Args[1], "ingest") {
			return true
		}
		lit, ok := ast.Unparen(call.Args[2]).(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("--ingest default is not a string literal at %s", fset.Position(call.Args[2].Pos()))
		}
		v, unqErr := strconv.Unquote(lit.Value)
		if unqErr != nil {
			t.Fatalf("--ingest default %s does not unquote: %v", lit.Value, unqErr)
		}
		if v == "" {
			t.Fatal("--ingest registers an empty default")
		}
		found = v
		return false
	})
	if found == "" {
		t.Fatal("no topFS.StringVar registering --ingest in cmd/toktop/flags.go")
	}
	return found
}

// bindsIngest reports whether a StringVar target is the &cli.ingest field.
func bindsIngest(arg ast.Expr) bool {
	un, ok := ast.Unparen(arg).(*ast.UnaryExpr)
	if !ok {
		return false
	}
	sel, ok := ast.Unparen(un.X).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := ast.Unparen(sel.X).(*ast.Ident)
	return ok && ident.Name == "cli" && sel.Sel.Name == "ingest"
}

// stringLit reports whether expr is the string literal want.
func stringLit(expr ast.Expr, want string) bool {
	lit, ok := ast.Unparen(expr).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == want
}

// ingestPortPat matches the :PORT of an ingest address as the documents spell
// it. The host is the first of the two the default carries, and the path or
// scheme beside it is what makes the match about the ingest endpoint rather
// than about any other loopback service: a `curl -X POST localhost:PORT`, an
// openapi `servers` URL, and the two loopback spellings with no path at all
// (the README's default sentence and the site's parenthetical). A discovery
// port such as 127.0.0.1:11434 carries none of those, so a document listing
// the engine ports the dashboard probes is not held to the --ingest default.
var ingestPortPat = regexp.MustCompile(
	`POST\s+(?:https?://)?(?:127\.0\.0\.1|localhost|\[::1\]):(\d+)` +
		`|- url:\s*https?://(?:127\.0\.0\.1|localhost|\[::1\]):(\d+)` +
		`|default(?:ed)?\s+on\s+(?:` + "`" + `)?(?:127\.0\.0\.1|localhost|\[::1\]):(\d+)` +
		`|\(\s*` + "<code>" + `(?:127\.0\.0\.1|localhost|\[::1\]):(\d+)`)

// staleIngestPorts is every port doc names on an ingest-shaped address that
// is not want. Each is a copy an agent, a generated client or a reader takes
// verbatim and finds nothing listening on. The pattern carries one capture
// group per spelling of the address, so the first non-empty group of a match
// is the port it names.
func staleIngestPorts(doc, want string) []string {
	var stale []string
	for _, m := range ingestPortPat.FindAllStringSubmatch(doc, -1) {
		for _, port := range m[1:] {
			if port == "" {
				continue
			}
			if port != want {
				stale = append(stale, port)
			}
			break
		}
	}
	slices.Sort(stale)
	return slices.Compact(stale)
}
