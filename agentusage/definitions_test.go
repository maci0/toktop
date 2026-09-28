// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// addDef registers a definition directly and removes it when the test ends.
// The generation moves with it: a watcher caches the adapter it derived, and
// the counter is what tells it the spec it read is no longer the one loaded.
func addDef(t *testing.T, name string, spec Spec) {
	t.Helper()
	defsMu.Lock()
	defs[name] = spec
	bumpDefsGen()
	defsMu.Unlock()
	t.Cleanup(func() {
		defsMu.Lock()
		delete(defs, name)
		bumpDefsGen()
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
			spec, ok := SpecFor("existing-agent")
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
			if _, ok := SpecFor("launchonly"); ok {
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

// A path that is there but cannot be read is refused the same way a malformed
// one is, so a caller asking errors.Is whether this file is usable gets one
// answer rather than two. A directory stands in for every unreadable path: the
// read fails, no platform reaches the definition parser, and the test says
// nothing about what a permission failure looks like on a run as root.
func TestLoadDefinitionsRejectsUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	err := LoadDefinitions(dir)
	if err == nil {
		t.Fatal("a directory was accepted as a definitions file")
	}
	if !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("unreadable path = %v, want ErrInvalidDefinitions", err)
	}
	if _, ok := errors.AsType[*fs.PathError](err); !ok {
		t.Fatalf("unreadable path = %v, want the I/O failure still wrapped as *fs.PathError", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error = %v, want the path %q in the message", err, dir)
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

// A file past the cap is refused, not read: it cannot be a definitions file,
// and json.Unmarshal builds a second copy of everything it decodes, so an
// uncapped read costs twice what the file is. The refusal is the same error a
// malformed file gets, so the caller contract (every refusal of a file that
// exists is ErrInvalidDefinitions) holds, and it registers nothing.
func TestLoadDefinitionsRejectsOversizeFile(t *testing.T) {
	// A valid document padded past the cap: the size is what must be refused,
	// not the JSON, which decodes cleanly on its own.
	body := `{"a":{"usage":{"roots":["~"]}},"pad":"` + strings.Repeat("x", maxDefinitionsBytes) + `"}`
	path := writeDefs(t, body)
	saved := snapshotDefs()
	t.Cleanup(func() { restoreDefs(saved) })

	err := LoadDefinitions(path)
	if err == nil {
		t.Fatal("oversize definitions accepted")
	}
	if !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("oversize file = %v, want ErrInvalidDefinitions", err)
	}
	if _, ok := SpecFor("a"); ok {
		t.Error("rejected oversize file still registered an agent")
	}
}

// A file at the cap is still read: the bound is on what is refused, not on
// what is accepted. The padding is inter-token whitespace, which keeps the
// document valid JSON without adding a top-level key the value struct cannot
// hold.
func TestLoadDefinitionsAcceptsFileAtCap(t *testing.T) {
	const prefix = `{"a":{"usage":{"roots":["~"]}}`
	body := prefix + strings.Repeat(" ", maxDefinitionsBytes-len(prefix)-1) + "}"
	path := writeDefs(t, body)
	dropDefs(t, "a")

	if err := LoadDefinitions(path); err != nil {
		t.Fatalf("file at the cap = %v, want nil", err)
	}
	if _, ok := SpecFor("a"); !ok {
		t.Error("file at the cap did not register its agent")
	}
}

// Every LoadDefinitions diagnostic names the file, and the file is an absolute
// path under $HOME: it names the account, and these are the lines pasted into
// issues. The home is folded out of all of them.
func TestLoadDefinitionsErrorsRedactHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unreadable", ""},
		{"malformed", "{oops"},
		{"null", "null"},
		{"null agent", `{"a":null}`},
		{"colliding", "{\"cafe\\u0301\":{\"usage\":{\"roots\":[\"~\"]}},\"caf\\u00e9\":{\"usage\":{\"roots\":[\"~\"]}}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(home, "agents.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				// A directory where the file belongs: a read error, which is
				// the one refusal that is not ErrInvalidDefinitions.
				path = filepath.Join(home, "as-directory")
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err := LoadDefinitions(path)
			if err == nil {
				t.Fatal("bad definitions accepted")
			}
			if strings.Contains(err.Error(), home) {
				t.Errorf("error leaks the home directory: %v", err)
			}
		})
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

	if _, ok := SpecFor("launchonly"); ok {
		t.Error("launch-only definition registered")
	}
	if _, ok := SpecFor("noroots"); ok {
		t.Error("roots-less definition registered")
	}
	if _, ok := SpecFor(""); ok {
		t.Error("blank-named definition registered")
	}
	spec, ok := SpecFor("full")
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
	spec2, _ := SpecFor("full")
	if spec2.Roots[0] == "mutated" {
		t.Fatal("SpecFor published mutable internal slice")
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

	if _, ok := SpecFor("café"); !ok {
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

	if _, ok := SpecFor("trimmed"); !ok {
		t.Fatal("whitespace name should be stored trimmed")
	}
	defsMu.RLock()
	_, storedUntrimmed := defs["  trimmed  "]
	defsMu.RUnlock()
	if storedUntrimmed {
		t.Fatal("untrimmed key should not be stored")
	}
	if _, ok := SpecFor("  trimmed  "); !ok {
		t.Fatal("lookup of the untrimmed name should still find the spec")
	}
	if _, ok := SpecFor("blankroots"); ok {
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

	if _, ok := SpecFor("caf\u00e9"); !ok {
		t.Fatal("NFC lookup missed the NFD-defined agent")
	}
	if _, ok := SpecFor("cafe\u0301"); !ok {
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

	nfd, nfdOK := SpecFor("cafe\u0301")
	if !nfdOK {
		t.Fatal("NFD-spelled agent not registered")
	}
	nfc, nfcOK := SpecFor("caf\u00e9d")
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
	if !ok || !slices.Equal(ad.roots("/tmp", time.Time{}), []string{home(".claude", "projects")}) {
		t.Errorf("the compiled-in claude adapter changed: %v %t", ad.roots("/tmp", time.Time{}), ok)
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

// A program that generates or edits agents.json goes through Definitions, so
// the exported type has to be the file this package reads: a round trip
// through it registers the same spec, keeps the launch fields an entry beside
// it carries (this package ignores them, a round trip must not delete them),
// and reads back the spelling a person wrote rather than the canonical one.
func TestDefinitionsRoundTripLoads(t *testing.T) {
	file := Definitions{
		"myagent": {Usage: &Spec{
			Roots:      []string{"~/.myagent/sessions"},
			Suffixes:   []string{".jsonl", ".jsonl.zstd"},
			Cumulative: true,
			HeaderCwd:  true,
		}},
		"launchonly": {},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := writeDefs(t, string(data))
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "myagent")

	spec, ok := SpecFor("myagent")
	if !ok {
		t.Fatal("the round-tripped definition was not registered")
	}
	want := Spec{
		Roots:      []string{"~/.myagent/sessions"},
		Suffixes:   []string{".jsonl", ".jsonl.zstd"},
		Cumulative: true,
		HeaderCwd:  true,
	}
	if !slices.Equal(spec.Roots, want.Roots) || !slices.Equal(spec.Suffixes, want.Suffixes) ||
		spec.Suffix != want.Suffix || spec.Cumulative != want.Cumulative || spec.HeaderCwd != want.HeaderCwd {
		t.Errorf("SpecFor(myagent) = %+v, want %+v", spec, want)
	}
	if _, ok := SpecFor("launchonly"); ok {
		t.Error("an entry carrying no usage block should register nothing")
	}

	// The same bytes unmarshal back into the value that produced them, so a
	// program that rewrites the file keeps the keys as written.
	var back Definitions
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(back)), slices.Sorted(maps.Keys(file))) {
		t.Errorf("round trip changed the agent names: %v, want %v", maps.Keys(back), maps.Keys(file))
	}
}

// Agents documents that the two database-backed agents are the only built-in
// names a build may not be able to read, and the rest are readable whatever
// the sqlite tag says. The registry is read here rather than the names
// Agents returns, since that list also carries whatever a neighbouring test
// registered.
func TestKnownAgentsAreReadableApartFromTheDatabaseOnes(t *testing.T) {
	for _, name := range knownAgents {
		if name == "crush" || name == "opencode" {
			// Both are behind the sqlite build tag, and opencode behind
			// EnableOpenCodeDB on top, so neither has one answer to assert.
			continue
		}
		if !Supported(name) {
			t.Errorf("Supported(%q) = false, want true", name)
		}
	}
}

// A key the usage block has no field for is read by nobody, and a block
// naming only such a key registers no transcripts, so the agent reads as one
// that used no tokens. The file is gauntlet's and a newer gauntlet may add a
// key here, so the answer is a report rather than a refusal: the load succeeds
// and names what it could not read.
func TestLoadDefinitionsReportsUnknownUsageKeys(t *testing.T) {
	path := writeDefs(t, `{
		"zeta":  {"usage": {"root": ["~/.zeta/sessions"]}},
		"alpha": {"usage": {"roots": ["~/.alpha/sessions"], "sufix": ".jsonl", "header_cwd": true}},
		"mid":   {"usage": {"roots": ["~/.mid/sessions"], "cumulative": true}}
	}`)
	dropDefs(t, "zeta", "alpha", "mid")
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha: sufix", "zeta: root"}
	if got := UnknownUsageKeys(); !slices.Equal(got, want) {
		t.Errorf("UnknownUsageKeys() = %q, want %q", got, want)
	}
	// A block whose only unrecognized key is the typo registers nothing, and
	// the keys beside it are read as written.
	if _, ok := SpecFor("alpha"); !ok {
		t.Error("alpha not registered; the roots key beside the typo was ignored")
	}
	if _, ok := SpecFor("mid"); !ok {
		t.Error("mid not registered")
	}
	if _, ok := SpecFor("zeta"); ok {
		t.Error("zeta registered from an unrecognized root key")
	}
}

// A file naming no key outside the set reports nothing, and the answer follows
// the file in force rather than accumulating across loads.
func TestUnknownUsageKeysFollowsTheFileInForce(t *testing.T) {
	known := writeDefs(t, `{"alpha": {"usage": {"roots": ["~/.alpha/sessions"], "suffix": ".jsonl"}}}`)
	if err := LoadDefinitions(known); err != nil {
		t.Fatal(err)
	}
	if got := UnknownUsageKeys(); len(got) != 0 {
		t.Errorf("UnknownUsageKeys() = %q for a file naming only known keys, want none", got)
	}
	typo := writeDefs(t, `{"beta": {"usage": {"roots": ["~/.beta/sessions"], "sufixes": [".jsonl"]}}}`)
	dropDefs(t, "beta")
	if err := LoadDefinitions(typo); err != nil {
		t.Fatal(err)
	}
	if got, want := UnknownUsageKeys(), []string{"beta: sufixes"}; !slices.Equal(got, want) {
		t.Errorf("UnknownUsageKeys() = %q, want %q", got, want)
	}
}

// usageKeyNames has to name every key the usage struct decodes, or the message
// printed beside an unrecognized key omits one that does work. Spelling one
// entry per known key and getting no report back is the assertion that binds
// the list to the decoder.
func TestUsageKeyNamesAreAllDecoded(t *testing.T) {
	values := map[string]string{
		"cumulative": "true",
		"header_cwd": "true",
		"roots":      `["~/.alpha/sessions"]`,
		"suffix":     `".jsonl"`,
		"suffixes":   `[".jsonl"]`,
	}
	names := UsageKeyNames()
	if !slices.Equal(names, []string{"cumulative", "header_cwd", "roots", "suffix", "suffixes"}) {
		t.Errorf("UsageKeyNames() = %q, want the sorted set of decoded keys", names)
	}
	for _, key := range names {
		value, ok := values[key]
		if !ok {
			t.Errorf("UsageKeyNames() names %q, which this test spells no value for", key)
			continue
		}
		path := writeDefs(t, `{"alpha": {"usage": {"`+key+`": `+value+`}}}`)
		dropDefs(t, "alpha")
		if err := LoadDefinitions(path); err != nil {
			t.Fatal(err)
		}
		if got := UnknownUsageKeys(); len(got) != 0 {
			t.Errorf("key %q is decoded but reported unknown: %q", key, got)
		}
	}
}

// A home that is relative, or that cannot be located at all, names no store.
// The rule GAUNTLET_HOME, KIMI_CODE_HOME and the XDG base directories are held
// to is that an unusable value resolves to nothing rather than to a path under
// the directory the run started in, where a missing store is an empty one and
// every agent reports no tokens.
func TestHomeDirRejectsUnusableHome(t *testing.T) {
	for _, tt := range []struct{ name, value string }{
		{"relative", filepath.Join("relative", "home")},
		{"dot", "."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", tt.value)
			t.Setenv("USERPROFILE", tt.value) // os.UserHomeDir on windows
			t.Setenv("GAUNTLET_HOME", "")
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("KIMI_CODE_HOME", "")
			if got := HomeDir(); got != "" {
				t.Fatalf("HomeDir() = %q, want \"\"", got)
			}
			// Every store built from it: the built-in roots, the definitions
			// file and kimi's sessions.
			for _, got := range []string{
				home(".claude", "projects"),
				DefinitionsPath(),
				kimiStore(),
			} {
				if got != "" {
					t.Errorf("store path %q, want \"\" for an unusable home", got)
				}
			}
		})
	}
}

// The absolute home is the one the stores are built from, so the guard above
// cannot reject a value every platform accepts.
func TestHomeDirAcceptsAbsoluteHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	if got := HomeDir(); got != home {
		t.Fatalf("HomeDir() = %q, want %q", got, home)
	}
}
