package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/remote"
)

// Mode and environment validation, and the warnings for flags and env vars
// that were set but cannot take effect.

func warnIgnoredFlags(set map[string]bool, f *cliFlags, nAdd, nRemote int) {
	if set["opencode-db"] && !f.agents {
		fmt.Fprintln(os.Stderr, "toktop: --opencode-db has no effect without --agents")
	}
	if set["ingest"] && f.noIngest {
		fmt.Fprintln(os.Stderr, "toktop: --ingest has no effect with --no-ingest")
	}
	if set["seed"] && !f.demo {
		fmt.Fprintln(os.Stderr, "toktop: --seed has no effect without --demo")
	}
	if set["origin"] && !f.demo {
		fmt.Fprintln(os.Stderr, "toktop: --origin has no effect without --demo")
	}
	if set["frames"] && !f.once {
		fmt.Fprintln(os.Stderr, "toktop: --frames has no effect without --once")
	} else if set["frames"] && (f.plain || f.jsonOut) {
		// --plain and --json both render the last snapshot alone: a linear
		// list and a one-object report have no chart for the earlier frames
		// to fill, so the count only buys the wait before it. Gated on once so
		// a --frames --plain run with no --once is not also told how --once
		// would use it. --json gets the same line as --plain because it is
		// the same situation, and a flag that warns in one report and stays
		// silent in the other reads as a difference the reports do not have.
		report, what := "--plain", "the text report"
		if f.jsonOut {
			report, what = "--json", "the JSON report"
		}
		fmt.Fprintf(os.Stderr, "toktop: --frames only sets how long --once waits with %s; %s renders the last snapshot\n", report, what)
	}
	if set["plain"] && !f.once {
		fmt.Fprintln(os.Stderr, "toktop: --plain has no effect without --once")
	}
	if set["json"] && !f.once {
		fmt.Fprintln(os.Stderr, "toktop: --json has no effect without --once")
	}
	if set["json"] && f.plain {
		fmt.Fprintln(os.Stderr, "toktop: --plain has no effect with --json; the JSON report replaces the text report")
	}
	if set["no-hot-reload"] && f.once {
		fmt.Fprintln(os.Stderr, "toktop: --no-hot-reload has no effect with --once")
	}
	if f.demo && set["add"] {
		fmt.Fprintln(os.Stderr, "toktop: --add has no effect with --demo")
	}
	if f.demo && set["bearer"] {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect with --demo")
	}
	if set["bearer"] && !f.demo && nAdd == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect without --add")
	}
	if f.demo && set["ssh-key"] {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect with --demo")
	}
	if f.demo && nRemote > 0 {
		fmt.Fprintln(os.Stderr, "toktop: ssh:// targets have no effect with --demo")
	}
	if set["ssh-key"] && !f.demo && nRemote == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect without an ssh:// target")
	}
}

// warnIgnoredFrameEnv names TOKTOP_COLUMNS / TOKTOP_LINES when they are set
// but no sized frame is rendered: the overrides only size the --once
// dashboard frame, and a silently ignored variable looks like a broken knob,
// same as a flag passed into a mode that never reads it. The --plain and
// --json reports are unsized by construction, so they read neither, and the
// message names which of the two replaced the frame. Without --once there is
// no report at all, so that is the reason named first: --plain is itself
// already named as having no effect there.
func warnIgnoredFrameEnv(once, plain, jsonOut bool) {
	if once && !plain && !jsonOut {
		return
	}
	for _, name := range [...]string{"TOKTOP_COLUMNS", "TOKTOP_LINES"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			continue
		}
		if !once {
			fmt.Fprintf(os.Stderr, "toktop: $%s has no effect without --once\n", name)
			continue
		}
		// Both --plain and --json replace the sized frame with a report that
		// has no layout to size, and each says which one it was.
		if plain {
			fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --plain; the text report has no fixed frame size\n", name)
			continue
		}
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --json; the JSON report is not a sized frame\n", name)
	}
}

// warnIgnoredGauntletHome names a $GAUNTLET_HOME that is set but cannot
// deliver the agent definitions it points at. agentusage honors the variable
// only when absolute, so a relative value silently falls back to
// ~/.gauntlet; an absolute one resolves to $GAUNTLET_HOME/agents.json, and a
// missing file is not an error to LoadDefinitions, because the default
// ~/.gauntlet usually has no agents.json either. Both failures look the same
// from the dashboard: in-house agents simply never appear, which reads as
// agents producing no tokens. The value is named instead, in the two forms
// the operator can act on.
//
// Only --agents reads the file, so the warning fires there too: without it
// nothing consulted the variable.
func warnIgnoredGauntletHome(agents bool) {
	if !agents {
		return
	}
	v := os.Getenv("GAUNTLET_HOME")
	if v == "" {
		return
	}
	if !filepath.IsAbs(v) {
		fmt.Fprintf(os.Stderr, "toktop: $GAUNTLET_HOME must be an absolute path; ignoring %q and reading ~/.gauntlet/agents.json\n", v)
		return
	}
	path := agentusage.DefinitionsPath()
	if _, err := os.Stat(path); err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: $GAUNTLET_HOME points at no agent definitions (%s is missing); no in-house agents are watched\n",
		core.RedactHome(path))
}

// warnIgnoredXDGHome names an $XDG_DATA_HOME or $XDG_CONFIG_HOME that is set
// but not a path DefinitionsPath-style consumers can use. Both readers honor
// the variable only when it is absolute, so a relative value silently falls
// back to the default directory: opencode's session database is read from
// ~/.local/share (agents that generated tokens report none) and the ssh
// trust-on-first-use store from ~/.config. Nothing else would report it.
//
// Each variable is named only where it would have been read: XDG_DATA_HOME
// with the opencode database open (f.agents && --opencode-db, resolved),
// XDG_CONFIG_HOME with an ssh:// target to connect to. Without one, the
// variable cannot take effect, and the rule GAUNTLET_HOME follows above
// names only a --agents run for the same reason.
func warnIgnoredXDGHome(opencodeDB, sshTargets bool) {
	for _, e := range [...]struct {
		name string
		read bool
	}{
		{"XDG_DATA_HOME", opencodeDB},
		{"XDG_CONFIG_HOME", sshTargets},
	} {
		if !e.read {
			continue
		}
		if v := os.Getenv(e.name); v != "" && !filepath.IsAbs(v) {
			fmt.Fprintf(os.Stderr, "toktop: $%s must be an absolute path; ignoring %q and reading the default directory\n", e.name, v)
		}
	}
}

// warnUnusedEnv names secret and log-level variables that are set but will
// not be read in this mode, matching warnIgnoredFlags for the flag form.
func warnUnusedEnv(bearerFlag, demo, noIngest, agents bool, nAdd, nRemote int) {
	if demo || nRemote == 0 {
		if os.Getenv(remote.PasswordEnv) != "" {
			if demo {
				fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --demo\n", remote.PasswordEnv)
			} else {
				fmt.Fprintf(os.Stderr, "toktop: $%s has no effect without an ssh:// target\n", remote.PasswordEnv)
			}
		}
	}
	if !bearerFlag && (demo || nAdd == 0) {
		reason := "without --add"
		if demo {
			reason = "with --demo"
		}
		for _, name := range [...]string{"OMNIROUTE_API_KEY", "TOKTOP_BEARER"} {
			if os.Getenv(name) != "" {
				fmt.Fprintf(os.Stderr, "toktop: $%s has no effect %s\n", name, reason)
			}
		}
	}
	// The audit logger is not the ingest endpoint's: the collector, the ssh
	// client, the agent watch and the attach path all build one from the same
	// variable, so --no-ingest alone leaves the level in force. Only a demo run,
	// which measures no engine and has no ssh target, writes no engine, ssh or
	// ingest audit record once the endpoint and the agent watch are both off.
	// The startup config line and record are still written, at the level this
	// variable sets.
	// Trimmed, so a variable set to whitespace reads as the unset default it
	// resolves to, the way ParseLogLevel, logActiveConfig and warnIgnoredFrameEnv
	// all read it. Naming a blank $TOKTOP_LOG_LEVEL as a knob in force would be
	// the same false claim the config line avoids.
	if demo && noIngest && !agents && strings.TrimSpace(os.Getenv(logcfg.LevelEnv)) != "" {
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --demo --no-ingest; no engine, ssh or ingest audit log is written in that run\n", logcfg.LevelEnv)
	}
}

// probeSecsMax caps auto-probe scheduling at 24h, well below the point where
// time.Duration(n)*time.Second overflows and NewTicker would panic.
const probeSecsMax = 24 * 60 * 60

// Poll interval bounds apply after flag.Duration parses a unit-bearing value
// (or zero). The 50ms floor prevents excessive polling; a provider request is
// bounded by provider.PollTimeout (1.5s). The --once frame wait is three
// times the interval, so this ceiling bounds it too.
const (
	intervalMin = 50 * time.Millisecond
	intervalMax = time.Hour
)

// validateFlags rejects out-of-range values at startup: a running dashboard
// that ignores what it was asked to do is a misconfiguration nobody can see.
func validateFlags(once bool, interval time.Duration, probeSecs, frames int) error {
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", interval)
	}
	// A value under a millisecond is one written without a usable unit. The
	// flag package rejects a bare number at Parse (missingUnitHint names the
	// unit there), so what lands here is a spelled-out unit that is simply too
	// small; the parenthetical states the reading rule, not a claim about this
	// particular value.
	if interval < intervalMin {
		return fmt.Errorf("--interval must be >= %s, got %s (a bare number reads as nanoseconds; use 1s or 500ms)", intervalMin, interval)
	}
	if interval > intervalMax {
		return fmt.Errorf("--interval must be <= 1h, got %s", interval)
	}
	if probeSecs < 0 {
		return fmt.Errorf("--probe must be >= 0 (0 disables auto-probe), got %d", probeSecs)
	}
	if probeSecs > probeSecsMax {
		return fmt.Errorf("--probe must be <= %d (seconds), got %d", probeSecsMax, probeSecs)
	}
	if once && frames < 1 {
		return fmt.Errorf("--frames must be >= 1, got %d", frames)
	}
	if once && frames > core.HistoryLen {
		return fmt.Errorf("--frames must be <= %d (chart history length), got %d", core.HistoryLen, frames)
	}
	return nil
}

// parseOrigin turns --origin into the instant the demo timeline starts at.
// RFC3339 and bare Unix seconds are both accepted, so a replay can be written
// either way; an empty value is the wall clock at launch (the source's own
// default), reported as a zero time so the caller pins nothing.
//
// Rejection is loud: a mistyped instant would otherwise leave the run on the
// wall clock, and the operator replaying a captured frame would get a second
// run that differs only in timestamps, which is exactly the difference they
// pinned the origin to remove.
func parseOrigin(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--origin must be an RFC3339 instant or Unix seconds, got %q", s)
	}
	return at, nil
}

// validateSSHKeyFlag rejects an explicitly empty --ssh-key. The flag names a
// file, so an empty value can only be a mistake, and it is indistinguishable
// from the flag never having been given: the run would fall back to
// ~/.ssh/config and authenticate as a key the operator did not choose.
// resolveBearer already makes an explicit empty --bearer override the
// environment rather than mean "unset"; this is the same distinction, closed
// by rejecting the value instead.
func validateSSHKeyFlag(set bool, val string) error {
	if set && strings.TrimSpace(val) == "" {
		return errors.New("--ssh-key must be a path, not empty (omit the flag to use ~/.ssh/config)")
	}
	return nil
}

// validateLogLevelEnv rejects a set-but-unknown TOKTOP_LOG_LEVEL before the
// ingest logger is built. Empty means the info default.
func validateLogLevelEnv() error {
	_, err := logcfg.ParseLogLevel(os.Getenv(logcfg.LevelEnv))
	return err
}

// validateIngestAddr rejects listen addresses that are empty or not host:port
// before net.Listen sees them. An empty string is equivalent to ":0" (every
// interface, ephemeral port), which would silently expose the unauthenticated
// ingest endpoint; a missing port is a common typo for "use the default".
func validateIngestAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("--ingest address must be host:port, not empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--ingest address must be host:port, got %q", addr)
	}
	n, convErr := strconv.Atoi(port)
	if convErr != nil || n < 0 || n > 65535 {
		return fmt.Errorf("--ingest port must be 0-65535, got %q", port)
	}
	return nil
}

// resolveBearer returns the token sent to --add endpoints. An explicit
// --bearer (including empty) wins so clearing the token does not fall through
// to the environment; otherwise OMNIROUTE_API_KEY, then TOKTOP_BEARER.
func resolveBearer(flagVal string, flagSet bool) string {
	if flagSet {
		return flagVal
	}
	if v := os.Getenv("OMNIROUTE_API_KEY"); v != "" {
		return v
	}
	return os.Getenv("TOKTOP_BEARER")
}

// warnBearerFlag names the two --bearer footguns: a token on argv is
// readable from process listings, and an explicit empty value suppresses
// the env fallbacks rather than meaning "unset".
func warnBearerFlag(flagSet bool, flagVal string) {
	if !flagSet {
		return
	}
	if flagVal != "" {
		fmt.Fprintln(os.Stderr, "toktop: --bearer is visible in process listings; prefer $TOKTOP_BEARER or $OMNIROUTE_API_KEY")
		return
	}
	if os.Getenv("OMNIROUTE_API_KEY") != "" || os.Getenv("TOKTOP_BEARER") != "" {
		fmt.Fprintln(os.Stderr, "toktop: empty --bearer overrides $OMNIROUTE_API_KEY / $TOKTOP_BEARER")
	}
}

// Frame floors and caps for TOKTOP_COLUMNS / TOKTOP_LINES. Below the floors
// the static frame cannot lay out legibly. Above the caps, composeFrame would
// allocate a pane of newlines/cells big enough to OOM a capture from a typo
// (TOKTOP_LINES=1000000000). 1024x512 is larger than any real terminal.
const (
	frameColumnsMin = 41
	frameLinesMin   = 21
	frameColumnsMax = 1024
	frameLinesMax   = 512
)

// frameEnv reads one TOKTOP_COLUMNS / TOKTOP_LINES override. Unset or empty
// means default (set is false). Surrounding whitespace is ignored so a value
// copied with a trailing newline still parses, matching TOKTOP_LOG_LEVEL.
func frameEnv(name string, least, most int) (n int, set bool, err error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false, nil
	}
	n, convErr := strconv.Atoi(v)
	if convErr != nil || n < least || n > most {
		return 0, false, fmt.Errorf("$%s must be an integer %d-%d, got %q", name, least, most, v)
	}
	return n, true, nil
}

// validateOnceEnv rejects a set-but-unusable frame override before --once
// renders: a capture sized by a typo'd variable must fail loudly rather than
// come out at the fallback size with nothing explaining why. Unset or empty
// means default, matching how every other optional setting reads here.
func validateOnceEnv() error {
	for _, e := range [...]struct {
		name        string
		least, most int
	}{
		{"TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax},
		{"TOKTOP_LINES", frameLinesMin, frameLinesMax},
	} {
		if _, _, err := frameEnv(e.name, e.least, e.most); err != nil {
			return err
		}
	}
	return nil
}
