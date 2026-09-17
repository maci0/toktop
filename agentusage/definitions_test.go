// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// addDef registers a definition directly and removes it when the test ends.
func addDef(t *testing.T, name string, spec Spec) {
	t.Helper()
	defsMu.Lock()
	defs[name] = spec
	defsMu.Unlock()
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, name)
		defsMu.Unlock()
	})
}

// Agents backs the --agents picker and Discover: built-ins must stay listed,
// runtime definitions join them sorted, and a definition shadowing a built-in
// must not appear twice.
func TestAgentsMergesBuiltinsAndDefinitions(t *testing.T) {
	addDef(t, "zzdefined", Spec{Roots: []string{"~/.zz/sessions"}})
	addDef(t, "codex", Spec{Roots: []string{"~/.shadowing/sessions"}})

	got := Agents()
	if !slices.Contains(got, "claude") {
		t.Errorf("built-in claude missing from %v", got)
	}
	if !slices.Contains(got, "zzdefined") {
		t.Errorf("defined agent missing from %v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("agent list not sorted: %v", got)
	}
	n := 0
	for _, a := range got {
		if a == "codex" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("codex appears %d times, want exactly once: %v", n, got)
	}
}

func TestAgentsIncludesRegisteredSpecs(t *testing.T) {
	const tool = "registered-agent"
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, tool)
		adaptersMu.Unlock()
	})
	if err := RegisterSpec("  "+tool+"  ", Spec{Roots: []string{t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	got := Agents()
	if !slices.Contains(got, tool) {
		t.Fatalf("registered agent missing from %v", got)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("agent list not sorted: %v", got)
	}
	addDef(t, tool, Spec{Roots: []string{t.TempDir()}})
	got = Agents()
	if !slices.Equal(got, slices.Compact(slices.Clone(got))) {
		t.Fatalf("agent list contains duplicates: %v", got)
	}
	got[0] = "changed-by-caller"
	if slices.Contains(Agents(), "changed-by-caller") {
		t.Fatal("mutating the returned list changed the registry")
	}
}

// A machine without agent definitions is ordinary; the loader must say so by
// succeeding quietly.
func TestLoadDefinitionsMissingFileIsNotAnError(t *testing.T) {
	if err := LoadDefinitions(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing file = %v, want nil", err)
	}
}

// A malformed file must be refused: running with a half-loaded agent set is
// worse than refusing.
func TestLoadDefinitionsRejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := LoadDefinitions(path)
	if err == nil {
		t.Fatal("malformed definitions accepted")
	}
	if !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("malformed file = %v, want ErrInvalidDefinitions", err)
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
		t.Fatalf("malformed file = %v, want wrapped json.SyntaxError", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("malformed file = %v, want the path %q in the message", err, path)
	}
}

// Only token-bearing definitions mean anything here: a launch-only entry says
// nothing about transcripts and must be skipped, a blank name is unusable,
// and a full entry carries every usage field through.
func TestLoadDefinitionsRegistersOnlyTokenBearingSpecs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := `{
		"launchonly": {"cmd": ["x"]},
		"noroots": {"usage": {}},
		"": {"usage": {"roots": ["/tmp"]}},
		"full": {"usage": {"roots": ["~/.full/sessions"], "suffix": ".ndjson",
			"cumulative": true, "header_cwd": true}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, "full")
		defsMu.Unlock()
	})

	if _, ok := definedSpec("launchonly"); ok {
		t.Error("launch-only definition registered")
	}
	if _, ok := definedSpec("noroots"); ok {
		t.Error("roots-less definition registered")
	}
	if _, ok := definedSpec(""); ok {
		t.Error("blank-named definition registered")
	}
	spec, ok := definedSpec("full")
	if !ok {
		t.Fatal("full definition not registered")
	}
	want := Spec{Roots: []string{"~/.full/sessions"}, Suffix: ".ndjson",
		Cumulative: true, HeaderCwd: true}
	if !slices.Equal(spec.Roots, want.Roots) || spec.Suffix != want.Suffix ||
		spec.Cumulative != want.Cumulative || spec.HeaderCwd != want.HeaderCwd {
		t.Errorf("spec = %+v, want %+v", spec, want)
	}
}

// Names with surrounding whitespace are the same agent Discover reports, and
// a spec whose roots are all blank is not readable: Supported must not say
// yes for an agent Watch would reject.
func TestLoadDefinitionsTrimsNamesAndSkipsBlankRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := `{
		"  trimmed  ": {"usage": {"roots": ["~/.trimmed/sessions"]}},
		"blankroots": {"usage": {"roots": ["", "  "]}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, "trimmed")
		defsMu.Unlock()
	})

	if _, ok := definedSpec("trimmed"); !ok {
		t.Fatal("whitespace name should be stored trimmed")
	}
	defsMu.RLock()
	_, storedUntrimmed := defs["  trimmed  "]
	defsMu.RUnlock()
	if storedUntrimmed {
		t.Fatal("untrimmed key should not be stored")
	}
	if _, ok := definedSpec("  trimmed  "); !ok {
		t.Fatal("lookup of the untrimmed name should still find the spec")
	}
	if _, ok := definedSpec("blankroots"); ok {
		t.Fatal("blank-root spec should not be stored")
	}
	if Supported("blankroots") {
		t.Fatal("Supported must not claim a spec Watch would reject")
	}
}

// NFD "café" (e + combining acute) and NFC "café" must be one agent: a
// macOS-typed definitions file and a precomposed JSON key would otherwise
// register two specs for the same name.
func TestLoadDefinitionsNormalizesNamesToNFC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := "{\"cafe\\u0301\": {\"usage\": {\"roots\": [\"~/.cafe/sessions\"]}}}"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, "caf\u00e9")
		delete(defs, "cafe\u0301")
		defsMu.Unlock()
	})

	if _, ok := definedSpec("caf\u00e9"); !ok {
		t.Fatal("NFC lookup missed the NFD-defined agent")
	}
	if _, ok := definedSpec("cafe\u0301"); !ok {
		t.Fatal("NFD lookup should compose to the same agent")
	}
	defsMu.RLock()
	_, nfdKey := defs["cafe\u0301"]
	nfcKey := defs["caf\u00e9"]
	defsMu.RUnlock()
	if nfdKey {
		t.Fatal("NFD spelling should not be stored as a separate key")
	}
	if len(nfcKey.Roots) == 0 {
		t.Fatal("NFC spelling should be the stored key")
	}
}

// NFD spellings are canonicalized per name, not merged across names: an
// NFD-keyed definition beside a different NFC-keyed one registers two
// distinct agents.
func TestLoadDefinitionsKeepsDistinctNFDSpellingSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := `{"cafe\u0301": {"usage": {"roots": ["~/.nfd/sessions"]}},
		"caf\u00e9d": {"usage": {"roots": ["~/.nfc/sessions"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, "caf\u00e9")
		delete(defs, "cafe\u0301")
		delete(defs, "caf\u00e9d")
		defsMu.Unlock()
	})

	nfd, nfdOK := definedSpec("cafe\u0301")
	if !nfdOK {
		t.Fatal("NFD-spelled agent not registered")
	}
	nfc, nfcOK := definedSpec("caf\u00e9d")
	if !nfcOK {
		t.Fatal("NFC-spelled agent not registered")
	}
	if slices.Equal(nfd.Roots, nfc.Roots) {
		t.Fatalf("two distinct spellings collapsed to one spec: %+v", nfd)
	}
}

// Two names that NFC reduces to one key (NFD beside precomposed) would
// silently overwrite each other in defs, leaving whichever entry iterated
// last. The loader must refuse the ambiguous file without registering
// anything.
func TestLoadDefinitionsRejectsNFCCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := `{"cafe\u0301": {"usage": {"roots": ["~/.nfd/sessions"]}},
		"caf\u00e9": {"usage": {"roots": ["~/.nfc/sessions"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	err := LoadDefinitions(path)
	if !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("colliding names = %v, want ErrInvalidDefinitions", err)
	}
	if !errors.Is(err, errCollidingDefinitions) {
		t.Fatalf("colliding names = %v, want errCollidingDefinitions", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("collision error = %v, want the path %q in the message", err, path)
	}
	defsMu.RLock()
	_, nfdStored := defs["cafe\u0301"]
	_, nfcStored := defs["caf\u00e9"]
	defsMu.RUnlock()
	if nfdStored || nfcStored {
		t.Fatal("refused file must not leave either spelling registered")
	}
}

// DefinitionsPath follows gauntlet's file so one definition serves both tools;
// GAUNTLET_HOME wins over the home-relative default.
func TestDefinitionsPathPrefersGauntletHome(t *testing.T) {
	gh := t.TempDir()
	t.Setenv("GAUNTLET_HOME", gh)
	if got, want := DefinitionsPath(), filepath.Join(gh, "agents.json"); got != want {
		t.Errorf("DefinitionsPath() = %q, want %q", got, want)
	}

	home := t.TempDir()
	t.Setenv("GAUNTLET_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	got := DefinitionsPath()
	if !strings.HasSuffix(got, filepath.Join(".gauntlet", "agents.json")) ||
		!strings.HasPrefix(got, home) {
		t.Errorf("DefinitionsPath() = %q, want the home-relative default under %q", got, home)
	}
}
