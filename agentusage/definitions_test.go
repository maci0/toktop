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
	"time"
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

// writeDefs writes body to a definitions file in a fresh temp dir and returns
// its path, for the tests that load it through LoadDefinitions.
func writeDefs(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// dropDefs removes loaded definitions from the registry when the test ends.
// A name the load refused to register is deleted harmlessly, so a rejecting
// test can pass the same list as an accepting one.
func dropDefs(t *testing.T, names ...string) {
	t.Helper()
	t.Cleanup(func() {
		defsMu.Lock()
		for _, name := range names {
			delete(defs, name)
		}
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

func TestLoadDefinitionsRejectsNull(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"null-agent": null}`,
		`{"null-agent": null, "existing-agent": {"usage": {"roots": ["/replacement"]}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			addDef(t, "existing-agent", Spec{Roots: []string{"/original"}})
			path := writeDefs(t, body)
			err := LoadDefinitions(path)
			if !errors.Is(err, ErrInvalidDefinitions) {
				t.Fatalf("LoadDefinitions() = %v, want ErrInvalidDefinitions", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error = %v, want path %q", err, path)
			}
			spec, ok := definedSpec("existing-agent")
			if !ok || !slices.Equal(spec.Roots, []string{"/original"}) {
				t.Fatalf("rejected file changed registry: %+v", spec)
			}
		})
	}
}

func TestLoadDefinitionsEmptyObjects(t *testing.T) {
	for _, body := range []string{`{}`, `{"launchonly": {}}`, `{"launchonly": {"usage": null}}`} {
		t.Run(body, func(t *testing.T) {
			path := writeDefs(t, body)
			if err := LoadDefinitions(path); err != nil {
				t.Fatal(err)
			}
			if _, ok := definedSpec("launchonly"); ok {
				t.Fatal("launch-only definition registered")
			}
		})
	}
}

func TestLoadDefinitionsMissingFileIsNotAnError(t *testing.T) {
	if err := LoadDefinitions(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing file = %v, want nil", err)
	}
}

// A malformed file must be refused: running with a half-loaded agent set is
// worse than refusing.
func TestLoadDefinitionsRejectsMalformedFile(t *testing.T) {
	path := writeDefs(t, "{oops")
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
	body := `{
		"launchonly": {"cmd": ["x"]},
		"noroots": {"usage": {}},
		"": {"usage": {"roots": ["/tmp"]}},
		"full": {"usage": {"roots": ["~/.full/sessions"], "suffix": ".ndjson",
			"cumulative": true, "header_cwd": true}}
	}`
	path := writeDefs(t, body)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "full")

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
	spec.Roots[0] = "mutated"
	spec2, _ := definedSpec("full")
	if spec2.Roots[0] == "mutated" {
		t.Fatal("definedSpec published mutable internal slice")
	}
}

// A definition with no root registers nothing, so it holds no canonical name.
// Recording it in the collision index anyway let it veto a later definition
// that does have a root, rejecting the whole file over an entry the registry
// never sees.
func TestLoadDefinitionsIgnoresRootlessEntryInCollisions(t *testing.T) {
	body := `{"café": {"usage": {}},
		"café": {"usage": {"roots": ["~/.nfc/sessions"]}}}`
	path := writeDefs(t, body)
	if err := LoadDefinitions(path); err != nil {
		t.Fatalf("LoadDefinitions = %v, want the rootless entry skipped, not a collision", err)
	}
	dropDefs(t, "café")

	if _, ok := definedSpec("café"); !ok {
		t.Error("the one usable definition did not register")
	}
}

// Names with surrounding whitespace are the same agent Discover reports, and
// a spec whose roots are all blank is not readable: Supported must not say
// yes for an agent Watch would reject.
func TestLoadDefinitionsTrimsNamesAndSkipsBlankRoots(t *testing.T) {
	body := `{
		"  trimmed  ": {"usage": {"roots": ["~/.trimmed/sessions"]}},
		"blankroots": {"usage": {"roots": ["", "  "]}}
	}`
	path := writeDefs(t, body)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "trimmed")

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
	body := "{\"cafe\\u0301\": {\"usage\": {\"roots\": [\"~/.cafe/sessions\"]}}}"
	path := writeDefs(t, body)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "caf\u00e9", "cafe\u0301")

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
	body := `{"cafe\u0301": {"usage": {"roots": ["~/.nfd/sessions"]}},
		"caf\u00e9d": {"usage": {"roots": ["~/.nfc/sessions"]}}}`
	path := writeDefs(t, body)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "caf\u00e9", "cafe\u0301", "caf\u00e9d")

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
	body := `{"cafe\u0301": {"usage": {"roots": ["~/.nfd/sessions"]}},
		"caf\u00e9": {"usage": {"roots": ["~/.nfc/sessions"]}}}`
	path := writeDefs(t, body)
	err := LoadDefinitions(path)
	if !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("colliding names = %v, want ErrInvalidDefinitions", err)
	}
	if !errors.Is(err, ErrCollidingDefinitions) {
		t.Fatalf("colliding names = %v, want ErrCollidingDefinitions", err)
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

// A relative GAUNTLET_HOME is ignored, like a relative XDG base directory: it
// would resolve agents.json against the working directory, and a missing file
// is not an error there, so the defined agents would vanish silently.
func TestDefinitionsPathIgnoresRelativeGauntletHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	t.Setenv("GAUNTLET_HOME", filepath.Join("relative", "gauntlet"))
	got := DefinitionsPath()
	if !strings.HasPrefix(got, home) {
		t.Errorf("DefinitionsPath() = %q, want the home-relative default under %q", got, home)
	}
}

// The definition a watcher reads from is a coeffect resolved on every poll,
// not a fact fixed at attach: a definition reloaded with a different root must
// reach a watcher that is already running.
func TestWatcherPicksUpReloadedDefinition(t *testing.T) {
	const tool = "zz-reload-def"
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	addDef(t, tool, Spec{Roots: []string{oldRoot}})

	w := Watch(tool, t.TempDir(), time.Now())
	if w == nil {
		t.Fatal("a defined agent with roots should be watchable")
	}
	append_(t, filepath.Join(oldRoot, "session.jsonl"), `{"output_tokens":5,"input_tokens":5}`)
	if got := w.Poll().Output; got != 5 {
		t.Fatalf("output %d, want 5 from the root attached with", got)
	}

	addDef(t, tool, Spec{Roots: []string{newRoot}})
	// Poll forces the fresh walk the periodic path would take a second later.
	append_(t, filepath.Join(newRoot, "session.jsonl"), `{"output_tokens":7,"input_tokens":7}`)
	if got := w.Poll().Output; got != 12 {
		t.Fatalf("output %d, want 12: the reloaded definition was not read", got)
	}
}

// SpecFor is the read side of the definition registry: what a file registered,
// as written, under the canonical name. A caller cannot otherwise tell an entry
// LoadDefinitions skipped from one the file never carried, and it must not be
// able to mutate the registry through the returned slices.
func TestSpecForReportsWhatDefinitionsRegistered(t *testing.T) {
	const tool = "zz-specfor"
	addDef(t, tool, Spec{Roots: []string{"~/.zz/sessions"}, Suffix: ".ndjson"})

	spec, ok := SpecFor("  " + tool + "  ")
	if !ok {
		t.Fatal("SpecFor(name with padding) = false, want the registered spec")
	}
	if !slices.Equal(spec.Roots, []string{"~/.zz/sessions"}) || spec.Suffix != ".ndjson" {
		t.Fatalf("SpecFor() = %+v, want the roots and suffix as registered", spec)
	}
	if _, ok := SpecFor("no-such-agent"); ok {
		t.Error("SpecFor(unregistered) = true, want false")
	}
	if _, ok := SpecFor("claude"); ok {
		t.Error("SpecFor of a built-in adapter = true; those have no definition")
	}
	if !Supported("claude") {
		t.Error("a built-in agent must still be Supported")
	}

	spec.Roots[0] = "/mutated"
	if again, _ := SpecFor(tool); again.Roots[0] != "~/.zz/sessions" {
		t.Error("mutating the returned spec changed the registry")
	}
}

// A definitions file that carries a usage block for an agent registers it, so
// SpecFor sees exactly what LoadDefinitions loaded.
func TestSpecForSeesLoadedDefinitions(t *testing.T) {
	path := writeDefs(t, `{"zz-loaded": {"usage": {"roots": ["~/.loaded/sessions"]}}}`)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "zz-loaded")

	spec, ok := SpecFor("zz-loaded")
	if !ok || !slices.Equal(spec.Roots, []string{"~/.loaded/sessions"}) {
		t.Fatalf("SpecFor(loaded) = %+v, %t", spec, ok)
	}
	if _, ok := SpecFor("launchonly"); ok {
		t.Error("SpecFor(never written) = true, want false")
	}
}

// A usage entry for an agent a compiled-in adapter reads cannot take effect:
// adapterFor prefers the adapter, so registering the spec would leave SpecFor
// reporting roots that Watch ignores. The entry is skipped, which is what makes
// SpecFor the answer to "what did this file register", and the rest of the
// file loads as usual.
func TestLoadDefinitionsSkipsBuiltinAgent(t *testing.T) {
	path := writeDefs(t, `{
		"zz-fine": {"usage": {"roots": ["~/.zz/sessions"]}},
		"claude":  {"usage": {"roots": ["/somewhere/else"]}}
	}`)
	dropDefs(t, "zz-fine")

	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := SpecFor("zz-fine"); !ok {
		t.Error("the entry the file could apply was not registered")
	}
	if _, ok := SpecFor("claude"); ok {
		t.Error("SpecFor registered a spec no watcher would read")
	}
	ad, ok := adapterFor("claude")
	if !ok || !slices.Equal(ad.roots("/tmp"), []string{home(".claude", "projects")}) {
		t.Errorf("the compiled-in claude adapter changed: %v %t", ad.roots("/tmp"), ok)
	}
	if !Supported("claude") {
		t.Error("claude stopped being readable")
	}
}

// ResetDefinitions is the undo LoadDefinitions has no other way to get: the
// loaded agents go, a built-in a file overwrote comes back as compiled, and a
// RegisterSpec adapter is left to UnregisterSpec.
func TestResetDefinitionsRestoresBuiltins(t *testing.T) {
	path := writeDefs(t, `{
		"zz-loaded": {"usage": {"roots": ["~/.loaded/sessions"]}},
		"feynman": {"usage": {"roots": ["/replacement"]}}
	}`)
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := SpecFor("zz-loaded"); !ok {
		t.Fatal("loaded definition not registered")
	}
	ResetDefinitions()
	t.Cleanup(ResetDefinitions)

	if _, ok := SpecFor("zz-loaded"); ok {
		t.Error("SpecFor(loaded) survived ResetDefinitions")
	}
	if Supported("zz-loaded") {
		t.Error("Supported(loaded) = true after ResetDefinitions")
	}
	spec, ok := SpecFor("feynman")
	if !ok || !slices.Equal(spec.Roots, []string{"~/.feynman/sessions"}) {
		t.Errorf("SpecFor(feynman) = %+v, %t, want the compiled-in roots", spec, ok)
	}
	if !Supported("feynman") {
		t.Error("Supported(feynman) = false after restoring the compiled-in definition")
	}
}
