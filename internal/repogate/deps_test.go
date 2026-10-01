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
	documented := documentedNames(t)
	for _, module := range directRequires(t) {
		used := slices.ContainsFunc(imports, func(path string) bool {
			return path == module || strings.HasPrefix(path, module+"/")
		})
		if !used {
			t.Errorf("%s is required by go.mod and imported nowhere; remove it or import it", module)
		}
		if !documented[module] {
			t.Errorf("%s is required by go.mod and has no row in %s; record why it is here", module, dependencyTable)
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

// documentedNames returns every name a table row in docs/DEPENDENCIES.md
// carries: the module, the tool coordinate or the Makefile pin, in whichever
// column that table puts it. A name the file only mentions in prose does not
// count. The license and the reason a reader checks a supply chain against
// live in the row, so a check that asks only whether the file mentions a name
// passes after the row carrying both has been deleted.
func documentedNames(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for _, cell := range strings.Split(line, "|") {
			cell = strings.Trim(strings.TrimSpace(cell), "`")
			// A cell holding whitespace is prose: the license and the reason
			// columns. Only the bare name column names a package.
			if cell == "" || strings.ContainsAny(cell, " \t") {
				continue
			}
			names[cell] = true
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s parsed to no table rows; the parser no longer understands the file", dependencyTable)
	}
	return names
}

func TestDependencyTableMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	direct := directRequires(t)
	rows := 0
	for _, line := range strings.Split(string(raw), "\n") {
		module := documentedModule(line)
		if module == "" {
			continue
		}
		rows++
		if !slices.Contains(direct, module) {
			t.Errorf("%s is documented in %s but is not a direct require in go.mod", module, dependencyTable)
		}
	}
	if rows == 0 {
		t.Fatalf("%s parsed to no dependency rows; the parser no longer understands the file", dependencyTable)
	}
}

// The module's tiers, lowest first. A package may import a package in a
// strictly lower tier and nothing else: a lower layer reaching back up
// inverts the dependency direction and is what makes a tree hard to change,
// since the two packages then have to move together.
//
//	0  core              the types and helpers everything is written in terms of
//	1  logcfg            the audit-log vocabulary, below every package that logs
//	2  bearer, lockfile, procs, selfreload, agentusage
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
	{"internal/bearer", "internal/lockfile", "internal/procs", "internal/selfreload", "agentusage"},
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
	for dir := range modulePackages(t) {
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
	for dir := range modulePackages(t) {
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

// modulePackages returns every package in the module by import path, and
// fails the test when the walk found none: a walk that matches no directory
// leaves the tier and architecture-map checks with nothing to say, and both
// pass.
func modulePackages(t *testing.T) map[string]bool {
	t.Helper()
	dirs, err := packageDirs(moduleRoot)
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("the module walk found no package directory; the checks below would pass on an empty set")
	}
	return dirs
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

// pythonPin is one requirement line in a requirements file under scripts/,
// with the count of sha256 hashes written on the lines that continue it.
type pythonPin struct {
	file    string
	name    string
	version string
	hashes  int
}

// pythonPinRecords returns every pin in the requirements files under
// scripts/, each with its version and the hashes on its continuation lines.
// Both files, because a package that moves from runtime to tooling is still a
// pin the gates below have to account for.
func pythonPinRecords(t *testing.T) []pythonPin {
	t.Helper()
	var pins []pythonPin
	for _, file := range []string{"requirements.txt", "requirements-dev.txt"} {
		raw, err := os.ReadFile(filepath.Join(moduleRoot, "scripts", file))
		if err != nil {
			t.Fatalf("read scripts/%s: %v", file, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			// A backslash continues the requirement onto the next line, and
			// it is how every hash in these files is attached. The
			// continuation itself is the line after this one, so the marker
			// goes and the line is read as the requirement it belongs to.
			line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), `\`))
			// Comments carry the reason a pin is there; `-r` pulls in the
			// other file, whose pins this walks directly.
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-r ") {
				continue
			}
			if strings.HasPrefix(line, "--") {
				// A `--hash` continues the pin above it. Any other option
				// is an install instruction with no pin to continue, so
				// there is nothing to attribute it to.
				if strings.HasPrefix(line, "--hash=") && len(pins) > 0 {
					pins[len(pins)-1].hashes++
				}
				continue
			}
			name, version, exact := strings.Cut(line, "==")
			name, version = strings.TrimSpace(name), strings.TrimSpace(version)
			if !exact || name == "" || version == "" || strings.ContainsAny(name+version, " \t") {
				t.Fatalf("scripts/%s: %q is not an exact name==version pin; a range or a bare name lets the index choose what gets installed", file, line)
			}
			pins = append(pins, pythonPin{file: file, name: name, version: version})
		}
	}
	if len(pins) == 0 {
		t.Fatal("the requirements files parsed to no pins; the parser no longer understands them")
	}
	return pins
}

// pythonPins returns the distribution name of every pin in the requirements
// files under scripts/.
func pythonPins(t *testing.T) []string {
	t.Helper()
	records := pythonPinRecords(t)
	names := make([]string, 0, len(records))
	for _, pin := range records {
		names = append(names, pin.name)
	}
	return names
}

// pythonUnhashed names the pins written as a bare version, with no
// --hash=sha256 line beside them, and why the hash is not there. A hash is
// what makes a registry swap of that file fail the install, so a pin without
// one takes whatever the index serves for the version. Every name is a
// distribution whose wheel carries a platform and interpreter tag
// (readable as the Tag: lines in the installed .dist-info/WHEEL), so hashing
// the one wheel a developer happens to have would refuse every other
// OS/arch/CPython. A pin added to a requirements file is either hashed or
// added here with its reason, and both directions fail the run when they name
// a pin that is gone.
var pythonUnhashed = map[string]string{
	"ast-serialize": "wheel is tagged for one interpreter and platform",
	"black":         "wheel is tagged for one interpreter and platform",
	"librt":         "wheel is tagged for one interpreter and platform",
	"mypy":          "wheel is tagged for one interpreter and platform",
	"pillow":        "wheel is tagged for one interpreter and platform",
	"pytokens":      "wheel is tagged for one interpreter and platform",
	"pyyaml":        "wheel is tagged for one interpreter and platform",
	"ruff":          "wheel is tagged for one interpreter and platform",
}

// TestPythonPinsAreExactAndHashed fails when a pin in scripts/ is not an
// exact name==version, or is exact and carries no sha256 and is not on the
// list of pins the tree decided to run unhashed. Neither failure was caught
// before: pythonPins read a range as part of the distribution's name, so a
// pin that had drifted to `>=` only tripped the documentation check by
// accident, and nothing in the run looked at the hash lines at all, so the
// claim in docs/DEPENDENCIES.md that the pure-Python packages are hashed had
// no gate under it.
func TestPythonPinsAreExactAndHashed(t *testing.T) {
	records := pythonPinRecords(t)
	for _, pin := range records {
		_, exempt := pythonUnhashed[pin.name]
		if pin.hashes == 0 && !exempt {
			t.Errorf("scripts/%s: %s==%s carries no --hash=sha256 line and is not in pythonUnhashed; hash it, or record it there with the reason", pin.file, pin.name, pin.version)
		}
		if pin.hashes > 0 && exempt {
			t.Errorf("scripts/%s: %s==%s carries a hash and is still in pythonUnhashed; drop it from that list", pin.file, pin.name, pin.version)
		}
	}
	for name := range pythonUnhashed {
		if !slices.ContainsFunc(records, func(pin pythonPin) bool { return pin.name == name }) {
			t.Errorf("pythonUnhashed names %s, which no requirements file pins any more", name)
		}
	}
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

// pythonLicenseRow returns the line of the Python table in the dependency
// table that names this pin, or "" when none does. The row is what a reader
// checks a supply chain against, and a pin named only in prose carries no
// license at all.
func pythonLicenseRow(t *testing.T, pin string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, dependencyTable))
	if err != nil {
		t.Fatalf("read %s: %v", dependencyTable, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		// The pin names the row. A trailing " (...)" is tolerated so a row can
		// carry the version or a note beside the name without the lookup
		// below having to know how it was spelled.
		first := strings.Trim(strings.TrimSpace(strings.Split(line, "|")[1]), "`")
		if first == pin || strings.HasPrefix(first, pin+" (") {
			return line
		}
	}
	return ""
}

// licenseIdentifiers matches an SPDX identifier in a cell of the table:
// MIT, BSD-3-Clause, Apache-2.0, GPL-3.0-or-later, LGPL-3.0, MPL-2.0, PSF-2.0
// and the "A OR B" form. It is deliberately loose about the version suffix --
// the point is that a license is named at all, not that the number matches.
var licenseIdentifiers = regexp.MustCompile(`\b(?:A?GPL|LGPL|MPL|MIT|BSD|Apache|PSF|ISC)[-\w.]*`)

// TestPythonPinsRecordALicense fails when a pin in scripts/ has a row in the
// dependency table naming it with no license beside it.
//
// The gap it closes is one this tree already fell into: yamllint was recorded
// as LGPL-2.1 while its installed metadata says GPL-3.0-or-later, and no gate
// read that column. TestPythonPinsAreDocumented only asks whether the name
// appears anywhere in the file, so a weaker copyleft identifier sat there
// indefinitely -- and a weaker identifier is the one kind of license error a
// reader cannot catch by reading the file, because the file is what they read.
//
// The table is the source of truth rather than the installed metadata: the
// env under dist/ is not built for a bare `go test`, and a gate that needed it
// would stop being a gate.
//
// Ceiling: this checks that a license is NAMED, not that it is the right one,
// so downgrading an identifier to a weaker one (yamllint to LGPL-2.1) passes
// here. Comparing against the installed metadata would catch that, but it
// needs the dist/ env, which a bare `go test` does not have. The upgrade path
// is a `make scripts-check` step that diffs the table's license column against
// each pin's .dist-info, which is where the env is guaranteed to exist.
func TestPythonPinsRecordALicense(t *testing.T) {
	records := pythonPinRecords(t)
	if len(records) == 0 {
		t.Fatal("the requirements files parsed to no pins; the parser no longer understands them")
	}
	for _, pin := range records {
		row := pythonLicenseRow(t, pin.name)
		if row == "" {
			t.Errorf("scripts/%s: %s==%s has no row in %s; the pin needs a license beside it", pin.file, pin.name, pin.version, dependencyTable)
			continue
		}
		cells := strings.Split(row, "|")
		// A table row splits with an empty cell on either side of the
		// delimiters, so the license is the third cell, not the second. Only
		// it is read: the fourth is prose, and a reason that happens to
		// contain a word like "Apache" would otherwise pass a row that names
		// no license.
		if len(cells) < 3 || !licenseIdentifiers.MatchString(cells[2]) {
			t.Errorf("scripts/%s: %s==%s has a row in %s naming no license: %s", pin.file, pin.name, pin.version, dependencyTable, strings.TrimSpace(row))
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
// a package the tree fetches from PyPI and never runs. Empty while every pin
// is imported by a file under scripts/ itself: wcwidth sat here until the
// screenshot renderer measured a capture row in display columns, and pyte,
// whose pin it was, has always been imported directly.
var pythonClosure = map[string]string{}

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

// expandVars substitutes $(NAME) with the assignment makeVars read, and keeps
// going through the value it substituted until none is left, so a pin defined
// in terms of another (`BIOME := @biomejs/biome@$(BIOME_VERSION)`) arrives
// expanded rather than carrying the inner reference into the gates below.
// Anything it cannot resolve is left alone so the caller can name it. The
// bound is a backstop against two assignments naming each other.
func expandVars(s string, vars map[string]string) string {
	for range maxVarDepth {
		expanded := expandVarsOnce(s, vars)
		if expanded == s {
			return s
		}
		s = expanded
	}
	return s
}

// maxVarDepth bounds how many times expandVars follows a chain of assignments
// before it gives up on a cycle.
const maxVarDepth = 16

func expandVarsOnce(s string, vars map[string]string) string {
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
			// `bunx` is a launcher like `uvx`, so it is read before the guard
			// below for the same reason: a recipe is allowed to start with
			// the launcher, and `bunx $(BIOME) check` is exactly that shape.
			if launch(i) == "bunx" && i+1 < len(fields) {
				fetched = append(fetched, fetchedTool{source: source, line: line, tool: fields[i+1]})
				continue
			}
			if i == 0 || i+1 >= len(fields) {
				continue
			}
			// `$(GO) run` fetches through the module proxy. A bare `run` after
			// an ordinary word is a recipe running something local, or the
			// prose of a target's help line. `go run` takes flags of its own
			// before the package, and the recipes put them there: the module
			// flags and the build tags the tree pins live in variables, and
			// GOTAGS spells its value as `$(strip $(TAGS) $(ZONE_TAG))`, whose
			// parens expandVars cannot close. A field still carrying one is a
			// fragment of a function call rather than a word the recipe
			// passes, so the coordinate is the first field after the flags
			// with no paren in it. A field naming a path inside the module is
			// the tree running its own code, and the rest of that line are its
			// arguments.
			if field == "run" && strings.HasPrefix(fields[i-1], "$(") {
				for _, next := range fields[i+1:] {
					// A quoted word carries its quote with it (`-tags "..."`
					// is one field), so the class is read off the word
					// without it.
					next = strings.Trim(next, `"`)
					if strings.HasPrefix(next, "-") || strings.ContainsAny(next, "()") {
						continue
					}
					if !strings.HasPrefix(next, "./") {
						fetched = append(fetched, fetchedTool{source: source, line: line, tool: next})
					}
					break
				}
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
// registry served that minute, which is the thing the callers below refuse.
// An npm scope opens with `@` (`@biomejs/biome@2.5.14`), so the separator is
// the last one: cutting at the first splits the scope off and hands the caller
// a coordinate with no name, which then matches any document at all.
func toolCoordinate(coord string) (name, version string, pinned bool) {
	if at := strings.LastIndex(coord, "@"); at > 0 {
		return coord[:at], coord[at+1:], true
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
//
// GitHub Actions reads both extensions, so both are globbed: a workflow
// renamed to .yaml still runs on the job's token, and a gate that read only
// .yml would stop holding the one file that now says what CI executes. A
// `uses:` on a tag would pass a run that no longer reads it.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(moduleRoot, ".github", "workflows", pattern))
		if err != nil {
			t.Fatalf("glob workflows: %v", err)
		}
		paths = append(paths, found...)
	}
	slices.Sort(paths)
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
	documented := documentedNames(t)
	for _, f := range allFetchedTools(t) {
		module, _, _ := toolCoordinate(f.tool)
		if module == "" {
			t.Errorf("%s fetches %q, which names no package; the check below would match every document", f.source, f.tool)
			continue
		}
		if !documented[module] {
			t.Errorf("%s fetches %s and it has no row in %s; record why it is here", f.source, module, dependencyTable)
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
