// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/agentusage"
)

func Example() {
	// Typical integration: load extra agent definitions, discover running
	// agents, and tail the one that is working in this directory.
	if err := agentusage.LoadDefinitions(agentusage.DefinitionsPath()); err != nil {
		fmt.Println(err) // malformed or unreadable; a missing file is not an error
	}
	// EnableOpenCodeDB reports whether this build can read opencode's store.
	// Call it before Watch; a false return is a build without -tags sqlite.

	if !agentusage.EnableOpenCodeDB(true) {
		fmt.Println("opencode: build without -tags sqlite")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range agentusage.Discover() {
		w := p.Watch(time.Now())
		if w.Err() != nil {
			continue // the agent keeps nothing this package can read
		}
		wg.Go(func() {
			// Run hands over a running total, so a caller that emits events
			// reports the growth from its previous sample. A delta reporting no
			// growth is the sample to measure from next time, whether the agent
			// was quiet or its transcript was rewritten under the watcher.
			var prev agentusage.Sample
			w.Run(ctx, agentusage.DefaultPollInterval, func(cur agentusage.Sample) {
				if d, ok := cur.Delta(prev); ok {
					fmt.Printf("%s pid %d: %d output, %d prompt\n", p.Tool, p.PID, d.Output, d.Input)
				}
				prev = cur
			})
		})
	}
	wg.Wait()
}

func ExampleRate() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Output: 100, At: t0}
	cur := agentusage.Sample{Output: 350, At: t0.Add(time.Second)}
	r, ok := agentusage.Rate(prev, cur)
	fmt.Println(int(r), ok)
	// Output: 250 true
}

// The method form takes the two samples the way a caller holds them, which is
// the same order Sample.Delta uses, so a Run callback measuring an interval
// cannot pass them the wrong way round.
func ExampleSample_RateFrom() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Output: 100, At: t0}
	cur := agentusage.Sample{Output: 350, At: t0.Add(time.Second)}
	if r, ok := cur.RateFrom(prev); ok {
		fmt.Println(int(r))
	}
	// Output: 250
}

// A transcript that records how long the model spent is reporting the
// interval the tokens were generated over, and the rate is that one rather
// than the wall gap between two readings. A turn whose counts arrive when it
// ends is the case: the gap since the previous turn also covers the tool
// calls the turn spent waiting, and dividing by it reports a generation rate
// for a run that never happened.
func ExampleRate_recordedSpan() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Output: 100, Span: 4 * time.Second, At: t0}
	cur := agentusage.Sample{Output: 700, Span: 14 * time.Second, At: t0.Add(time.Minute)}
	r, ok := agentusage.Rate(prev, cur)
	fmt.Println(int(r), ok)
	// Output: 60 true
}

func ExampleInputRate() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Input: 80, At: t0}
	cur := agentusage.Sample{Input: 200, At: t0.Add(time.Second)}
	r, ok := agentusage.InputRate(prev, cur)
	fmt.Println(int(r), ok)
	// Output: 120 true
}

// Reasoning is the share of Output an agent reports separately, and it rates
// the same way, so a caller showing a thinking rate has one call to make.
func ExampleThinkingRate() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Thinking: 20, At: t0}
	cur := agentusage.Sample{Thinking: 70, At: t0.Add(time.Second)}
	r, ok := agentusage.ThinkingRate(prev, cur)
	fmt.Println(int(r), ok)
	// Output: 50 true
}

func ExampleProcess_Watch() {
	for _, p := range agentusage.Discover() {
		if w := p.Watch(time.Now()); w.Err() == nil {
			fmt.Println(w.Tool(), w.Dir())
		}
	}
}

func ExampleLoadDefinitions() {
	err := agentusage.LoadDefinitions("/no/such/agents.json")
	fmt.Println(err)
	// Output: <nil>
}

// A definitions file this build cannot use is one error, told apart from the
// ordinary case of a machine with no definitions at all by errors.Is rather
// than by reading the message: a file that is missing is no error, and a
// malformed, unreadable or oversized one wraps ErrInvalidDefinitions, which
// covers two names colliding after normalization through
// ErrCollidingDefinitions. A colliding file leaves the registry as it was, so
// a program that reports the error keeps reading the agents it already had.
func ExampleLoadDefinitions_error() {
	dir, err := os.MkdirTemp("", "agentusage-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o644); err != nil {
		fmt.Println(err)
		return
	}
	err = agentusage.LoadDefinitions(path)
	fmt.Println(errors.Is(err, agentusage.ErrInvalidDefinitions))
	// The message names the file, with $HOME folded to "~" so the line can be
	// pasted into an issue as it stands.
	fmt.Println(strings.Contains(err.Error(), path))
	// Output:
	// true
	// true
}

// A test that loads a definitions file leaves the process-wide registry
// changed for every later test in the same binary, so it undoes the load.
func ExampleResetDefinitions() {
	dir, err := os.MkdirTemp("", "agentusage-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path,
		[]byte(`{"myagent": {"usage": {"roots": ["~/.myagent/sessions"]}}}`), 0o644); err != nil {
		fmt.Println(err)
		return
	}
	if err := agentusage.LoadDefinitions(path); err != nil {
		fmt.Println(err)
		return
	}
	_, loaded := agentusage.SpecFor("myagent")
	agentusage.ResetDefinitions()
	_, after := agentusage.SpecFor("myagent")
	fmt.Println(loaded, after)
	// Output: true false
}

func ExampleRegisterSpec() {
	err := agentusage.RegisterSpec("", agentusage.Spec{})
	fmt.Println(err)
	// Output: usage spec needs an agent name
}

// The pattern a consumer's own tests need: register an agent this package
// does not ship, point it at a transcript the test writes, and take the
// registration back out so the next test in the binary does not inherit it.
func ExampleRegisterSpec_fakeAgent() {
	dir, err := os.MkdirTemp("", "agentusage-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	if err := agentusage.RegisterSpec("fakeagent", agentusage.Spec{Roots: []string{dir}}); err != nil {
		fmt.Println(err)
		return
	}
	defer agentusage.UnregisterSpec("fakeagent")

	w := agentusage.Watch("fakeagent", "/home/me/project", time.Now())
	if w.Err() != nil {
		fmt.Println(w.Err())
		return
	}
	// A transcript already on disk when the watcher attached belongs to an
	// earlier run, so the record has to be written after it. The generic
	// reader takes counters under any of the names the supported agents use.
	record := []byte(`{"usage":{"input_tokens":30,"output_tokens":12}}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), record, 0o644); err != nil {
		fmt.Println(err)
		return
	}
	s := w.Poll()
	fmt.Println(s.Input, s.Output, s.Total)
	// Output: 30 12 42
}

func ExampleUnregisterSpec() {
	err := agentusage.RegisterSpec("myagent", agentusage.Spec{
		Roots: []string{"~/.myagent/sessions"},
	})
	fmt.Println(err, agentusage.Supported("myagent"))
	agentusage.UnregisterSpec("myagent")
	fmt.Println(agentusage.Supported("myagent"))
	// Output:
	// <nil> true
	// false
}

func ExampleWatcher_Err() {
	w := agentusage.Watch("nosuchagent", "", time.Now())
	fmt.Println(w == nil, errors.Is(w.Err(), agentusage.ErrUnsupportedTool))
	// Output: true true
}

func ExampleSpecFor() {
	// A definitions file says nothing about the entries it skipped, so a
	// program that wrote one asks the registry what actually landed. Roots come
	// back as written, and the name is canonicalized like every other lookup.
	spec, ok := agentusage.SpecFor(" pi ")
	fmt.Println(ok, spec.Roots)
	// Output: true [~/.pi/agent/sessions]
}

func ExampleSample_Empty() {
	fmt.Println(agentusage.Sample{}.Empty())
	fmt.Println(agentusage.Sample{Thinking: 12}.Empty())
	// Output:
	// true
	// false
}

func ExampleSample_Delta() {
	t0 := time.Unix(1_000_000, 0)
	prev := agentusage.Sample{Output: 100, Input: 80, At: t0}
	cur := agentusage.Sample{Output: 350, Input: 200, At: t0.Add(time.Second)}

	d, ok := cur.Delta(prev)
	fmt.Println(ok, d.Output, d.Input)
	if r, ok := agentusage.Rate(prev, cur); ok {
		fmt.Println(int(r))
	}
	// Output:
	// true 250 120
	// 250
}

// Run reports the running total; Delta is what changed since the last report,
// so a caller that emits events never bills the same tokens twice. A delta
// that reports no growth is the sample to measure from next time, whatever
// the reason: a quiet agent, or a transcript rewritten under the watcher.
func ExampleWatcher_Run() {
	w := agentusage.Watch("claude", "/home/me/project", time.Now())
	if w.Err() != nil {
		return // the agent keeps nothing this package can read
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var prev agentusage.Sample
	w.Run(ctx, agentusage.DefaultPollInterval, func(cur agentusage.Sample) {
		if d, ok := cur.Delta(prev); ok {
			fmt.Printf("%d output, %d prompt at %s\n", d.Output, d.Input, d.At.Format(time.TimeOnly))
		}
		prev = cur
	})
	// One final read after the agent has exited: the last records of a session
	// land once the process is gone, so Run's last callback is not the end of
	// the story.
	if d, ok := w.Poll().Delta(prev); ok {
		fmt.Printf("%d output, %d prompt at the end\n", d.Output, d.Input)
	}
}

func ExampleWatcher_SetNow() {
	w := agentusage.Watch("claude", "/home/me/project", time.Now())
	// A frozen clock stamps every published sample with one instant, so a
	// replayed run derives the same event ids from the same readings.
	base := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	w.SetNow(func() time.Time { return base })
}

// A test that replays a run rather than waiting it out puts the watcher's
// passes on a virtual timeline: the driver fires them, so the readings a
// callback sees are a function of the steps the test took and not of how long
// the transcripts took to grow. Paired with SetNow, which stamps what the
// watcher publishes, both halves of a run are then the driver's.
func ExampleWatcher_SetPacer() {
	dir, err := os.MkdirTemp("", "agentusage-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	if err := agentusage.RegisterSpec("replayed", agentusage.Spec{Roots: []string{dir}}); err != nil {
		fmt.Println(err)
		return
	}
	defer agentusage.UnregisterSpec("replayed")

	w := agentusage.Watch("replayed", "/home/me/project", time.Now())
	if w.Err() != nil {
		fmt.Println(w.Err())
		return
	}
	pace := agentusage.NewVirtualPacer()
	now := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	w.SetNow(func() time.Time { return now })
	w.SetPacer(pace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, agentusage.DefaultPollInterval, func(cur agentusage.Sample) {
			fmt.Println(cur.Output)
		})
	}()

	// One pass per step, with the transcript written in between, so every
	// callback sees the same readings whatever the wall clock did.
	path := filepath.Join(dir, "session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer f.Close()
	for step := range 3 {
		if _, err := fmt.Fprintf(f, `{"usage":{"output_tokens":%d}}`+"\n", 10*(step+1)); err != nil {
			fmt.Println(err)
			return
		}
		pace.Fire(now.Add(time.Duration(step) * agentusage.DefaultPollInterval))
	}
	cancel()
	<-done
}

// The engine-overlap check: endpoints an engine is advertised on, and the
// agents connected to any of them. A pid with no match is absent from the map.
func ExampleMatchingEndpoints() {
	engine, err := netip.ParseAddrPort("127.0.0.1:11434")
	if err != nil {
		return
	}
	for _, p := range agentusage.Discover() {
		// The map is keyed by pid, and holds the advertised endpoint a
		// process is connected to, not the peer's own spelling of it.
		for _, ep := range agentusage.MatchingEndpoints([]int{p.PID}, []netip.AddrPort{engine}) {
			fmt.Printf("%s pid %d already feeds %s\n", p.Tool, p.PID, ep)
		}
	}
}

func ExampleConnectedTo() {
	engine, err := netip.ParseAddrPort("[::1]:11434")
	if err != nil {
		return
	}
	for _, p := range agentusage.Discover() {
		// A false answer means "cannot tell", which reads as "not connected".
		if agentusage.ConnectedTo(p.PID, []netip.AddrPort{engine}) {
			fmt.Printf("%s pid %d is talking to the engine\n", p.Tool, p.PID)
		}
	}
}

func ExamplePeers() {
	for _, p := range agentusage.Discover() {
		for _, ep := range agentusage.Peers(p.PID) {
			fmt.Printf("%s pid %d is connected to %s\n", p.Tool, p.PID, ep)
		}
	}
}

func ExampleSupported() {
	for _, tool := range agentusage.Agents() {
		if agentusage.Supported(tool) {
			fmt.Println(tool)
		}
	}
}

// A program that writes agents.json marshals a Definitions value, so what it
// writes is the file LoadDefinitions reads rather than a hand-rolled copy of
// the format. An entry this package ignores is carried through untouched:
// definitions also describe how to launch an agent, which is not this
// package's business to keep or drop.
func ExampleDefinitions() {
	file := agentusage.Definitions{
		"myagent": {Usage: &agentusage.Spec{
			Roots: []string{"{dir}/.myagent/sessions"},
		}},
	}
	data, err := json.Marshal(file)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(data))
	// Output: {"myagent":{"usage":{"roots":["{dir}/.myagent/sessions"]}}}
}

// Editing a definitions file a program did not write is the case that costs:
// the file also describes how to launch each agent, and a rewrite that dropped
// those keys would delete an operator's configuration from their own file.
// Reading keeps them in Definition.Extra as the JSON the file spelled them
// with, so a rewrite changes the usage block and leaves the rest.
func ExampleDefinition_roundTrip() {
	const written = `{"myagent":{"launch":["myagent","--serve"],"usage":{"roots":["~/.myagent/sessions"]}}}`

	var file agentusage.Definitions
	if err := json.Unmarshal([]byte(written), &file); err != nil {
		fmt.Println(err)
		return
	}
	// The one change this program makes.
	file["myagent"].Usage.Roots = []string{"{dir}/.myagent/logs"}

	data, err := json.Marshal(file)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(data))
	// Output: {"myagent":{"launch":["myagent","--serve"],"usage":{"roots":["{dir}/.myagent/logs"]}}}
}

// A usage key this build has no field for is reported rather than refused: the
// file belongs to gauntlet, and a newer gauntlet can name a key an older
// build does not read yet. A misspelled key is the case worth reporting, since
// "root" leaves the agent with nothing to read and it then looks like an agent
// that never produces tokens. UsageKeyNames is the known set to name beside
// the offending key, so a consumer writing the message does not repeat the
// list and let it drift.
func ExampleUnknownUsageKeys() {
	dir, err := os.MkdirTemp("", "agentusage-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path,
		[]byte(`{"myagent": {"usage": {"roots": ["~/.myagent/sessions"], "sufixes": [".jsonl"]}}}`), 0o644); err != nil {
		fmt.Println(err)
		return
	}
	if err := agentusage.LoadDefinitions(path); err != nil {
		fmt.Println(err)
		return
	}
	defer agentusage.ResetDefinitions()

	for _, entry := range agentusage.UnknownUsageKeys() {
		agent, key, _ := strings.Cut(entry, ": ")
		fmt.Printf("%s: unknown usage key %q, known keys are %q\n",
			agent, key, agentusage.UsageKeyNames())
	}
	// The roots beside the typo still register, so the agent is readable and
	// the one misspelled key is a warning rather than a broken definition.
	_, registered := agentusage.SpecFor("myagent")
	fmt.Println("myagent registered:", registered)
	// Output:
	// myagent: unknown usage key "sufixes", known keys are ["cumulative" "header_cwd" "roots" "suffix" "suffixes"]
	// myagent registered: true
}

// A dashboard following several agent processes has to decide which of them it
// is already following, and key its map so one directory holds one entry. Both
// answers are per-platform questions this package settles, because two
// spellings of one directory differ byte for byte on macOS and Windows and name
// two directories on Linux: a caller spelling out filepath.Clean gets the
// first two platforms wrong. What holds on every platform is shown here; a
// caller wanting the macOS and Windows spellings of one directory asked for
// them, and they are one directory.
func ExampleSameDir() {
	fmt.Println(agentusage.SameDir("/home/me/project", "/home/me/project"))
	fmt.Println(agentusage.SameDir("/home/me/project", "/home/me/other"))
	// Output:
	// true
	// false
}

// DirKey is the same comparison as a map key, for a caller tracking one
// process per directory. Two spellings SameDir calls equal fold to one key, so
// the map holds a single entry rather than one per spelling, and two it calls
// different stay separate entries. The paths here are the same on every
// platform, since a platform that folds them together would be asserting
// something the example cannot also assert.
func ExampleDirKey() {
	followers := map[string]int{}
	for _, dir := range []string{"/home/me/project", "/home/me/project", "/home/me/other"} {
		followers[agentusage.DirKey(dir)]++
	}
	fmt.Println(len(followers))
	// Output: 2
}

// Agents lists every name this package knows, whether or not it can be read
// here. Supported is the separate question, and the two are asked together
// because a name alone is not a promise: the only built-ins nothing can read
// are the two database agents, and opencode needs this build to carry the
// sqlite tag on top of EnableOpenCodeDB.
func ExampleAgents() {
	fmt.Println(agentusage.Supported("claude"))
	fmt.Println(agentusage.Supported("no-such-agent"))
	// Output:
	// true
	// false
}
