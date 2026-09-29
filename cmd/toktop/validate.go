package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/remote"
)

// Mode and environment validation, the resolution of the inputs that come from
// the environment, and the warnings for flags and env vars that were set but
// cannot take effect.

// operatorText prepares a value the operator supplied for a startup line on
// stderr. The audit record already runs every one of these through
// logcfg.Field, which folds control characters and the bidi overrides that
// would reorder the line; the terminal copy did not, so the same string was
// safe in the log and live in the terminal, and a flag or env var carrying an
// escape sequence repainted the operator's screen (or named an endpoint with
// glyphs it does not have). Same fold, so the line an operator reads and the
// record they paste into an issue are the same text.
func operatorText(s string) string { return logcfg.Field(s, 256) }

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
	if set["json"] && !f.once {
		fmt.Fprintln(os.Stderr, "toktop: --json has no effect without --once")
	}
	if set["json"] && f.jsonOut && f.plain {
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
	for _, e := range frameEnvVars {
		if strings.TrimSpace(os.Getenv(e.name)) == "" {
			continue
		}
		if !once {
			fmt.Fprintf(os.Stderr, "toktop: $%s has no effect without --once\n", e.name)
			continue
		}
		// Both --plain and --json replace the sized frame with a report that
		// has no layout to size, and each says which one it was.
		if plain {
			fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --plain; the text report has no fixed frame size\n", e.name)
			continue
		}
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --json; the JSON report is not a sized frame\n", e.name)
	}
}

// warnIgnoredGauntletHome names an $GAUNTLET_HOME that is set but cannot
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
	v := os.Getenv(agentusage.GauntletHomeEnv)
	if v == "" {
		return
	}
	if !filepath.IsAbs(v) {
		fmt.Fprintf(os.Stderr, "toktop: $%s must be an absolute path; ignoring %q and reading ~/.gauntlet/agents.json\n",
			agentusage.GauntletHomeEnv, core.RedactHome(v))
		return
	}
	path := agentusage.DefinitionsPath()
	if _, err := os.Stat(path); err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: $%s points at no agent definitions (%s is missing); no in-house agents are watched\n",
		agentusage.GauntletHomeEnv, core.RedactHome(path))
}

// warnIgnoredUserHome names a home directory the built-in agent stores cannot
// be built from. Every store this binary knows is a path under it
// (~/.claude/projects and its siblings), and the rule the XDG and GAUNTLET
// variables are held to is that an unusable value names no path rather than a
// path under the directory the run started in, where a missing store is an
// empty one and the agent reports no tokens.
//
// A home that is unset or not absolute stops the run before this is reached:
// loadAgentDefs cannot place agents.json without one, and refuses to start.
// The one run that gets past it is a GAUNTLET_HOME naming an absolute
// directory, where the definitions file is read and the built-in stores are
// not, so that is the only case this has to say anything about.
func warnIgnoredUserHome() {
	if agentusage.HomeDir() != "" {
		return
	}
	if _, err := os.UserHomeDir(); err == nil {
		// The value is not quoted: a home is either an account name or a path
		// under one, and this line is read over a stranger's shoulder and
		// pasted into issues. Naming the directory is what tells the operator
		// the variable to change; the account it holds is not theirs to
		// publish.
		fmt.Fprintln(os.Stderr, "toktop: the home directory is not an absolute path; no built-in agent store is read")
		return
	}
	fmt.Fprintln(os.Stderr, "toktop: the home directory cannot be located; no built-in agent store is read")
}

// warnIgnoredXDGHome names an $XDG_DATA_HOME, $XDG_CONFIG_HOME or
// $KIMI_CODE_HOME that is set but not a path DefinitionsPath-style consumers
// can use. Every reader honors its variable only when it is absolute, so a
// relative value silently falls back to the default directory: opencode's
// session database is read from ~/.local/share (agents that generated tokens
// report none), the ssh trust-on-first-use store from ~/.config, and kimi's
// session logs from ~/.kimi-code/sessions. Nothing else would report it.
//
// Each variable is named only where it would have been read: XDG_DATA_HOME
// with the opencode database open (f.agents && --opencode-db, resolved),
// XDG_CONFIG_HOME with an ssh:// target to connect to, KIMI_CODE_HOME with
// --agents. Without one, the variable cannot take effect, and the rule
// GAUNTLET_HOME follows above names only a --agents run for the same reason.
//
// A relative value is named with the consequence that follows, which is not
// the same for all three: the opencode database and the kimi store have a
// home directory to fall back to, and the ssh host-key store resolves through
// os.UserConfigDir, which refuses a relative XDG_CONFIG_HOME outright on
// Linux. There the run does not read a default directory, it names no store
// and every connect fails, so the warning says so rather than sending the
// operator after a fallback that does not exist.
//
// An absolute KIMI_CODE_HOME is checked for the store under it, the way
// GAUNTLET_HOME is checked for the file beside it. kimi creates its sessions
// directory itself on first run, so a variable the operator set naming no
// sessions directory is a home kimi does not use: every session reads as an
// agent producing no tokens, which is the failure the relative case above
// already names. The other two are not checked, because nothing is wrong with
// a variable naming a directory the reader creates: the ssh host-key store is
// written on first contact, and most machines running this have no opencode at
// all.
// Each entry names the reader that decides the consequence, rather than
// branching on the variable's spelling: a table keyed by name sends the two
// special cases to whichever row happens to carry the string, and a rename
// there would silently downgrade both to the generic line.
func warnIgnoredXDGHome(opencodeDB, sshTargets, agents bool) {
	for _, e := range [...]struct {
		name string
		read bool
		// noStore is the warning for a relative value that leaves the reader
		// with no path at all, rather than a fallback. It reports whether it
		// printed, so the generic line and this one never both fire for one
		// variable. Nil for the readers that have a default to fall back to.
		noStore func() bool
	}{
		{name: agentusage.XDGDataHomeEnv, read: opencodeDB},
		{name: remote.XDGConfigHomeEnv, read: sshTargets, noStore: warnNoHostKeyStore},
		{name: agentusage.KimiHomeEnv, read: agents},
	} {
		if !e.read {
			continue
		}
		v := os.Getenv(e.name)
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) {
			fmt.Fprintf(os.Stderr, "toktop: $%s must be an absolute path; ignoring %q\n", e.name, core.RedactHome(v))
			// The consequence differs per variable, and naming the wrong one
			// sends the operator after a fallback that does not exist. A store
			// with a default to fall back to says so; the ssh host-key store
			// resolves through os.UserConfigDir, which refuses a relative
			// XDG_CONFIG_HOME on Linux and so names no store at all, failing
			// every connect. Asked of the package that resolves it, so the
			// warning and the connect cannot disagree.
			if e.noStore != nil && e.noStore() {
				continue
			}
			fmt.Fprintln(os.Stderr, "toktop: reading the default directory instead")
			continue
		}
		if e.name == agentusage.KimiHomeEnv {
			warnMissingKimiStore()
		}
	}
}

// warnNoHostKeyStore reports whether a relative $XDG_CONFIG_HOME leaves the ssh
// host-key store with no path on this platform, printing the line naming that
// consequence when it does. The caller prints the generic fallback line when it
// returns false, so the two never both fire for one variable.
func warnNoHostKeyStore() bool {
	if remote.HostKeyStorePath() != "" {
		return false
	}
	fmt.Fprintf(os.Stderr, "toktop: $%s names no host-key store on this platform; every ssh:// target will fail to connect\n",
		remote.XDGConfigHomeEnv)
	return true
}

// warnMissingKimiStore names an absolute $KIMI_CODE_HOME with no sessions
// directory under it. kimiRoots returns nothing for a store that cannot be
// listed, and no watcher is built, so the dashboard shows no kimi agent at all
// and reads as one that used no tokens. The path comes from the package rather
// than the caller, which is what KimiStorePath is exported for, so the
// warning and the reader resolve $KIMI_CODE_HOME the same way.
func warnMissingKimiStore() {
	store := agentusage.KimiStorePath()
	if info, err := os.Stat(store); err == nil && info.IsDir() {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: $%s points at no kimi sessions (%s is missing); no kimi agent is watched\n",
		agentusage.KimiHomeEnv, core.RedactHome(store))
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
		for _, name := range bearerEnvVars {
			if os.Getenv(name) != "" {
				fmt.Fprintf(os.Stderr, "toktop: $%s has no effect %s\n", name, reason)
			}
		}
	}
	// The audit logger is not the ingest endpoint's: the collector, the ssh
	// client, the agent watch and the attach path all build one from the same
	// variable, so --no-ingest alone leaves the level in force. A demo run
	// with the endpoint and the agent watch both off writes no engine, ssh or
	// ingest record, but the startup config line and its audit record are
	// still written, at the level this variable sets.
	// Trimmed, so a variable set to whitespace reads as the unset default it
	// resolves to, the way ParseLogLevel, logActiveConfig and warnIgnoredFrameEnv
	// all read it. Naming a blank $TOKTOP_LOG_LEVEL as a knob in force would be
	// the same false claim the config line avoids.
	if demo && noIngest && !agents && strings.TrimSpace(os.Getenv(logcfg.LevelEnv)) != "" {
		fmt.Fprintf(os.Stderr, "toktop: $%s with --demo --no-ingest sets only the startup config record; no engine, ssh or ingest audit log is written in that run\n", logcfg.LevelEnv)
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
	// flag package rejects a bare number at Parse (valueHint names the
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

// dateDigits, dateTimeDigits and dateSecDigits are the digit counts a date or
// a datetime is written in: YYYYMMDD, YYYYMMDDHHMM and YYYYMMDDHHMMSS. A bare
// --origin of one of those widths is a date the operator mistyped for a Unix
// second, and reading it as one would pin the run to an instant in 1970 that
// the operator never meant.
const (
	dateDigits     = 8  // 20260928
	dateTimeDigits = 12 // 20260928T1337
	dateSecDigits  = 14 // 20260928T133700
)

// parseOrigin turns --origin into the instant the demo timeline starts at.
// RFC3339 and bare Unix seconds are both accepted, so a replay can be written
// either way; an empty value is the wall clock at launch (the source's own
// default), reported as a zero time so the caller pins nothing.
//
// Rejection is loud: a mistyped instant would otherwise leave the run on the
// wall clock, and the operator replaying a captured frame would get a second
// run that differs only in timestamps, which is exactly the difference they
// pinned the origin to remove. The bare-integer branch is therefore bounded by
// width alone: every Unix second the flag documents is accepted, from 0 to the
// 19-digit forms, and only a digit string shaped like a date is refused.
func parseOrigin(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if secs, ok := bareUnixSeconds(s); ok {
		return time.Unix(secs, 0).UTC(), nil
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--origin must be an RFC3339 instant or Unix seconds, got %q", s)
	}
	return at, nil
}

// bareUnixSeconds reads s as a Unix second, or reports that it is not one. A
// leading minus is a pre-epoch second and has no date spelling to be confused
// with, so the width bound is on the unsigned form only.
func bareUnixSeconds(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	if s[0] == '-' {
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
			return secs, true
		}
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	if len(s) == dateDigits || len(s) == dateTimeDigits || len(s) == dateSecDigits {
		return 0, false
	}
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return secs, true
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
	if err := parsePort("--ingest", port); err != nil {
		return err
	}
	return nil
}

// portMax is the highest port a target or an ingest listener may name.
const portMax = 65535

// parsePort validates a textual port and reports it under the flag or option
// the caller carries it for. Zero is accepted: it is the ephemeral port a
// listener binds when the operator leaves the choice open.
func parsePort(label, port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > portMax {
		return fmt.Errorf("%s port must be 0-%d, got %q", label, portMax, port)
	}
	return nil
}

// bearerEnvVars are the environment variables a bearer token is read from, in
// precedence order.
var bearerEnvVars = [...]string{"OMNIROUTE_API_KEY", "TOKTOP_BEARER"}

// resolveBearer returns the token sent to --add endpoints, empty when none is
// set. An explicit --bearer (including empty) wins so clearing the token does
// not fall through to the environment; otherwise bearerEnvVars in order.
//
// Every source is trimmed of surrounding whitespace, which bearer.Set trims
// anyway, so the token is not left with the line ending
// `export TOKTOP_BEARER=$(cat key)` leaves behind. Untrimmed, a variable
// holding only that newline is a non-empty string that wins the precedence
// chain and then authenticates nothing: every --add endpoint answers 401 while
// the startup line reads bearer=set. A value that is whitespace is a wrapper
// that failed to read the file, not a token.
func resolveBearer(flagVal string, flagSet bool) string {
	if flagSet {
		return strings.TrimSpace(flagVal)
	}
	for _, name := range bearerEnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// warnBlankBearer names a bearer variable that is set but carries no token,
// where a --add endpoint is attached. The generic message is a 401 per poll
// and names nothing about the cause, and the startup line still reports
// bearer=set because the source was set, so an operator reading either is told
// the run is authenticated. Blank values are read in precedence order and do
// not win it: the token falls through to the next source, which is the right
// resolution, but the variable that was set to nothing is still the mistake.
//
// inForce says whether a token is actually installed, so the two cases read
// differently. A blank variable that another source covered changes nothing
// about the run, and telling the operator the endpoints are queried without a
// token sends them after an authentication problem this run does not have. A
// blank variable that left the run with no token is the mistake, and the 401s
// it explains arrive with nothing naming the cause.
func warnBlankBearer(nAdd int, demo, inForce bool) {
	if demo || nAdd == 0 {
		return
	}
	for _, name := range bearerEnvVars {
		v, set := os.LookupEnv(name)
		if !set || strings.TrimSpace(v) != "" {
			continue
		}
		if inForce {
			fmt.Fprintf(os.Stderr, "toktop: $%s is set but blank; it is ignored, and the token in force comes from another source\n", name)
			continue
		}
		fmt.Fprintf(os.Stderr, "toktop: $%s is set but blank; the --add endpoints are queried without a token\n", name)
	}
}

// warnBearerFlag names the two --bearer footguns: a token on argv is
// readable from process listings, and an explicit empty value suppresses
// the env fallbacks rather than meaning "unset".
//
// The fallbacks are named from bearerEnvVars and read the way resolveBearer
// reads them, so the list and the presence test cannot disagree with the
// precedence chain. A name spelled here that the chain does not consult
// reports an override that does not happen, and a whitespace-only value read
// untrimmed reports one that does not happen either, since resolveBearer
// skips it.
func warnBearerFlag(flagSet bool, flagVal string) {
	if !flagSet {
		return
	}
	names := "$" + strings.Join(bearerEnvVars[:], " / $")
	if flagVal != "" {
		fmt.Fprintf(os.Stderr, "toktop: --bearer is visible in process listings; prefer %s\n", names)
		return
	}
	for _, name := range bearerEnvVars {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "toktop: empty --bearer overrides %s\n", names)
		return
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

// frameEnvVars are the TOKTOP_COLUMNS / TOKTOP_LINES overrides, each with the
// bounds it is validated against, the key the startup line reports it under,
// and the frame dimension it sizes. One table so the range check, the
// unused-variable warning, the startup line and the size a --once frame is
// rendered with cannot name a different pair, a different bound, or a
// different dimension.
var frameEnvVars = [...]struct {
	name        string
	key         string
	width       bool
	least, most int
}{
	{name: "TOKTOP_COLUMNS", key: "columns", width: true, least: frameColumnsMin, most: frameColumnsMax},
	{name: "TOKTOP_LINES", key: "lines", least: frameLinesMin, most: frameLinesMax},
}

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
	for _, e := range frameEnvVars {
		if _, _, err := frameEnv(e.name, e.least, e.most); err != nil {
			return err
		}
	}
	return nil
}

// screenshotFontEnv is used only by scripts/screenshot.py. Listed here so a
// developer export of it is not reported as a typo by warnUnknownEnv.
const screenshotFontEnv = "TOKTOP_SCREENSHOT_FONT"

// toktopEnvVars are the TOKTOP_* names this process recognizes. Derived from
// the tables the readers use, so a name the code stops reading cannot stay
// listed as known, and a new one cannot start as an unrecognized typo. The
// bearer chain contributes only the names it spells TOKTOP_*, since the
// others (OMNIROUTE_API_KEY) are not this program's to claim.
// See also SSH_AUTH_SOCK and the ssh defaults.
var toktopEnvVars = knownToktopEnv()

func knownToktopEnv() map[string]bool {
	names := []string{remote.PasswordEnv, logcfg.LevelEnv, screenshotFontEnv}
	for _, e := range frameEnvVars {
		names = append(names, e.name)
	}
	for _, name := range bearerEnvVars {
		if strings.HasPrefix(name, "TOKTOP_") {
			names = append(names, name)
		}
	}
	m := make(map[string]bool, len(names))
	for _, name := range names {
		m[name] = true
	}
	return m
}

// warnUnknownEnv reports unrecognized TOKTOP_* variables once at startup:
// a misspelled knob would otherwise be ignored silently and look like a
// no-op feature. Sorted, so the same set of names reads the same way in
// every capture of the startup output.
func warnUnknownEnv() {
	var unknown []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "TOKTOP_") || toktopEnvVars[name] {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		fmt.Fprintf(os.Stderr, "toktop: ignoring unknown environment variable(s): %s\n",
			strings.Join(reportedNames(unknown), ", "))
	}
}

// maxReportedName caps one externally supplied name printed in a startup
// warning. The names come from a definitions file and from the environment,
// so a wrapper or a supervisor can put a megabyte-long key in one, and the
// startup output is read in a terminal and pasted into issues.
const maxReportedName = 64

// reportedField makes one externally supplied name safe to print on stderr.
// Both name sets are text this program did not write: a key out of
// ~/.gauntlet/agents.json and a name out of the environment. Untreated, an
// escape sequence in either reaches the operator's terminal, and an
// unknown-key warning is the first thing a run prints, so it is the cheapest
// place in the tree to plant a clipboard write or a title change. SanitizeText
// drops the sequences, the bidi controls and the zero-width marks that render
// one key as another, and the cap bounds what one name can spend, both
// cutting between grapheme clusters so a name ending in an emoji or a
// decomposed accent is never sliced mid-character.
func reportedField(s string) string {
	// The home fold rides with the sanitizer: the names come from an
	// operator's agents.json and from the environment, and a definitions key
	// that is path-shaped (a shared or synced config) would otherwise put
	// the account into the first line the run prints. The path printed
	// beside it is folded for the same reason.
	return core.TruncateClusters(core.SingleLine(core.RedactHome(s)), maxReportedName)
}

// loadAgentDefs pulls in ~/.gauntlet/agents.json so --agents can follow
// agents toktop was not built to know (in-house wrappers, the pi family),
// including where they keep their transcripts. A missing file is the normal
// case; a malformed or unreadable one is returned so the caller can refuse to
// start: agents silently missing from the watch look exactly like agents
// doing nothing.
//
// An empty path is the home directory lookup failing inside DefinitionsPath.
// It is an error, not an absent file: without the path no agents file is read
// at all, and a startup that says nothing about it leaves the operator looking
// at a watch that reports no in-house agents for the whole run.
func loadAgentDefs() error {
	path := agentusage.DefinitionsPath()
	if path == "" {
		return fmt.Errorf("cannot locate agents.json: no home directory and no absolute $%s", agentusage.GauntletHomeEnv)
	}
	if err := agentusage.LoadDefinitions(path); err != nil {
		return err
	}
	warnUnknownUsageKeys(path)
	return nil
}

// warnUnknownUsageKeys names the usage keys a definitions file spells that
// agentusage has no field for. The file is gauntlet's, so an unrecognized key
// is a version this build is older than rather than an error, and refusing to
// start over one would break a run on a newer gauntlet than this build. It is
// still named, because a key nobody reads is usually a key nobody spelled: one
// with no counterpart in the known set leaves the agent with whatever the rest
// of its block said, and a block naming only that key registers no transcripts
// at all, which on the dashboard is an agent that used no tokens.
//
// One line, on stderr with the rest of the startup warnings, with the home
// folded out of the path the way every other line naming the file folds it.
func warnUnknownUsageKeys(path string) {
	unknown := agentusage.UnknownUsageKeys()
	if len(unknown) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: %s: ignoring unknown usage key(s) %s (this build reads %s)\n",
		core.RedactHome(path), strings.Join(reportedNames(unknown), ", "),
		strings.Join(agentusage.UsageKeyNames(), ", "))
}

// reportedNames is reportedField over a list, so both startup warnings read
// one sorted list of names the same way.
func reportedNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = reportedField(name)
	}
	return out
}
