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

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/remote"
)

// Mode and environment validation, and the warnings for flags and env vars
// that were set but cannot take effect.

func warnIgnoredFlags(set map[string]bool, demo, once, plain, agents, noIngest bool, nAdd, nRemote int) {
	if set["opencode-db"] && !agents {
		fmt.Fprintln(os.Stderr, "toktop: --opencode-db has no effect without --agents")
	}
	if set["ingest"] && noIngest {
		fmt.Fprintln(os.Stderr, "toktop: --ingest has no effect with --no-ingest")
	}
	if set["seed"] && !demo {
		fmt.Fprintln(os.Stderr, "toktop: --seed has no effect without --demo")
	}
	if set["frames"] && !once {
		fmt.Fprintln(os.Stderr, "toktop: --frames has no effect without --once")
	} else if set["frames"] && plain {
		// The plain report renders the last snapshot as a linear list; there
		// is no chart for the earlier frames to fill, so the count only buys
		// the wait before it. Gated on once so a --frames --plain run with no
		// --once is not also told how --once would use it.
		fmt.Fprintln(os.Stderr, "toktop: --frames only sets how long --once waits with --plain; the text report renders the last snapshot")
	}
	if set["plain"] && !once {
		fmt.Fprintln(os.Stderr, "toktop: --plain has no effect without --once")
	}
	if set["no-hot-reload"] && once {
		fmt.Fprintln(os.Stderr, "toktop: --no-hot-reload has no effect with --once")
	}
	if demo && set["add"] {
		fmt.Fprintln(os.Stderr, "toktop: --add has no effect with --demo")
	}
	if demo && set["bearer"] {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect with --demo")
	}
	if set["bearer"] && !demo && nAdd == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect without --add")
	}
	if demo && set["ssh-key"] {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect with --demo")
	}
	if demo && nRemote > 0 {
		fmt.Fprintln(os.Stderr, "toktop: ssh:// targets have no effect with --demo")
	}
	if set["ssh-key"] && !demo && nRemote == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect without an ssh:// target")
	}
}

// warnIgnoredFrameEnv names TOKTOP_COLUMNS / TOKTOP_LINES when they are set
// but no sized frame is rendered: the overrides only size the --once
// dashboard frame, and a silently ignored variable looks like a broken knob,
// same as a flag passed into a mode that never reads it. The --plain report
// is unsized by construction, so it reads neither. Without --once there is
// no report at all, so that is the reason named first: --plain is itself
// already named as having no effect there.
func warnIgnoredFrameEnv(once, plain bool) {
	if once && !plain {
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
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --plain; the text report has no fixed frame size\n", name)
	}
}

// warnIgnoredGauntletHome names a $GAUNTLET_HOME that is set but not a path
// DefinitionsPath can use. agentusage honors it only when absolute, so a
// relative value silently falls back to ~/.gauntlet and the agents.json the
// operator pointed at is never read. Silence there looks like agents producing
// no tokens, so the value is named instead.
//
// Only --agents reads the file, so the warning fires there too: without it
// nothing consulted the variable.
func warnIgnoredGauntletHome(agents bool) {
	if !agents {
		return
	}
	if v := os.Getenv("GAUNTLET_HOME"); v != "" && !filepath.IsAbs(v) {
		fmt.Fprintf(os.Stderr, "toktop: $GAUNTLET_HOME must be an absolute path; ignoring %q and reading ~/.gauntlet/agents.json\n", v)
	}
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
func warnUnusedEnv(bearerFlag, demo, noIngest bool, nAdd, nRemote int) {
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
	if noIngest && os.Getenv(logcfg.LevelEnv) != "" {
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --no-ingest\n", logcfg.LevelEnv)
	}
}

// probeSecsMax caps auto-probe scheduling at 24h, well below the point where
// time.Duration(n)*time.Second overflows and NewTicker would panic.
const probeSecsMax = 24 * 60 * 60

// Poll interval bounds apply after flag.Duration parses a unit-bearing value
// (or zero). The 50ms floor prevents excessive polling; a provider request is
// bounded by provider.PollTimeout (1.5s). The 1h ceiling also caps the
// per-frame wait in --once, which is three times the interval.
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
	if interval < intervalMin {
		if interval < time.Millisecond {
			return fmt.Errorf("--interval must be >= %s, got %s (bare numbers are nanoseconds; use 1s or 500ms)", intervalMin, interval)
		}
		return fmt.Errorf("--interval must be >= %s, got %s", intervalMin, interval)
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
