// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The --json report and the revision that names its shape. internal/ui names
// both: the struct the report is built from, and the constant published as the
// report's `schema` field. A report consumer pins to the revision rather than
// to the binary's version, so a key renamed or a field dropped under an
// unchanged revision is a contract the report stops keeping.
const (
	jsonReportFile   = "internal/ui/json.go"
	jsonReportRoot   = "jsonReport"
	jsonReportSchema = "jsonReportSchema"
)

// TestJSONReportSchemaCoversRemovedFields holds the report's `schema` field to
// the changes the report made, across the last release.
//
// The doc comment on jsonReportSchema states the rule: the revision moves when
// a published field is removed, renamed, or changes meaning or unit, and
// adding a field is not a bump. Nothing else in the tree can see a violation
// of it. make check-api diffs the exported Go surface of a package, and the
// report's keys are struct tags rather than declarations, so a rename there is
// invisible to it. make check-changelog-covers asks whether a watched file
// moved, and this file is watched, so a rename needs a changelog entry: the
// entry is what the consumer reads, and nothing holds it to the report. Both
// read the changelog's shape rather than the diff it describes.
//
// A missing key is the shape of the failure worth catching. encoding/json does
// not error on a key the struct does not carry: a report that stopped
// publishing mem_total_mib decodes into a consumer's struct with that field at
// zero, silently, and the consumer reads an idle machine's memory as none at
// all. That is the whole reason the revision is published.
func TestJSONReportSchemaCoversRemovedFields(t *testing.T) {
	head, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(jsonReportFile)))
	if err != nil {
		t.Fatalf("read %s: %v", jsonReportFile, err)
	}
	headSchema, ok := jsonReportSchemaRevision(t, head)
	if !ok {
		t.Fatalf("%s declares no %s constant, so the report publishes no revision to move", jsonReportFile, jsonReportSchema)
	}

	baseTag := lastReleaseTag(t)
	if baseTag == "" {
		t.Skip("no released tag before HEAD^ to compare the report against")
	}
	base, err := gitFileAt(t, baseTag, jsonReportFile)
	if err != nil {
		t.Skipf("%s does not carry %s: %v", baseTag, jsonReportFile, err)
	}
	baseSchema, ok := jsonReportSchemaRevision(t, base)
	if !ok {
		t.Skipf("%s declares no %s constant to compare against", baseTag, jsonReportSchema)
	}

	headKeys := publishedReportKeys(t, head)
	baseKeys := publishedReportKeys(t, base)

	var removed []string
	for key := range baseKeys {
		if !headKeys[key] {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)

	if headSchema < baseSchema {
		t.Errorf("%s is %d, below the %d %s published; a revision only moves forward",
			jsonReportSchema, headSchema, baseSchema, baseTag)
	}
	if len(removed) == 0 {
		return
	}
	if headSchema > baseSchema {
		return
	}
	t.Errorf("these keys of the --json report are gone and %s is still %d, the revision %s published:",
		jsonReportSchema, headSchema, baseTag)
	for _, key := range removed {
		t.Errorf("  %s", key)
	}
	t.Errorf("  a consumer reading one of them decodes the report with no error and reads a zero")
	t.Errorf("  bump %s in %s, or restore the key", jsonReportSchema, jsonReportFile)
}

// lastReleaseTag names the newest tag reachable from the commit before HEAD,
// the base make check-api and make check-changelog-covers measure a cut
// against. An empty result means there is no release to compare against, which
// the caller reports as a skip rather than a pass.
func lastReleaseTag(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", moduleRoot, "describe", "--tags", "--abbrev=0", "HEAD^").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitFileAt reads one file as the named tag's tree holds it.
func gitFileAt(t *testing.T, tag, path string) ([]byte, error) {
	t.Helper()
	return exec.Command("git", "-C", moduleRoot, "show", tag+":"+path).Output()
}

// jsonReportSchemaRevision reads the integer the report's `schema` field
// publishes. The constant is spelled in the file rather than imported: this
// package sits above every tier and imports none of them, and the value in the
// base tree is the one the comparison is about, which no build of this tree
// holds either.
func jsonReportSchemaRevision(t *testing.T, src []byte) (int, bool) {
	t.Helper()
	astFile, err := parser.ParseFile(token.NewFileSet(), jsonReportFile, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", jsonReportFile, err)
	}
	for _, decl := range astFile.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != jsonReportSchema || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s is not an integer literal", jsonReportSchema)
				}
				n, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatalf("%s is %q, which is not a revision", jsonReportSchema, lit.Value)
				}
				return n, true
			}
		}
	}
	return 0, false
}

// publishedReportKeys returns every JSON key the report publishes, keyed by the
// path a consumer reads it at: `engines[].proc_rss_mib`,
// `system.mem_total_mib`. The walk starts at the report's own struct and
// follows the field types out of it, so a key in a struct the report does not
// carry is not part of the report and a struct the report does carry is
// covered however deep it is.
func publishedReportKeys(t *testing.T, src []byte) map[string]bool {
	t.Helper()
	astFile, err := parser.ParseFile(token.NewFileSet(), jsonReportFile, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", jsonReportFile, err)
	}

	structs := map[string]*ast.StructType{}
	for _, decl := range astFile.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				structs[ts.Name.Name] = st
			}
		}
	}

	keys := map[string]bool{}
	seen := map[string]bool{}
	var walk func(name, prefix string)
	walk = func(name, prefix string) {
		st, ok := structs[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		for _, field := range st.Fields.List {
			if len(field.Names) == 0 || !field.Names[0].IsExported() {
				continue
			}
			key, ok := jsonKey(field)
			if !ok {
				continue
			}
			path := prefix + key
			keys[path] = true
			if elem, list, ok := structElem(field.Type); ok {
				if list {
					path += "[]"
				}
				walk(elem, path+".")
			}
		}
	}
	walk(jsonReportRoot, "")
	if len(keys) == 0 {
		t.Fatalf("%s has no %s struct with JSON keys, so the report publishes nothing to compare", jsonReportFile, jsonReportRoot)
	}
	return keys
}

// jsonKey is the name a field is published under, and whether it is published
// at all: encoding/json drops a `json:"-"` field and drops an unexported one
// whatever its tag says. The tag arrives as the quoted literal, so it is
// unquoted before the tag syntax is read: handed the quotes, reflect.StructTag
// finds no `json:` at the head of the value and every field falls back to its
// Go name.
func jsonKey(field *ast.Field) (string, bool) {
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return "", false
	}
	value, ok := reflect.StructTag(tag).Lookup("json")
	if !ok || value == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(value, ",")
	if name == "" {
		return field.Names[0].Name, true
	}
	return name, true
}

// structElem names the struct a field carries, whether the field is a list of
// them, unwrapping the pointers, slices and arrays a report field is written
// through. A named type other than a struct the report's own types reach ends
// the walk. A pointer reports no list: `system` is read at
// `system.mem_total_mib`, not at `system[].`, and the path a consumer writes
// is what the message has to name.
func structElem(expr ast.Expr) (string, bool, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name, false, true
	case *ast.StarExpr:
		name, _, ok := structElem(t.X)
		return name, false, ok
	case *ast.ArrayType:
		name, _, ok := structElem(t.Elt)
		return name, t.Len == nil, ok
	}
	return "", false, false
}
