// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recoveryTable is where the tree says what state it keeps, where each piece
// lives, and what losing it costs. It is prose, and prose does not fail a
// build: state added after it was written sits in the tree with no backup, no
// restore procedure and no row in the RPO table, and the only thing that
// notices is the operator who lost it. A file the program creates, renames or
// removes is state, so every package whose shipped code calls one of fsCalls is
// named there or the test below fails.
const recoveryTable = "docs/RECOVERY.md"

// fsCalls are the os calls that leave something on disk: a file created,
// written, renamed, given a mode, or removed. A package that calls one owns
// state whether or not that call is a backup of anything, and the inventory is
// checked against this list rather than against what the table happens to say
// today.
var fsCalls = []string{
	"Chmod", "Create", "CreateTemp", "Link", "MkdirAll", "OpenFile",
	"Rename", "Remove", "RemoveAll", "Symlink", "Truncate", "WriteFile",
}

// filesystemPackages returns the directory of every package in the module
// whose shipped code calls one of fsCalls on the os package. Test files are
// left out: they write the fixtures they assert against, which is the test's
// own state and not a state a host can lose.
func filesystemPackages(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "site", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		if !touchesFilesystem(file) {
			return nil
		}
		dir, err := filepath.Rel(moduleRoot, filepath.Dir(path))
		if err != nil {
			t.Fatalf("relativize %s: %v", path, err)
		}
		found = append(found, filepath.ToSlash(dir))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// touchesFilesystem reports whether a parsed file calls one of fsCalls on os.
func touchesFilesystem(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		if slices.Contains(fsCalls, sel.Sel.Name) {
			found = true
			return false
		}
		return true
	})
	return found
}

func TestEveryPackageThatWritesToDiskIsInTheRecoveryTable(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(recoveryTable)))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range filesystemPackages(t) {
		if !strings.Contains(string(doc), pkg) {
			t.Errorf("%s names no package %q, which creates, renames or removes files; "+
				"add it to the state inventory with where its state lives and what losing it costs",
				recoveryTable, pkg)
		}
	}
}

// The audit log is the record that says a recovery happened or failed: the
// lines naming an engine going down, a store backup that could not be written,
// a store read back from its copy, and a rename that could not be made durable
// are the only trace any of those left. It is state a run keeps after it
// exits, and toktop writes it wherever stderr points rather than to a file of
// its own, so the state inventory has to say so: a row that names it, an RPO
// and an RTO that price losing it, and the failure domain that says its
// durability is the operator's redirect.
//
// The second half is pinned against the code rather than against the prose. The
// claim is that toktop never opens a log file, so the log's durability is the
// operator's redirect and not a setting here. A logger that grew an os.Open of
// its own would give the log a path of its own to back up, and the inventory
// row would then be wrong; this fails rather than letting that drift reach an
// operator in the middle of a restore.
func TestTheAuditLogIsInventoryAndItIsStderrOnly(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(recoveryTable)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		what, row string
	}{
		{"the state inventory", "| the audit log |"},
		{"the RPO table", "| RPO for the audit log |"},
		{"the RTO table", "| RTO for the audit log |"},
	} {
		if !strings.Contains(string(doc), want.row) {
			t.Errorf("%s carries no row for %s: it is the record that says a recovery happened or failed, "+
				"so what losing it costs belongs beside what losing the pin store costs",
				recoveryTable, want.what)
		}
	}

	// os.Stderr, not a file: the whole claim is that the log's durability is
	// the operator's redirect.
	fset := token.NewFileSet()
	path := filepath.Join(moduleRoot, "internal", "logcfg", "logcfg.go")
	file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if perr != nil {
		t.Fatalf("parse %s: %v", path, perr)
	}
	writesAFile := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		switch sel.Sel.Name {
		case "Create", "CreateTemp", "Open", "OpenFile", "WriteFile":
			writesAFile = true
		}
		return true
	})
	if writesAFile {
		t.Errorf("internal/logcfg opens a file of its own, so the audit log has a path of its own; "+
			"%s still names it as stderr-only, which is now wrong. Add where the log lives and what losing it costs",
			recoveryTable)
	}
}
