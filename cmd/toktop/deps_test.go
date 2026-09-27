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
