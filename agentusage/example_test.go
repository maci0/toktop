// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
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
