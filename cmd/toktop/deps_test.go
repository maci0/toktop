package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// moduleRoot is the directory holding go.mod, relative to this package.
const moduleRoot = "../.."

// modulePath is that module's import path, the prefix every intra-module
// import carries.
const modulePath = "github.com/maci0/toktop"

// dependencyTable is where every direct Go dependency records its reason. A
// module that appears in go.mod and in neither that file nor an import fails
// TestDirectDependenciesAreImported.
const dependencyTable = "docs/DEPENDENCIES.md"

// directRequires returns the modules go.mod requires without the indirect
// marker, which is the set this tree chose rather than inherited.
func directRequires(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var direct []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case strings.HasPrefix(line, "require ") && !strings.Contains(line, "// indirect"):
			direct = append(direct, strings.Fields(strings.TrimPrefix(line, "require "))[0])
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			if !strings.Contains(line, "// indirect") {
				direct = append(direct, strings.Fields(line)[0])
			}
		}
	}
	if len(direct) == 0 {
		t.Fatal("go.mod parsed to no direct requires; the parser no longer understands the file")
	}
	return direct
}

// importedModules returns every import path in the module, test files
// included: a benchmark-only dependency is still a dependency.
func importedModules(t *testing.T) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != moduleRoot && (strings.HasPrefix(name, ".") || name == "dist" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			paths = append(paths, strings.Trim(spec.Path.Value, `"`))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	return paths
}

func TestDirectDependenciesAreImported(t *testing.T) {
	imports := importedModules(t)
	reasoned, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	for _, module := range directRequires(t) {
		used := slices.ContainsFunc(imports, func(path string) bool {
			return path == module || strings.HasPrefix(path, module+"/")
		})
		if !used {
			t.Errorf("%s is required by go.mod and imported nowhere; remove it or import it", module)
		}
		if !strings.Contains(string(reasoned), module) {
			t.Errorf("%s is required by go.mod and has no entry in %s; record why it is here", module, dependencyTable)
		}
	}
}

// documentedModule is one row of the table in docs/DEPENDENCIES.md, or the
// empty string for a line that names no module.
func documentedModule(line string) string {
	cells := strings.Split(line, "|")
	if len(cells) < 2 {
		return ""
	}
	cell := strings.Trim(strings.TrimSpace(cells[1]), "`")
	first, _, _ := strings.Cut(cell, "/")
	if !strings.Contains(first, ".") || strings.ContainsAny(cell, " \t") {
		return ""
	}
	return cell
}

func TestDependencyTableMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	direct := directRequires(t)
	for _, line := range strings.Split(string(raw), "\n") {
		module := documentedModule(line)
		if module == "" {
			continue
		}
		if !slices.Contains(direct, module) {
			t.Errorf("%s is documented in %s but is not a direct require in go.mod", module, dependencyTable)
		}
	}
}

// The module's tiers, lowest first. A package may import a package in a
// strictly lower tier and nothing else: a lower layer reaching back up
// inverts the dependency direction and is what makes a tree hard to change,
// since the two packages then have to move together.
//
//	0  core              the types and helpers everything is written in terms of
//	1  logcfg, bearer, procs, selfreload, agentusage
//	2  gpu, probe, provider, demo, selfupdate, ui
//	3  sysmon, ingest, agentwatch
//	4  remote, collector
//	5  cmd/toktop        the only package allowed to wire the rest together
var tiers = [][]string{
	{"internal/core"},
	{"internal/logcfg", "internal/bearer", "internal/procs", "internal/selfreload", "agentusage"},
	{"internal/gpu", "internal/probe", "internal/provider", "internal/demo", "internal/selfupdate", "internal/ui"},
	{"internal/sysmon", "internal/ingest", "internal/agentwatch"},
	{"internal/remote", "internal/collector"},
	{"cmd/toktop"},
}

// tierOf maps every package in the module to its tier, and fails the test if
// a package is in none: a new package must be placed deliberately rather than
// inheriting a direction by omission.
func tierOf(t *testing.T) map[string]int {
	t.Helper()
	tier := make(map[string]int)
	for i, names := range tiers {
		for _, name := range names {
			tier[modulePath+"/"+name] = i
		}
	}
	present, err := packageDirs(moduleRoot)
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	for dir := range present {
		if _, ok := tier[dir]; !ok {
			t.Errorf("package %q is in no tier; place it in tiers in deps_test.go", dir)
		}
	}
	return tier
}

// packageDirs returns every directory in the module holding Go files, named
// by its import path ("internal/remote", "cmd/toktop").
func packageDirs(root string) (map[string]bool, error) {
	dirs := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "dist" || name == "node_modules") {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				dirs[modulePath+"/"+strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))] = true
				return nil
			}
		}
		return nil
	})
	return dirs, err
}

// TestImportsPointDownward fails when a package imports one at or above its
// own tier. Go forbids an import cycle, so this is what keeps a cycle from
// being broken by moving code sideways into a package that already depends
// on the caller.
func TestImportsPointDownward(t *testing.T) {
	tier := tierOf(t)
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != moduleRoot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		dir := modulePath + "/" + strings.TrimPrefix(strings.TrimPrefix(filepath.Dir(path), moduleRoot), string(filepath.Separator))
		from, ok := tier[dir]
		if !ok {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			if !strings.HasPrefix(imported, modulePath+"/") || imported == dir {
				// Not one of ours, or an external test package reaching
				// back into the package it sits beside.
				continue
			}
			to, ok := tier[imported]
			if !ok {
				t.Errorf("%s imports %q, which is in no tier; place it in tiers in deps_test.go", path, imported)
				continue
			}
			if to >= from {
				t.Errorf("%s (tier %d) imports %s (tier %d); a package may only import strictly lower tiers", path, from, imported, to)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}
