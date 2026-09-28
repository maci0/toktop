// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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

// goModRequires returns every module go.mod requires, and the subset of those
// that carry no indirect marker. The marked-out set is what this tree chose
// rather than inherited: a tool directive marks its module indirect, and the
// dependency tests below read the unmarked set for that reason.
func goModRequires(t *testing.T) (all, direct []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		fields := []string(nil)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			fields = strings.Fields(strings.TrimPrefix(line, "require "))
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			fields = strings.Fields(line)
		}
		if len(fields) == 0 {
			continue
		}
		all = append(all, fields[0])
		if !strings.Contains(line, "// indirect") {
			direct = append(direct, fields[0])
		}
	}
	if len(direct) == 0 {
		t.Fatal("go.mod parsed to no direct requires; the parser no longer understands the file")
	}
	return all, direct
}

// directRequires returns the modules go.mod requires without the indirect
// marker, which is the set this tree chose rather than inherited.
func directRequires(t *testing.T) []string {
	t.Helper()
	_, direct := goModRequires(t)
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
//	1  logcfg            the audit-log vocabulary, below every package that logs
//	2  bearer, procs, selfreload, agentusage
//	3  gpu, probe, provider, demo, selfupdate, ui
//	4  sysmon, ingest, agentwatch
//	5  remote, collector  the ssh client and the fan-in, over every engine-side package
//	6  cmd/toktop         the only package allowed to wire the rest together
//	7  repogate          the tests over the repository's own metadata, above the
//	                      packages whose metadata they read
//
// logcfg sits below its consumers rather than beside them: procs, gpu and
// ingest all reach for the redaction helpers, so a tier that held logcfg
// alongside them would be a layer importing sideways into itself.
var tiers = [][]string{
	{"internal/core"},
	{"internal/logcfg"},
	{"internal/bearer", "internal/procs", "internal/selfreload", "agentusage"},
	{"internal/gpu", "internal/probe", "internal/provider", "internal/demo", "internal/selfupdate", "internal/ui"},
	{"internal/sysmon", "internal/ingest", "internal/agentwatch"},
	{"internal/remote", "internal/collector"},
	{"cmd/toktop"},
	{"internal/repogate"},
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

// TestArchitectureMapCoversEveryPackage fails when a package in the tree has
// no entry in the architecture map, so the map answers "where does this go"
// for a package that actually exists. tiers is the enforcement; this is the
// document the two drift apart in, and a package named in neither is a package
// nobody placed.
func TestArchitectureMapCoversEveryPackage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "docs", "ARCHITECTURE.md"))
	if err != nil {
		t.Fatalf("read docs/ARCHITECTURE.md: %v", err)
	}
	present, err := packageDirs(moduleRoot)
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	for dir := range present {
		rel := strings.TrimPrefix(dir, modulePath+"/")
		if !strings.Contains(string(raw), "`"+rel+"`") {
			t.Errorf("package %q is in no entry in docs/ARCHITECTURE.md; add it to the map", rel)
		}
	}
}

// importPathOf names a directory the walk handed back by its import path. The
// walk spells paths with the platform separator and echoes the root it was
// given verbatim, so on Windows the root arrives as "../.." and the path under
// it as "..\..\internal\core": a byte prefix test misses, and the leftover
// "..\..\internal\core" matches no tier and reads as a package nobody placed.
// Both sides are folded to slashes before the prefix comes off.
func importPathOf(root, path string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root))
	return modulePath + "/" + strings.TrimPrefix(rel, "/")
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
				dirs[importPathOf(root, path)] = true
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
		dir := importPathOf(moduleRoot, filepath.Dir(path))
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

// toolModules returns the module each `tool` directive in go.mod names. The
// directive spells a package inside the module (honnef.co/go/tools/cmd/
// staticcheck), so the module is the longest require in go.mod that is a
// prefix of the directive.
func toolModules(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	requires, _ := goModRequires(t)
	var modules []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		fields := []string(nil)
		switch {
		case line == "tool (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "tool "):
			fields = strings.Fields(strings.TrimPrefix(line, "tool "))
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			fields = strings.Fields(line)
		}
		if len(fields) == 0 {
			continue
		}
		pkg := fields[0]
		module := ""
		for _, require := range requires {
			if (pkg == require || strings.HasPrefix(pkg, require+"/")) && len(require) > len(module) {
				module = require
			}
		}
		if module == "" {
			t.Fatalf("tool directive %q names no module go.mod requires; a tool outside the module graph cannot be verified", pkg)
		}
		modules = append(modules, module)
	}
	return modules
}

func TestToolDirectivesAreDocumented(t *testing.T) {
	reasoned, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	tools := toolModules(t)
	if len(tools) == 0 {
		t.Fatal("go.mod parsed to no tool directives; the parser no longer understands the file")
	}
	for _, module := range tools {
		if !strings.Contains(string(reasoned), module) {
			t.Errorf("%s is run by a tool directive and has no entry in %s; record why it is here", module, dependencyTable)
		}
	}
}

// pythonPins returns the distribution name of every exact pin in the
// requirements files under scripts/. Both files, because a package that moves
// from runtime to tooling is still a pin the table has to account for.
func pythonPins(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, file := range []string{"requirements.txt", "requirements-dev.txt"} {
		raw, err := os.ReadFile(filepath.Join(moduleRoot, "scripts", file))
		if err != nil {
			t.Fatalf("read scripts/%s: %v", file, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			// Comments carry the reason a pin is there; `-r` pulls in the
			// other file, whose names this walks directly. A `--hash` line
			// continues the pin above it, so the name is already counted.
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-r ") || strings.HasPrefix(line, "--") {
				continue
			}
			name, _, _ := strings.Cut(line, "==")
			name = strings.TrimSpace(name)
			if name == "" || strings.ContainsAny(name, " \t") {
				t.Fatalf("scripts/%s: %q is not an exact name==version pin", file, line)
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("the requirements files parsed to no pins; the parser no longer understands them")
	}
	return names
}

func TestPythonPinsAreDocumented(t *testing.T) {
	reasoned, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	for _, name := range pythonPins(t) {
		if !strings.Contains(string(reasoned), name) {
			t.Errorf("%s is pinned by a requirements file and has no entry in %s; record why it is here", name, dependencyTable)
		}
	}
}

// pythonImport matches a top-level import in a file under scripts/, at any
// indentation so the deferred imports inside a function count, and
// captures the module both `import x` and `from x import y` name.
var pythonImport = regexp.MustCompile(`(?m)^[ \t]*(?:from[ \t]+([A-Za-z_][A-Za-z0-9_]*)|import[ \t]+([A-Za-z_][A-Za-z0-9_]*))`)

// pythonModuleNames returns every top-level module a file under scripts/
// imports, the standard library included: a pin that is neither here nor in
// pythonClosure is a package this tree installs and never reaches for.
func pythonModuleNames(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join(moduleRoot, "scripts")
	names := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".py") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range pythonImport.FindAllStringSubmatch(string(raw), -1) {
			name := match[1]
			if name == "" {
				name = match[2]
			}
			names[name] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("scripts/ parsed to no imports; the matcher no longer understands the files")
	}
	return names
}

// pythonImportNames is the top-level module a distribution in
// scripts/requirements.txt is imported under, for the pins whose distribution
// name is not the name a file writes. An empty value is the identity, so a pin
// whose import name matches needs no entry.
var pythonImportNames = map[string]string{
	"pillow": "PIL",
}

// pythonImportName is the top-level module a distribution is imported under.
func pythonImportName(distribution string) string {
	if module, ok := pythonImportNames[distribution]; ok {
		return module
	}
	return distribution
}

// pythonClosure is the runtime pin a distribution in scripts/requirements.txt
// is required by, for the pins no file in scripts/ imports itself. The install
// runs --no-deps, so a pin nothing imports has to be spelled out here or it is
// a package the tree fetches from PyPI and never runs.
var pythonClosure = map[string]string{
	"wcwidth": "pyte",
}

// TestPythonRuntimePinsAreUsed fails when scripts/requirements.txt pins a
// distribution nothing under scripts/ imports and nothing in pythonClosure
// accounts for. The Go side has this gate for the direct require block
// (TestDirectDependenciesAreImported); the Python side had only the
// documentation check, so a pin outlived the import that needed it and kept
// being installed from the index.
func TestPythonRuntimePinsAreUsed(t *testing.T) {
	runtime, err := os.ReadFile(filepath.Join(moduleRoot, "scripts", "requirements.txt"))
	if err != nil {
		t.Fatalf("read scripts/requirements.txt: %v", err)
	}
	var pins []string
	for _, line := range strings.Split(string(runtime), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-r ") || strings.HasPrefix(line, "--") {
			continue
		}
		name, _, _ := strings.Cut(line, "==")
		if name = strings.TrimSpace(name); name != "" {
			pins = append(pins, name)
		}
	}
	if len(pins) == 0 {
		t.Fatal("scripts/requirements.txt parsed to no pins; the parser no longer understands it")
	}
	pinned := make(map[string]bool, len(pins))
	for _, name := range pins {
		pinned[name] = true
	}
	imported := pythonModuleNames(t)
	for _, name := range pins {
		if imported[pythonImportName(name)] {
			continue
		}
		if _, inClosure := pythonClosure[name]; inClosure {
			continue
		}
		t.Errorf("%s is pinned by scripts/requirements.txt and imported nowhere under scripts/; remove the pin or record it in pythonClosure as a requirement of the pin that needs it", name)
	}
	for name, module := range pythonImportNames {
		if !pinned[name] {
			t.Errorf("pythonImportNames names %s, which scripts/requirements.txt no longer pins", name)
		}
		if !imported[module] {
			t.Errorf("pythonImportNames maps %s to %s, which nothing under scripts/ imports", name, module)
		}
		if strings.EqualFold(name, module) {
			t.Errorf("pythonImportNames maps %s to %s, which is the same name; the pin needs no entry", name, module)
		}
	}
	for name, requiredBy := range pythonClosure {
		if !pinned[name] {
			t.Errorf("pythonClosure names %s, which scripts/requirements.txt no longer pins", name)
		}
		if !pinned[requiredBy] {
			t.Errorf("pythonClosure says %s is required by %s, which scripts/requirements.txt no longer pins", name, requiredBy)
		}
		if imported[name] {
			t.Errorf("pythonClosure says %s is required by %s, but scripts/ imports %s directly", name, requiredBy, name)
		}
	}
}

// makeVars returns the `NAME := value` assignments in the Makefile. The
// version pins live in those assignments, so a recipe names its tool through
// one and a recipe that grows a bare package name is the drift to catch.
func makeVars(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, ok := strings.Cut(line, " := ")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t$") {
			continue
		}
		vars[name] = strings.TrimSpace(value)
	}
	if len(vars) == 0 {
		t.Fatal("the Makefile parsed to no assignments; the parser no longer understands the file")
	}
	return vars
}

// expandVars substitutes $(NAME) with the assignment makeVars read, once, and
// leaves anything it cannot resolve alone so the caller can name it.
func expandVars(s string, vars map[string]string) string {
	var out strings.Builder
	for {
		before, after, found := strings.Cut(s, "$(")
		out.WriteString(before)
		if !found {
			return out.String()
		}
		name, rest, closed := strings.Cut(after, ")")
		if !closed {
			out.WriteString("$(" + after)
			return out.String()
		}
		if value, ok := vars[name]; ok {
			out.WriteString(value)
		} else {
			out.WriteString("$(" + name + ")")
		}
		s = rest
	}
}

// fetchedTool is one tool a recipe pulls from a registry or a proxy while the
// recipe runs: the file and line it came from, and the coordinate the recipe
// names.
type fetchedTool struct {
	source string
	line   string
	tool   string
}

// allFetchedTools returns every tool the Makefile and the CI workflows fetch.
// A job that runs a package by coordinate is a dependency of this tree whether
// the recipe is a Makefile target or a step, so both are scanned by the same
// two gates below.
func allFetchedTools(t *testing.T) []fetchedTool {
	t.Helper()
	return append(fetchedTools(t), workflowFetchedTools(t)...)
}

// fetchedTools returns every tool in the Makefile a recipe fetches, in the
// order the recipes spell them. `go run` resolves through the module proxy,
// `bunx` through the npm registry and `uvx` through PyPI, so each is a package
// this tree takes a dependency on at build time.
func fetchedTools(t *testing.T) []fetchedTool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	vars := makeVars(t)
	var fetched []fetchedTool
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(expandVars(line, vars))
		source := "Makefile"
		// A recipe line may start with the silent `@`, so every launcher is
		// matched with it stripped.
		launch := func(i int) string { return strings.TrimPrefix(fields[i], "@") }
		for i, field := range fields {
			// `uvx` takes uv's own flags before the coordinate (`uvx -q
			// yamllint@1.38.0`), so the tool is the first field after it that
			// is not one. Read before the i == 0 guard below, because a recipe
			// is allowed to start with the launcher.
			if launch(i) == "uvx" {
				for _, next := range fields[i+1:] {
					if strings.HasPrefix(next, "-") {
						continue
					}
					fetched = append(fetched, fetchedTool{source: source, line: line, tool: next})
					break
				}
				continue
			}
			if i == 0 || i+1 >= len(fields) {
				continue
			}
			// `$(GO) run` and `bunx` fetch from a registry or a proxy. A bare
			// `run` after an ordinary word is a recipe running something
			// local, or the prose of a target's help line.
			if (field == "run" && strings.HasPrefix(fields[i-1], "$(")) || launch(i) == "bunx" {
				fetched = append(fetched, fetchedTool{source: source, line: line, tool: fields[i+1]})
			}
		}
	}
	if len(fetched) == 0 {
		t.Fatal("the Makefile parsed to no tool invocations; the parser no longer understands the file")
	}
	return fetched
}

// toolCoordinate splits a fetched coordinate into its package name and the
// version it pins. npm and the Go proxy spell the separator `@`, uv spells it
// `==`, so both count; without a separator the recipe resolved whatever the
// registry served that minute, which is the thing the caller below refuses.
func toolCoordinate(coord string) (name, version string, pinned bool) {
	if name, version, ok := strings.Cut(coord, "@"); ok {
		return name, version, true
	}
	if name, version, ok := strings.Cut(coord, "=="); ok {
		return name, version, true
	}
	return coord, "", false
}

// workflowFiles returns the workflow YAML under .github/workflows, the same
// set the Makefile's WORKFLOWS wildcard names. A job runs with the
// permissions, the environment and the network a workflow gives it, so a
// package a step fetches is as much a dependency as one a recipe fetches.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(moduleRoot, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflow files under .github/workflows; the gate below would pass on an empty set")
	}
	return paths
}

// workflowFetchedTools returns every tool a workflow step pulls from a
// registry while the job runs. `go run` and `go install name@version` resolve
// through the module proxy, `bunx` and `npx` through the npm registry, `uvx`
// and `pip install` through PyPI. A `go install` without a version is not
// matched: `go install tool` names the tool directive in go.mod, which the
// module graph already pins.
func workflowFetchedTools(t *testing.T) []fetchedTool {
	t.Helper()
	var fetched []fetchedTool
	for _, path := range workflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for number, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			add := func(i int, coord string) {
				if coord == "" {
					return
				}
				fetched = append(fetched, fetchedTool{
					source: fmt.Sprintf(".github/workflows/%s:%d", filepath.Base(path), number+1),
					line:   line,
					tool:   coord,
				})
			}
			for i, field := range fields {
				// The launchers that take flags of their own before the
				// coordinate: the first field after the launcher that is not
				// a flag is the package.
				for _, launcher := range []string{"uvx", "bunx", "npx"} {
					if field == launcher {
						for _, next := range fields[i+1:] {
							if strings.HasPrefix(next, "-") {
								continue
							}
							add(i, next)
							break
						}
					}
				}
				// `pip install` names its package after a run of flags, the
				// same shape as the launchers above. A requirements file is
				// not a coordinate: `uv pip install -r file` installs what
				// the file pins, and the file is the place that is checked.
				if (field == "pip" || field == "pip3") && i+1 < len(fields) && fields[i+1] == "install" {
					rest := fields[i+2:]
					for j := 0; j < len(rest); j++ {
						if rest[j] == "-r" || rest[j] == "--requirement" {
							j++
							continue
						}
						if strings.HasPrefix(rest[j], "-") {
							continue
						}
						add(i, rest[j])
						break
					}
				}
				if field != "go" || i+2 >= len(fields) {
					continue
				}
				switch fields[i+1] {
				case "run":
					add(i, fields[i+2])
				case "install":
					if strings.Contains(fields[i+2], "@") {
						add(i, fields[i+2])
					}
				}
			}
		}
	}
	return fetched
}

// TestToolPinsAreExact fails when a recipe fetches a tool without naming a
// version: `go run` without @version resolves whatever the proxy serves that
// minute, and `bunx` without @version installs the latest release. A pin held
// in an assignment counts, because that is where the version pins live.
func TestToolPinsAreExact(t *testing.T) {
	for _, f := range allFetchedTools(t) {
		_, version, ok := toolCoordinate(f.tool)
		if !ok || version == "" || strings.ContainsAny(version, "$ ") {
			t.Errorf("%s fetches a tool without a version: %s; pin it in a variable above the recipe", f.source, f.line)
		}
	}
}

// TestFetchedToolsAreDocumented is the other half of the pin: the coordinate
// has to appear in the dependency table, or the package lands with a version
// nobody recorded a reason for. A row here costs one line, and it is what a
// reader of the table has to check a supply chain against.
func TestFetchedToolsAreDocumented(t *testing.T) {
	reasoned, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	for _, f := range allFetchedTools(t) {
		module, _, _ := toolCoordinate(f.tool)
		if !strings.Contains(string(reasoned), module) {
			t.Errorf("%s fetches %s and it has no entry in %s; record why it is here", f.source, module, dependencyTable)
		}
	}
}

// isCommitSHA reports whether ref is a full 40-character lowercase hex commit
// id, the only action ref that names one immutable tree. A tag or a branch is
// a name the publisher can move, and a run resolves it at dispatch time.
func isCommitSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, r := range ref {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// TestWorkflowActionsAreCommitPinned fails when a workflow uses an action by
// tag or branch. Every third-party action in these workflows is a package a
// release depends on: it runs with the job's token and its network, so a moved
// ref changes what CI executes without a review. A local action (a path under
// this repository) names no ref and is left alone.
func TestWorkflowActionsAreCommitPinned(t *testing.T) {
	pinned := 0
	for _, path := range workflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for number, line := range strings.Split(string(raw), "\n") {
			_, after, ok := strings.Cut(line, "uses:")
			if !ok {
				continue
			}
			// The version a reader wants trails the coordinate as a comment;
			// GitHub resolves the ref alone.
			if hash := strings.Index(after, "#"); hash >= 0 {
				after = after[:hash]
			}
			value := strings.Trim(strings.TrimSpace(after), `"'`)
			if value == "" {
				continue
			}
			if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "docker://") {
				continue
			}
			action, ref, ok := strings.Cut(value, "@")
			if !ok {
				t.Errorf("%s:%d uses %s with no ref; name the commit", filepath.Base(path), number+1, value)
				continue
			}
			if !isCommitSHA(ref) {
				t.Errorf("%s:%d uses %s at %q, a tag or a branch; pin the 40-character commit id", filepath.Base(path), number+1, action, ref)
				continue
			}
			pinned++
		}
	}
	if pinned == 0 {
		t.Fatal("no pinned action ref found in the workflows; the parser no longer understands them")
	}
}
