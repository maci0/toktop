// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/logcfg"
)

// The startup config record: the knobs a run applies, rendered as a prose
// line on stderr and as one record in the audit log. Both come from one
// derivation in activeConfig, so the two cannot name different things.

// configLog is where the startup config record goes. A var so a test can read
// it, the same reason attach.go has attachLog.
var configLog = logcfg.Logger

// configFlag is one knob of the startup line: the name the audit record gives
// it, the value it takes, and whether the stderr line prints it bare. A knob
// not in force is absent from the list, so the prose line and the audit record
// are rendered from one derivation and cannot name different things.
type configFlag struct {
	key   string
	value string
	bare  bool
}

// activeConfig resolves the knobs that will actually apply. Secrets are named
// as set/unset, never carried. opencodeOn is the resolved gate, not the flag: a
// build without the sqlite driver reads no opencode database however the flag
// is set.
func activeConfig(f *cliFlags, explicit map[string]bool, nAdd, nRemote int, opencodeOn bool) []configFlag {
	cfg := []configFlag{{key: "interval", value: f.interval.String()}}
	if f.noIngest {
		cfg = append(cfg, configFlag{key: "ingest", value: "off"})
	} else {
		cfg = append(cfg, configFlag{key: "ingest", value: f.ingest})
	}
	if lvl := strings.TrimSpace(os.Getenv(logcfg.LevelEnv)); lvl != "" {
		// The resolved level, not the raw string: "warning" and "WARN" print
		// as warn, so the line matches what the audit log actually applies.
		// main rejects an unparseable value, so the error case is unreachable.
		if parsed, err := logcfg.ParseLogLevel(lvl); err == nil {
			cfg = append(cfg, configFlag{key: "log", value: logcfg.LogLevelName(parsed)})
		}
	}
	if f.demo {
		cfg = append(cfg, configFlag{key: "demo", bare: true})
		// The two inputs a demo run replays from, on the startup line and in
		// the audit log. A run that ends in a crash leaves the log behind and
		// nothing else: the report that carries the seed is never written, so
		// without these the run that produced the lines around them cannot be
		// reproduced from what survived it. The origin is recorded as the flag
		// value the operator gave, so the replay is a copy of the command
		// rather than a re-derivation of it.
		cfg = append(cfg, configFlag{key: "seed", value: strconv.FormatInt(f.seed, 10)})
		if f.origin != "" {
			cfg = append(cfg, configFlag{key: "origin", value: f.origin})
		}
	}
	if f.agents {
		cfg = append(cfg, configFlag{key: "agents", bare: true})
		if opencodeOn {
			cfg = append(cfg, configFlag{key: "opencode-db", bare: true})
		}
	}
	if f.once {
		cfg = append(cfg, configFlag{key: "once", bare: true})
		// Only the report that is actually rendered is named: --json
		// replaces the text report, so a line reading "once plain json"
		// claims a knob is in force that the run ignored.
		if f.jsonOut {
			cfg = append(cfg, configFlag{key: "json", bare: true})
		}
		// The sized frame is the only --once output the two reports above do
		// not replace, so it is the only one an override can reach. Reported
		// under the short key rather than the variable name because a capture
		// run is reproduced from this line: it names the frame that was
		// rendered, where the render itself is a bitmap with nothing in it to
		// say how large it was meant to be. An override that cannot take
		// effect is absent, the same rule plain and json follow.
		if !f.plain && !f.jsonOut {
			for _, e := range frameEnvVars {
				if v, set, err := frameEnv(e.name, e.least, e.most); err == nil && set {
					cfg = append(cfg, configFlag{key: e.key, value: strconv.Itoa(v)})
				}
			}
		}
	}
	// --plain on its own is the live text report, so it is a knob in force
	// with or without --once: warnIgnoredFlags says the flag stays live and
	// keeps the keys, and only --json replaces it. A run that rendered the
	// linear report with nothing on the line saying so left the record of
	// which report it was carrying to the shape of the next few lines.
	if f.plain && !f.jsonOut {
		cfg = append(cfg, configFlag{key: "plain", bare: true})
	}
	if f.probeSecs > 0 {
		cfg = append(cfg, configFlag{key: "probe", value: fmt.Sprintf("%ds", f.probeSecs)})
	}
	if nRemote > 0 && !f.demo {
		cfg = append(cfg, configFlag{key: "ssh", value: strconv.Itoa(nRemote)})
	}
	if nAdd > 0 && !f.demo {
		if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
			// set or refused, never bare "set": bearer.Set runs after this
			// line and turns down a token carrying CR or LF, leaving the run
			// to query the --add endpoints unauthenticated. A record reading
			// bearer=set there describes a credential that is not in force,
			// and the 401s it explains arrive with nothing naming the cause.
			state := "set"
			if !bearer.Usable(tok) {
				state = "refused"
			}
			cfg = append(cfg, configFlag{key: "bearer", value: state})
		}
	}
	return cfg
}

// logActiveConfig writes the knobs of the run twice: as the startup line on w,
// and as one record in the audit log. The live dashboard hides stderr under the
// alt screen, and every other subsystem (the collector, the ssh client, the
// agent watch, the ingest endpoint) audits there, so a run whose config never
// reaches the audit log is a run nobody can reconstruct from it: the record
// says which interval, which endpoints and which log floor produced the lines
// around it.
func logActiveConfig(w io.Writer, f *cliFlags, explicit map[string]bool, nAdd, nRemote int, opencodeOn bool) {
	cfg := activeConfig(f, explicit, nAdd, nRemote, opencodeOn)
	var b strings.Builder
	b.WriteString("toktop:")
	attrs := make([]any, 0, len(cfg))
	for _, c := range cfg {
		if c.bare {
			fmt.Fprintf(&b, " %s", c.key)
			attrs = append(attrs, c.key, true)
			continue
		}
		// One fold for both copies, so the line on stderr and the record in
		// the audit log cannot disagree about what the knob was set to.
		v := operatorText(c.value)
		fmt.Fprintf(&b, " %s=%s", c.key, v)
		attrs = append(attrs, c.key, v)
	}
	fmt.Fprintln(w, b.String())
	configLog().Info("toktop: config", attrs...)
}
