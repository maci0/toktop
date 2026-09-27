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
// An agent is read one of two ways. Transcript agents (claude, qwen, dsh,
// clanker, copilot, codex, kimi) appear in the adapters table, each naming
// where its logs live under a working directory and how one line becomes a
// Sample.
// RegisterSpec adds one this package does not ship with, and LoadDefinitions
// reads the same declaration from a JSON file, DefinitionsPath being the
// default location.
//
// Database agents are registered as sources instead: crush is built in,
// opencode is added by EnableOpenCodeDB because its store is machine-wide
// and the operator opts into it.
//
// Discover finds the agent processes running now, and Watch reads the
// transcripts of the one working in a directory, so a caller can take a
// Sample on an interval without knowing which agent is underneath.
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
//     session header, or implicit because the log lives inside the project
//     directory itself (clanker), so the cwd is the key.
//   - Never invent a number. An agent whose transcript cannot be found, parsed,
//     or attributed simply reports nothing, and the dashboard shows no rate.
package agentusage
