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

// publishedPackages are the importable packages of this module: a Go program
// outside the module can name them, and `internal/` bars it from naming
// anything else. The Makefile's PUBLIC_PKGS names the same set, and a package
// added to one belongs in the other.
var publishedPackages = []string{"agentusage"}

// internalPrefix is the import path prefix Go refuses to resolve from outside
// this module. A type under it is nameable here and unnameable there.
const internalPrefix = modulePath + "/internal/"

// TestPublishedSurfaceNamesNoInternalType fails when an exported declaration
// in a published package spells a type from internal/. A caller outside the
// module cannot write that import path, so a signature carrying one does not
// compile for the consumer the package exists to serve, and `make check-api`
// does not see it: that gate compares what a release stopped exporting, and a
// type that never could be written stays in the surface for as long as it is
// there.
//
// An exported alias is the one spelling that is legal, because the alias is
// the name the caller writes: `type Pacer = core.Pacer` is reachable as
// agentusage.Pacer, and the internal path stays behind it. Every other
// position is a leak, a struct field and an interface method included, as is
// the declared type of a constant or a variable, which go doc prints and a
// caller assigns to.
//
// The files are read whether or not this build's tags include them. A leak is
// a property of a declaration, not of the platform it is reachable on, so the
// half behind the sqlite tag is checked on the build that leaves it out.
func TestPublishedSurfaceNamesNoInternalType(t *testing.T) {
	for _, pkg := range publishedPackages {
		for _, use := range internalTypesInPublicSurface(t, pkg) {
			t.Errorf("%s: exported %s names %s, which a caller outside this module cannot write; export an alias for it, or keep the type out of the signature",
				pkg, use.where, use.spells)
		}
	}
}

// TestPublishedPackagesMatchTheMakefile fails when the list of published
// packages and the Makefile's PUBLIC_PKGS have drifted apart. `make check-api`
// reads the Makefile's list, and the leak check above reads this file's, so a
// package added to one is invisible to the gate written against the other: a
// new public surface would go unchecked, and a package dropped from the
// Makefile would take its compatibility gate with it.
func TestPublishedPackagesMatchTheMakefile(t *testing.T) {
	declared := makefileAssignment(t, "PUBLIC_PKGS")
	if declared == "" {
		t.Fatal("the Makefile assigns no PUBLIC_PKGS; the published surface would be read from no list at all")
	}
	inMakefile := strings.Fields(strings.TrimPrefix(declared, "./"))
	inTest := slices.Sorted(slices.Values(publishedPackages))
	slices.Sort(inMakefile)
	if !slices.Equal(inMakefile, inTest) {
		t.Errorf("Makefile PUBLIC_PKGS is %v and publishedPackages is %v; they name the same importable packages and have to be the same list",
			inMakefile, inTest)
	}
}

// makefileAssignment returns the value a Makefile line assigns to a name, or
// the empty string when no line does. The `:=` parser the tool pins are read
// with is not used here: it reads the recursive `=` assignments as though they
// were immediate, and PUBLIC_PKGS is one of those. Reading the one line keeps
// this off the tool pins, which are a different concern.
func makefileAssignment(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// A name a longer one ends in, and a comment that mentions one, are not
	// the assignment. The left edge of the line and the operator the value
	// follows are what identify it.
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*[:+?]?=`)
	for _, line := range strings.Split(string(raw), "\n") {
		operator := pattern.FindStringIndex(line)
		if operator == nil {
			continue
		}
		return strings.TrimSpace(line[operator[1]:])
	}
	return ""
}

// internalUse is one internal type named at one place in the public surface.
type internalUse struct {
	where  string
	spells string
}

// internalTypesInPublicSurface returns every internal type an exported
// declaration of a package spells outside an exported alias. An empty result
// is the passing case and not an accident: the published surface of every
// package in publishedPackages was walked, and found to hold one.
func internalTypesInPublicSurface(t *testing.T, pkg string) []internalUse {
	t.Helper()
	dir := filepath.Join(moduleRoot, pkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	names := 0
	var uses []internalUse
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, name, err)
		}
		if file.Name.Name != pkg {
			t.Fatalf("%s/%s declares package %s; the published list names a directory holding another package", dir, name, file.Name.Name)
		}
		names++
		imports := internalImports(file)
		for _, decl := range file.Decls {
			uses = append(uses, internalTypesInDecl(decl, imports)...)
		}
	}
	if names == 0 {
		t.Fatalf("%s parsed to no files; the walk above would report nothing and pass", dir)
	}
	return uses
}

// internalImports maps the local name of every import under internalPrefix to
// the import path it stands for. The map is built from the import block
// rather than from the spelling at a use site, so a file that imports core
// under another name is covered too.
func internalImports(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || !strings.HasPrefix(path, internalPrefix) {
			continue
		}
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			// An unaliased import's local name is its last path element.
			name = path[strings.LastIndex(path, "/")+1:]
		}
		imports[name] = path
	}
	return imports
}

// internalTypesInDecl returns the internal types an exported declaration of
// this file spells. A function's body is not walked: what a function does
// with a value is the package's business, and only the types in its signature
// cross the boundary.
func internalTypesInDecl(decl ast.Decl, imports map[string]string) []internalUse {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() || d.Type == nil {
			return nil
		}
		return internalTypesInType(d.Type, imports, "func "+d.Name.Name)
	case *ast.GenDecl:
		var uses []internalUse
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				// An alias is the one declaration whose right-hand side is
				// not part of the surface: the name the alias carries is
				// what a caller writes, so `type Pacer = core.Pacer` is
				// reachable as agentusage.Pacer and the internal path stays
				// behind it. An unexported alias hides the same way, and an
				// exported one that names an internal type in a signature is
				// caught there rather than here.
				if s.Assign.IsValid() {
					continue
				}
				if s.Name.IsExported() {
					uses = append(uses, internalTypesInType(s.Type, imports, "type "+s.Name.Name)...)
				}
			case *ast.ValueSpec:
				// The values are expressions and go unwalked; the declared
				// type is what a caller writes on their own.
				if s.Type == nil {
					continue
				}
				for _, name := range s.Names {
					if name.IsExported() {
						uses = append(uses, internalTypesInType(s.Type, imports, name.Name)...)
					}
				}
			}
		}
		return uses
	}
	return nil
}

// internalTypesInType walks a type expression, the struct fields and
// interface methods nested in it included, and returns the internal packages
// it names. The whole expression is inspected rather than only its outermost
// selector, because the leak that costs a caller a compile is the one in the
// field of a struct it can otherwise construct.
func internalTypesInType(expr ast.Expr, imports map[string]string, where string) []internalUse {
	if expr == nil {
		return nil
	}
	var uses []internalUse
	ast.Inspect(expr, func(n ast.Node) bool {
		// A struct field no caller can name is not part of the surface: its
		// type stays inside the package, which is where the walk stops. An
		// embedded field has no name of its own and is walked, since the
		// embedded type's name is the field's name.
		if field, ok := n.(*ast.Field); ok && len(field.Names) > 0 {
			exported := slices.ContainsFunc(field.Names, func(name *ast.Ident) bool { return name.IsExported() })
			if !exported {
				return false
			}
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, internal := imports[ident.Name]
		if !internal {
			return true
		}
		uses = append(uses, internalUse{where: where, spells: path + "." + sel.Sel.Name})
		return true
	})
	return uses
}
