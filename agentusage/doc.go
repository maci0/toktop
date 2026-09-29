// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package agentusage reports the token usage AI coding agents record on disk.
//
// Typical use: LoadDefinitions, Discover running agents, Watch each process
// (or Process.Watch), then Poll or Run for Sample values. A Sample is the
// total since the watcher attached, so a caller reporting events takes the
// growth from Sample.Delta. EnableOpenCodeDB opts into opencode's
// machine-wide SQLite store; crush is read whenever the sqlite build tag is
// on.
//
// An agent is read one of two ways. Transcript agents (agy, claude,
// clanker, codex, copilot, cursor-agent, dsh, gemini, grok, kimi, microagent,
// qwen) appear in the adapters table, each naming where its logs live under a
// working directory and how one line becomes a Sample.
// RegisterSpec adds one this package does not ship with, and LoadDefinitions
// reads the same declaration from a JSON file, DefinitionsPath being the
// default location. Definitions is that file as a Go value, so a program
// writing or editing one marshals it rather than hand-building the shape.
//
// Database agents are registered as sources instead: crush is built in,
// opencode is added by EnableOpenCodeDB because its store is machine-wide
// and the operator opts into it.
//
// Discover finds the agent processes running now, in pid order, and Watch reads the
// transcripts of the one working in a directory, so a caller can take a
// Sample on an interval without knowing which agent is underneath.
//
// Both of those, and Peers, need a process table to read: procfs on Linux,
// ps(1) on macOS. Every other platform reports nothing, which is "cannot
// tell" rather than an error, and a program that runs on more than one of
// them should treat an empty Discover as no local agents there. Watch is
// unaffected: it reads transcripts, and a caller that knows the agent name
// and its working directory gets usage on any platform.
//
// The agent registry is process-wide, since one process reports one set of
// agents. RegisterSpec and UnregisterSpec add and remove an adapter, and
// LoadDefinitions and ResetDefinitions do the same for a definitions file, so
// a program that teaches this package an agent can take it back out.
//
// The registry calls and Watch are safe to call from several goroutines at
// once, and a Watcher is safe to use while its Run is going: Poll takes the
// read lock a running Run also takes, so a final read after the agent exits
// is a call like any other rather than a race. A definitions file reloaded
// after a watcher started reaches it on the next poll, and a source
// withdrawn by EnableOpenCodeDB(false) stops being read the same way; an
// adapter installed by RegisterSpec is fixed for the life of the process, and
// a watcher keeps the one it attached with.
//
// Every method on a nil *Watcher is safe to call on the result of Watch:
// Tool and Dir report the empty string, Err matches ErrUnsupportedTool, Poll
// and Sample report the zero Sample, Run returns at once, and SetNow does
// nothing. A caller skipping the agents it cannot read therefore asks Err,
// which is what names that case, rather than testing the pointer:
//
//	if w := agentusage.Watch(tool, dir, time.Now()); w.Err() != nil {
//		// no readable usage for this agent
//	}
//
// The crush and opencode sources need a SQLite driver, so they exist only
// under the sqlite build tag. Without it the package still compiles, and
// Supported reports those agents unreadable.
//
// Three things a program needs sit beside that workflow rather than in it.
// MatchingEndpoints and ConnectedTo answer whether an agent's tokens are
// already being counted by an engine the program watches, matching a process's
// connections against the endpoints an engine is advertised on. They read the
// process table through Peers, and an unreadable one answers "not connected"
// rather than raising an error. SetLogger sends the lines this package audits
// (a transcript walk, read or database read that could not finish) to the
// logger the embedding program already writes to,
// defaulting to the one from log/slog. SameDir and DirKey answer whether two
// recorded paths name one directory and give that comparison a map key, which
// is a per-platform question this package settles rather than each caller
// spelling out: two spellings of one directory differ byte for byte on macOS
// and Windows and name two directories on Linux.
//
// Agents differ in what they print to stdout: some report token usage as they
// stream, some only at exit, some never. They agree on something else, though,
// which is that they keep a structured session transcript, and that transcript
// carries per-message usage with timestamps. Tailing it gives a live rate
// without root, without intercepting anyone's network traffic, and without
// asking the agent to behave differently.
//
// The design constraints that shape everything here:
//
//   - Only count usage after the watcher attached. Session transcripts persist
//     across runs, so the watcher records where each file ended when it
//     attached and reads only what is appended after that. Database-backed
//     agents (opencode, crush) use the since argument the same way; file
//     transcripts are always tailed from their attach-time end.
//   - Attribute the transcript to the right process. Each adapter ties its
//     files to a working directory: recorded per record, read from the
//     session header, read from a file beside the transcript (kimi, gemini,
//     agy, grok), or implicit because the log lives inside the project
//     directory itself (clanker), so the cwd is the key.
//   - Never invent a number. An agent whose transcript cannot be found, parsed,
//     or attributed simply reports nothing, and the dashboard shows no rate.
package agentusage
