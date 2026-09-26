package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/ui"
)

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func TestOutputFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(io.Writer) int
	}{
		{"help", func(w io.Writer) int { return runHelp(w, nil) }},
		{"version help", func(w io.Writer) int { return runVersion(w, []string{"--help"}) }},
		{"version", func(w io.Writer) int { return runVersion(w, nil) }},
		{"update help", func(w io.Writer) int { return runUpdate(context.Background(), w, []string{"--help"}) }},
		{"update version", func(w io.Writer) int { return runUpdate(context.Background(), w, []string{"--version"}) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = tt.run(failedOutput{}) })
			if code != 1 || !strings.Contains(stderr, "write stdout: output unavailable") {
				t.Fatalf("code = %d, stderr = %q; want 1 and output error", code, stderr)
			}
		})
	}
}

func TestOutputStatusBrokenPipe(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"epipe", syscall.EPIPE},
		{"closed pipe", io.ErrClosedPipe},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = outputStatus(tt.err) })
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q; want 0 and no stderr", code, stderr)
			}
		})
	}
}

func TestUpdateErrorOmitsHomeDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-user")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("home", home)
	self := filepath.Join(home, "bin", "toktop")
	tmp := filepath.Join(home, "bin", ".toktop-update-123")
	for _, tc := range []struct {
		name string
		err  error
		want string
		code int
	}{
		{"create", fmt.Errorf("cannot write next to %s: %w", self,
			&os.PathError{Op: "open", Path: tmp, Err: os.ErrPermission}),
			"cannot write next to " + filepath.Join("~", "bin", "toktop") + ": open " + filepath.Join("~", "bin", ".toktop-update-123") + ": permission denied", 1},
		{"rename", fmt.Errorf("cannot replace %s: %w", self,
			&os.LinkError{Op: "rename", Old: tmp, New: self, Err: os.ErrPermission}),
			"cannot replace " + filepath.Join("~", "bin", "toktop") + ": rename " + filepath.Join("~", "bin", ".toktop-update-123") + " " + filepath.Join("~", "bin", "toktop") + ": permission denied", 1},
		{"ordinary", errors.New("checksum mismatch"), "checksum mismatch", 1},
		{"canceled", fmt.Errorf("download: %w", context.Canceled), "interrupted", 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			got := captureStderr(t, func() { code = updateErr("update failed", tc.err) })
			if code != tc.code || !strings.Contains(got, tc.want) {
				t.Errorf("code = %d, stderr = %q; want %d and %q", code, got, tc.code, tc.want)
			}
			if strings.Contains(got, home) || strings.Contains(got, "private-user") {
				t.Errorf("update diagnostic contains home directory: %q", got)
			}
		})
	}
}

func TestRunOnceOutput(t *testing.T) {
	t.Setenv("TOKTOP_COLUMNS", "120")
	t.Setenv("TOKTOP_LINES", "38")
	for _, plain := range []bool{false, true} {
		t.Run(strconv.FormatBool(plain), func(t *testing.T) {
			cfg := ui.Config{Version: "test", PollEvery: time.Second}
			snap := core.Snapshot{Uptime: 7 * time.Second}
			var out bytes.Buffer
			for _, w := range []io.Writer{&out, failedOutput{}} {
				ch := make(chan core.Snapshot, 1)
				ch <- snap
				var code int
				stderr := captureStderr(t, func() {
					code = runOnce(context.Background(), w, cfg, ch, 1, plain)
				})
				if w == &out {
					want := ui.StaticFrame(cfg, snap, 120, 38)
					if plain {
						want = ui.PlainTextFrame(cfg, snap)
					}
					if code != 0 || stderr != "" || out.String() != want+"\n" {
						t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr, out.String())
					}
				} else if code != 1 || !strings.Contains(stderr, "write stdout: output unavailable") {
					t.Fatalf("code = %d, stderr = %q; want 1 and output error", code, stderr)
				}
			}
		})
	}
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		stamped, module, want string
	}{
		{stamped: "0.5.0", module: "v9.9.9", want: "0.5.0"}, // release ldflags
		{stamped: "v0.5.0", module: "", want: "0.5.0"},      // tag spelling
		{stamped: "dev", module: "v0.5.0", want: "dev"},     // make build
		{stamped: "dev", module: "(devel)", want: "dev"},
		{stamped: "", module: "v0.5.0", want: "0.5.0"}, // go install @v0.5.0
		{stamped: "", module: "(devel)", want: "dev"},  // go build, no vcs version
		{stamped: "", module: "", want: "dev"},
	}
	for _, tt := range tests {
		if got := resolveVersion(tt.stamped, tt.module); got != tt.want {
			t.Errorf("resolveVersion(%q, %q) = %q, want %q", tt.stamped, tt.module, got, tt.want)
		}
	}
}

// Unstamped binaries used to report 0.1.0, which is a real tag. go test of
// this tree must not impersonate that release.
func TestVersionIsNotTheFirstReleaseTag(t *testing.T) {
	if version == "0.1.0" {
		t.Fatalf("version = %q; unstamped builds must not report the v0.1.0 tag", version)
	}
}

func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name      string
		once      bool
		interval  time.Duration
		probeSecs int
		frames    int
		wantErr   string
	}{
		{name: "defaults are valid", interval: time.Second, frames: 2},
		{name: "probe off is zero, not negative", interval: time.Second, probeSecs: 0},
		{name: "negative interval rejected", interval: -time.Second, wantErr: "--interval"},
		{name: "zero interval rejected", interval: 0, wantErr: "--interval"},
		{name: "bare-number nanoseconds rejected", interval: time.Nanosecond, wantErr: "nanoseconds"},
		{name: "below floor rejected", interval: minInterval - time.Millisecond, wantErr: "--interval"},
		{name: "floor accepted", interval: minInterval},
		{name: "cap accepted", interval: maxInterval},
		{name: "above cap rejected", interval: maxInterval + time.Second, wantErr: "--interval"},
		{name: "negative probe rejected", interval: time.Second, probeSecs: -5, wantErr: "--probe"},
		{name: "probe at cap accepted", interval: time.Second, probeSecs: maxProbeSecs},
		{name: "probe above cap rejected", interval: time.Second, probeSecs: maxProbeSecs + 1, wantErr: "--probe"},
		{name: "frames unchecked without --once", interval: time.Second, frames: 0},
		{name: "zero frames with --once rejected", once: true, interval: time.Second, frames: 0, wantErr: "--frames"},
		{name: "negative frames with --once rejected", once: true, interval: time.Second, frames: -1, wantErr: "--frames"},
		{name: "frames at history length accepted", once: true, interval: time.Second, frames: core.HistoryLen},
		{name: "frames above history length rejected", once: true, interval: time.Second, frames: core.HistoryLen + 1, wantErr: "--frames"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFlags(tt.once, tt.interval, tt.probeSecs, tt.frames)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateFlags() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateFlags() = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

// captureStderr runs f with stderr redirected and returns what it printed.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func captureWarnUnknownEnv(t *testing.T) string {
	t.Helper()
	return captureStderr(t, warnUnknownEnv)
}

// isolateToktopEnv unsets every TOKTOP_* variable so silence assertions do
// not depend on the caller's environment.
func isolateToktopEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "TOKTOP_") {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Setenv(name, val) })
	}
}

func TestWarnUnknownEnv(t *testing.T) {
	isolateToktopEnv(t)
	t.Run("known variables pass silently", func(t *testing.T) {
		t.Setenv("TOKTOP_BEARER", "x")
		t.Setenv("TOKTOP_SSH_PASSWORD", "x")
		t.Setenv("TOKTOP_COLUMNS", "80")
		t.Setenv("TOKTOP_LINES", "24")
		t.Setenv("TOKTOP_LOG_LEVEL", "warn")
		t.Setenv("TOKTOP_SCREENSHOT_FONT", "/tmp/Meslo.ttf")
		if got := captureWarnUnknownEnv(t); got != "" {
			t.Fatalf("warnUnknownEnv() printed %q, want silence", got)
		}
	})
	t.Run("misspelled variable is named", func(t *testing.T) {
		t.Setenv("TOKTOP_BEARE", "x") // typo: must not be swallowed
		got := captureWarnUnknownEnv(t)
		if !strings.Contains(got, "TOKTOP_BEARE") {
			t.Fatalf("warnUnknownEnv() printed %q, want mention of TOKTOP_BEARE", got)
		}
	})
	t.Run("unrelated variables ignored", func(t *testing.T) {
		t.Setenv("OTHER_VAR", "x")
		if got := captureWarnUnknownEnv(t); got != "" {
			t.Fatalf("warnUnknownEnv() printed %q, want silence", got)
		}
	})
}

func TestFrameEnv(t *testing.T) {
	t.Run("trimmed value is used", func(t *testing.T) {
		t.Setenv("TOKTOP_COLUMNS", " 120 ")
		n, set, err := frameEnv("TOKTOP_COLUMNS", minFrameColumns, maxFrameColumns)
		if err != nil || !set || n != 120 {
			t.Fatalf("frameEnv() = %d, %v, %v, want 120, true, nil", n, set, err)
		}
	})
	t.Run("unset is not set", func(t *testing.T) {
		t.Setenv("TOKTOP_COLUMNS", "")
		n, set, err := frameEnv("TOKTOP_COLUMNS", minFrameColumns, maxFrameColumns)
		if err != nil || set || n != 0 {
			t.Fatalf("frameEnv() = %d, %v, %v, want 0, false, nil", n, set, err)
		}
	})
}

func TestValidateOnceEnv(t *testing.T) {
	tests := []struct {
		name    string
		columns string
		lines   string
		wantErr string // empty means silence expected
	}{
		{name: "unset passes"},
		{name: "empty means unset", columns: "", lines: ""},
		{name: "whitespace only means unset", columns: "  ", lines: "\t"},
		{name: "typical values pass", columns: "120", lines: "38"},
		{name: "floors accepted", columns: "41", lines: "21"},
		{name: "caps accepted", columns: strconv.Itoa(maxFrameColumns), lines: strconv.Itoa(maxFrameLines)},
		{name: "whitespace trimmed", columns: " 120 ", lines: " 38 "},
		{name: "below floor rejected", columns: "40", wantErr: "TOKTOP_COLUMNS"},
		{name: "above cap rejected", columns: strconv.Itoa(maxFrameColumns + 1), wantErr: "TOKTOP_COLUMNS"},
		{name: "lines above cap rejected", lines: strconv.Itoa(maxFrameLines + 1), wantErr: "TOKTOP_LINES"},
		{name: "not a number rejected", lines: "full-hd", wantErr: "TOKTOP_LINES"},
		{name: "negative rejected", columns: "-1", wantErr: "TOKTOP_COLUMNS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_COLUMNS", tt.columns)
			t.Setenv("TOKTOP_LINES", tt.lines)
			err := validateOnceEnv()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateOnceEnv() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateOnceEnv() = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

// writeAgentsJSON points GAUNTLET_HOME at a temp dir holding the given file
// body ("" writes nothing, leaving agents.json absent).
func writeAgentsJSON(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		path := filepath.Join(dir, "agents.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadAgentDefs(t *testing.T) {
	t.Run("missing file is silent", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", writeAgentsJSON(t, ""))
		if err := loadAgentDefs(); err != nil {
			t.Fatalf("loadAgentDefs() = %v, want nil for an absent agents.json", err)
		}
	})

	t.Run("malformed file aborts startup naming the file", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", writeAgentsJSON(t, "{oops"))
		err := loadAgentDefs()
		if err == nil {
			t.Fatal("loadAgentDefs() = nil, want an error for a malformed agents.json")
		}
		if !errors.Is(err, agentusage.ErrInvalidDefinitions) {
			t.Fatalf("loadAgentDefs() = %v, want ErrInvalidDefinitions", err)
		}
		if !strings.Contains(err.Error(), "agents.json") {
			t.Fatalf("loadAgentDefs() = %v, want the file named", err)
		}
	})

	t.Run("valid file loads quietly", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", writeAgentsJSON(t,
			`{"deftest-agent":{"usage":{"roots":["~/.deftest/sessions"]}}}`))
		if err := loadAgentDefs(); err != nil {
			t.Fatalf("loadAgentDefs() = %v, want nil", err)
		}
		if !slices.Contains(agentusage.Agents(), "deftest-agent") {
			t.Fatalf("defined agent missing from %v", agentusage.Agents())
		}
	})
}

func TestUsage(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	got := buf.String()
	for _, want := range []string{
		"toktop -",              // what the tool is
		"Usage:",                // invocation line
		"[ssh://user@host ...",  // positional targets documented
		"toktop help",           // git-style help command
		"toktop version",        // git-style version command
		"help [update|version]", // both extra commands are help topics
		"Examples:",             // worked examples section
		"-demo",                 // generated flag docs survive
		"-interval",             // PrintDefaults, not just the examples
		"-add",                  // repeatable backend flag
		"1s or 500ms",           // --interval names the duration format
		"min 50ms",              // --interval floor (bare numbers are nanoseconds)
		"OMNIROUTE_API_KEY",     // env fallbacks named
		"--add",                 // http(s) leftovers hint at --add
		"userinfo",              // --add must not embed credentials
		"TOKTOP_SSH_PASSWORD",   // ssh URL must not embed a password
		"ssh://[user@]host",     // ssh target shape
		"host:port",             // --ingest listen address shape
		"piping",                // --once is the non-TTY path
		"needs a terminal",      // live dashboard vs --once
	} {
		if !strings.Contains(got, want) {
			t.Errorf("usage() missing %q", want)
		}
	}
}

// runUpdate must answer -h/--help the way the top-level command does: full
// usage on stdout with exit 0, so `toktop update --help | grep repo` works.
func TestRunUpdateHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			var code int
			got := captureStderr(t, func() {
				code = runUpdate(context.Background(), &out, []string{arg})
			})
			if code != 0 {
				t.Fatalf("runUpdate(%q) = %d, want 0", arg, code)
			}
			if out.Len() == 0 {
				t.Fatalf("runUpdate(%q) wrote nothing to stdout", arg)
			}
			if got != "" {
				t.Fatalf("runUpdate(%q) leaked %q to stderr", arg, got)
			}
			for _, want := range []string{"Usage:", "--check", "--repo", "owner/name", "--version", "GITHUB_TOKEN"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("update help missing %q", want)
				}
			}
		})
	}
}

func TestRunUpdateUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub string // required substring on stderr
	}{
		{name: "unknown flag", args: []string{"--bogus"}, wantSub: "flag provided but not defined"},
		{name: "unexpected argument", args: []string{"extra"}, wantSub: "unexpected argument"},
		{name: "unexpected argument points at help", args: []string{"extra"}, wantSub: "toktop update --help"},
		{name: "repo path traversal", args: []string{"--repo", "maci0/toktop/../../../users/octocat"}, wantSub: "owner/name"},
		{name: "repo query string", args: []string{"--repo", "maci0/toktop?evil=1"}, wantSub: "owner/name"},
		{name: "empty repo", args: []string{"--repo", ""}, wantSub: "owner/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			got := captureStderr(t, func() {
				code = runUpdate(context.Background(), io.Discard, tt.args)
			})
			if code != 2 {
				t.Fatalf("runUpdate(%v) = %d, want 2", tt.args, code)
			}
			if !strings.Contains(got, tt.wantSub) {
				t.Fatalf("stderr = %q, want mention of %q", got, tt.wantSub)
			}
		})
	}
}

func TestWarnIgnoredFlags(t *testing.T) {
	tests := []struct {
		name     string
		set      map[string]bool
		demo     bool
		once     bool
		agents   bool
		noIngest bool
		nAdd     int
		nRemote  int
		wantSub  string // empty means silence expected
		wantAlso string // additional flag that must be named
	}{
		{name: "seed outside demo warns", set: map[string]bool{"seed": true}, wantSub: "--seed"},
		{name: "seed inside demo silent", set: map[string]bool{"seed": true}, demo: true},
		{name: "seed default silent", set: map[string]bool{}, demo: false},
		{name: "frames outside once warns", set: map[string]bool{"frames": true}, wantSub: "--frames"},
		{name: "frames inside once silent", set: map[string]bool{"frames": true}, once: true},
		{name: "both no-ops warn twice", set: map[string]bool{"seed": true, "frames": true},
			wantSub: "--seed", wantAlso: "--frames"},
		{name: "opencode-db without agents warns", set: map[string]bool{"opencode-db": true},
			wantSub: "--opencode-db"},
		{name: "opencode-db with agents silent", set: map[string]bool{"opencode-db": true}, agents: true},
		{name: "plain outside once warns", set: map[string]bool{"plain": true}, wantSub: "--plain"},
		{name: "plain inside once silent", set: map[string]bool{"plain": true}, once: true},
		{name: "hot-reload with once warns", set: map[string]bool{"no-hot-reload": true}, once: true,
			wantSub: "--no-hot-reload"},
		{name: "hot-reload without once silent", set: map[string]bool{"no-hot-reload": true}},
		{name: "add with demo warns", set: map[string]bool{"add": true}, demo: true, wantSub: "--add"},
		{name: "add without demo silent", set: map[string]bool{"add": true}},
		{name: "bearer with demo warns", set: map[string]bool{"bearer": true}, demo: true, wantSub: "--bearer"},
		{name: "bearer without add warns", set: map[string]bool{"bearer": true}, wantSub: "--bearer"},
		{name: "bearer with add silent", set: map[string]bool{"bearer": true}, nAdd: 1},
		{name: "ssh-key with demo warns", set: map[string]bool{"ssh-key": true}, demo: true, nRemote: 1,
			wantSub: "--ssh-key"},
		{name: "ssh-key without target warns", set: map[string]bool{"ssh-key": true}, wantSub: "--ssh-key"},
		{name: "ssh-key with target silent", set: map[string]bool{"ssh-key": true}, nRemote: 1},
		{name: "ssh target with demo warns", demo: true, nRemote: 1, wantSub: "ssh://"},
		{name: "ssh target without demo silent", nRemote: 1},
		{name: "ingest with no-ingest warns", set: map[string]bool{"ingest": true}, noIngest: true,
			wantSub: "--ingest"},
		{name: "ingest without no-ingest silent", set: map[string]bool{"ingest": true}},
		{name: "no-ingest without ingest silent", noIngest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureStderr(t, func() {
				warnIgnoredFlags(tt.set, tt.demo, tt.once, tt.agents, tt.noIngest, tt.nAdd, tt.nRemote)
			})
			if tt.wantSub == "" {
				if got != "" {
					t.Fatalf("warnIgnoredFlags() printed %q, want silence", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantSub) {
				t.Fatalf("warnIgnoredFlags() printed %q, want mention of %q", got, tt.wantSub)
			}
			if tt.wantAlso != "" && !strings.Contains(got, tt.wantAlso) {
				t.Fatalf("warnIgnoredFlags() printed %q, want mention of %q", got, tt.wantAlso)
			}
			if tt.wantAlso != "" && strings.Count(got, "has no effect") < 2 {
				t.Fatalf("warnIgnoredFlags() printed %q, want two no-effect warnings", got)
			}
		})
	}
}

func TestWaitForFrames(t *testing.T) {
	mark := core.Snapshot{Uptime: 7 * time.Second} // comparable field identifies the frame
	t.Run("collects the requested frames", func(t *testing.T) {
		ch := make(chan core.Snapshot, 2)
		ch <- mark
		ch <- mark
		got, err := waitForFrames(context.Background(), ch, 2, time.Second)
		if err != nil || got.Uptime != mark.Uptime {
			t.Fatalf("waitForFrames() = %+v, %v; want the sent frame, nil", got, err)
		}
	})
	t.Run("timeout names the cause", func(t *testing.T) {
		ch := make(chan core.Snapshot)
		if _, err := waitForFrames(context.Background(), ch, 1, 10*time.Millisecond); err == nil ||
			strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("waitForFrames() = %q, want timeout error", err)
		}
	})
	// A Ctrl+C during --once must abort the wait at once rather than hang
	// until the per-frame timeout fires.
	t.Run("canceled context interrupts immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		_, err := waitForFrames(ctx, make(chan core.Snapshot), 3, time.Minute)
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("waitForFrames() = %v, want errInterrupted", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("waitForFrames() took %s after cancellation, want prompt return", elapsed)
		}
	})
	t.Run("closed channel reports an error", func(t *testing.T) {
		ch := make(chan core.Snapshot)
		close(ch)
		if _, err := waitForFrames(context.Background(), ch, 2, time.Second); err == nil {
			t.Fatal("waitForFrames() succeeded on closed channel, want error")
		}
	})
}

func TestWarnIgnoredFrameEnv(t *testing.T) {
	tests := []struct {
		name    string
		once    bool
		columns string
		lines   string
		wantSub string // empty means silence expected
	}{
		{name: "unset is silent"},
		{name: "whitespace only is silent", columns: "  ", lines: "\t"},
		{name: "columns outside once warns", columns: "120", wantSub: "TOKTOP_COLUMNS"},
		{name: "lines outside once warns", lines: "38", wantSub: "TOKTOP_LINES"},
		{name: "both set warn twice", columns: "120", lines: "38", wantSub: "TOKTOP_COLUMNS"},
		{name: "inside once silent", once: true, columns: "120", lines: "38"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_COLUMNS", tt.columns)
			t.Setenv("TOKTOP_LINES", tt.lines)
			got := captureStderr(t, func() { warnIgnoredFrameEnv(tt.once) })
			if tt.wantSub == "" {
				if got != "" {
					t.Fatalf("warnIgnoredFrameEnv() printed %q, want silence", got)
				}
				return
			}
			if n := strings.Count(got, "has no effect"); tt.columns != "" && tt.lines != "" && n < 2 {
				t.Fatalf("warnIgnoredFrameEnv() printed %d warnings, want 2 for both variables", n)
			}
			if !strings.Contains(got, tt.wantSub) {
				t.Fatalf("warnIgnoredFrameEnv() printed %q, want mention of %q", got, tt.wantSub)
			}
		})
	}
}

func TestWarnUnusedEnv(t *testing.T) {
	isolateToktopEnv(t)
	t.Setenv("OMNIROUTE_API_KEY", "")
	tests := []struct {
		name       string
		bearerFlag bool
		demo       bool
		noIngest   bool
		nAdd       int
		nRemote    int
		sshPass    string
		omni       string
		bearer     string
		logLevel   string
		wantSub    string
	}{
		{name: "unset is silent"},
		{name: "ssh password without target warns", sshPass: "x", wantSub: "TOKTOP_SSH_PASSWORD"},
		{name: "ssh password with target silent", sshPass: "x", nRemote: 1},
		{name: "ssh password with demo warns", sshPass: "x", demo: true, nRemote: 1, wantSub: "TOKTOP_SSH_PASSWORD"},
		{name: "bearer env without add warns", bearer: "x", wantSub: "TOKTOP_BEARER"},
		{name: "omni env without add warns", omni: "x", wantSub: "OMNIROUTE_API_KEY"},
		{name: "bearer env with add silent", bearer: "x", nAdd: 1},
		{name: "bearer env with demo warns", bearer: "x", demo: true, nAdd: 1, wantSub: "TOKTOP_BEARER"},
		{name: "bearer flag suppresses env warning", bearerFlag: true, bearer: "x"},
		{name: "log level with no-ingest warns", logLevel: "warn", noIngest: true, wantSub: "TOKTOP_LOG_LEVEL"},
		{name: "log level with ingest silent", logLevel: "warn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_SSH_PASSWORD", tt.sshPass)
			t.Setenv("OMNIROUTE_API_KEY", tt.omni)
			t.Setenv("TOKTOP_BEARER", tt.bearer)
			t.Setenv("TOKTOP_LOG_LEVEL", tt.logLevel)
			got := captureStderr(t, func() {
				warnUnusedEnv(tt.bearerFlag, tt.demo, tt.noIngest, tt.nAdd, tt.nRemote)
			})
			if tt.wantSub == "" {
				if got != "" {
					t.Fatalf("warnUnusedEnv() printed %q, want silence", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantSub) {
				t.Fatalf("warnUnusedEnv() printed %q, want mention of %q", got, tt.wantSub)
			}
		})
	}
}

func TestValidateLogLevelEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "unset passes"},
		{name: "empty means unset", value: ""},
		{name: "info passes", value: "info"},
		{name: "warn passes", value: "WARN"},
		{name: "bogus rejected", value: "trace", wantErr: "TOKTOP_LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_LOG_LEVEL", tt.value)
			err := validateLogLevelEnv()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLogLevelEnv() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLogLevelEnv() = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunUpdateVersion(t *testing.T) {
	var out bytes.Buffer
	var code int
	got := captureStderr(t, func() {
		code = runUpdate(context.Background(), &out, []string{"--version"})
	})
	if code != 0 {
		t.Fatalf("runUpdate(--version) = %d, want 0", code)
	}
	if got != "" {
		t.Fatalf("runUpdate(--version) leaked %q to stderr", got)
	}
	if !strings.Contains(out.String(), "toktop "+version) {
		t.Fatalf("stdout = %q, want toktop %s", out.String(), version)
	}
}

func TestRunUpdateInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var code int
	got := captureStderr(t, func() {
		code = runUpdate(ctx, io.Discard, nil)
	})
	if code != 130 {
		t.Fatalf("runUpdate(canceled) = %d, want 130", code)
	}
	if !strings.Contains(got, "interrupted") {
		t.Fatalf("stderr = %q, want interrupted", got)
	}
}

func TestInformationalCommandsRejectExtraArguments(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(io.Writer, []string) int
		args []string
	}{
		{"help help", runHelp, []string{"help", "extra"}},
		{"help short flag", runHelp, []string{"-h", "extra"}},
		{"help double short flag", runHelp, []string{"--h", "extra"}},
		{"help single dash flag", runHelp, []string{"-help", "extra"}},
		{"help long flag", runHelp, []string{"--help", "extra"}},
		{"version short help", runVersion, []string{"-h", "extra"}},
		{"version double short help", runVersion, []string{"--h", "extra"}},
		{"version single dash help", runVersion, []string{"-help", "extra"}},
		{"version long help", runVersion, []string{"--help", "extra"}},
		{"update help", func(w io.Writer, args []string) int {
			return runUpdate(context.Background(), w, args)
		}, []string{"--help", "extra"}},
		{"update short help", func(w io.Writer, args []string) int {
			return runUpdate(context.Background(), w, args)
		}, []string{"-h", "extra"}},
		{"update version", func(w io.Writer, args []string) int {
			return runUpdate(context.Background(), w, args)
		}, []string{"--version", "extra"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			var code int
			stderr := captureStderr(t, func() { code = tt.run(&out, tt.args) })
			if code != 2 || out.Len() != 0 {
				t.Fatalf("code = %d, stdout = %q; want 2 and no stdout", code, out.String())
			}
			if !strings.Contains(stderr, `unexpected argument "extra"`) || !strings.Contains(stderr, "--help") {
				t.Fatalf("stderr = %q; want unexpected argument and help hint", stderr)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Run("no topic prints top-level help", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, nil) })
		if code != 0 {
			t.Fatalf("runHelp() = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runHelp() leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "-demo") || !strings.Contains(out.String(), "-interval") {
			t.Fatalf("runHelp() stdout missing top-level usage with flags: %q", out.String())
		}
	})
	t.Run("update topic prints update help", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, []string{"update"}) })
		if code != 0 {
			t.Fatalf("runHelp(update) = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runHelp(update) leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "--check") || strings.Contains(out.String(), "-demo") {
			t.Fatalf("runHelp(update) stdout = %q, want update help", out.String())
		}
	})
	t.Run("unknown topic is a usage error", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runHelp(io.Discard, []string{"bogus"}) })
		if code != 2 {
			t.Fatalf("runHelp(bogus) = %d, want 2", code)
		}
		if !strings.Contains(got, "bogus") || !strings.Contains(got, "toktop --help") {
			t.Fatalf("stderr = %q, want topic name and --help", got)
		}
		if strings.Contains(got, "unknown option") {
			t.Fatalf("stderr = %q, bare word must not be called an option", got)
		}
	})
	t.Run("version topic prints top-level help", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, []string{"version"}) })
		if code != 0 {
			t.Fatalf("runHelp(version) = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runHelp(version) leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "-demo") {
			t.Fatalf("runHelp(version) stdout missing top-level usage: %q", out.String())
		}
	})
	t.Run("dashed leftover is an unknown option", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runHelp(io.Discard, []string{"--demo"}) })
		if code != 2 {
			t.Fatalf("runHelp(--demo) = %d, want 2", code)
		}
		if !strings.Contains(got, "unknown option") || !strings.Contains(got, "--demo") {
			t.Fatalf("stderr = %q, want unknown option --demo", got)
		}
		if strings.Contains(got, "no help topic") {
			t.Fatalf("stderr = %q, a dashed arg is an option not a topic", got)
		}
	})
	t.Run("extra after update topic is a usage error", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, []string{"update", "extra"}) })
		if code != 2 {
			t.Fatalf("runHelp(update extra) = %d, want 2", code)
		}
		if out.Len() != 0 {
			t.Fatalf("runHelp(update extra) wrote %q to stdout", out.String())
		}
		if !strings.Contains(got, "extra") || !strings.Contains(got, "toktop --help") {
			t.Fatalf("stderr = %q, want extra and --help", got)
		}
	})
	t.Run("extra after version topic is a usage error", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, []string{"version", "extra"}) })
		if code != 2 {
			t.Fatalf("runHelp(version extra) = %d, want 2", code)
		}
		if out.Len() != 0 {
			t.Fatalf("runHelp(version extra) wrote %q to stdout", out.String())
		}
		if !strings.Contains(got, "extra") || !strings.Contains(got, "toktop --help") {
			t.Fatalf("stderr = %q, want extra and --help", got)
		}
	})
	t.Run("help flag variants print top-level help", func(t *testing.T) {
		for _, arg := range []string{"-h", "--h", "-help", "--help", "help"} {
			var out bytes.Buffer
			var code int
			got := captureStderr(t, func() { code = runHelp(&out, []string{arg}) })
			if code != 0 || got != "" || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("runHelp(%q) = %d, stderr = %q", arg, code, got)
			}
		}
	})
}

func TestRunVersion(t *testing.T) {
	t.Run("prints version on stdout", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runVersion(&out, nil) })
		if code != 0 {
			t.Fatalf("runVersion() = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runVersion() leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "toktop "+version) {
			t.Fatalf("stdout = %q, want toktop %s", out.String(), version)
		}
	})
	t.Run("extra argument is a usage error", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runVersion(io.Discard, []string{"extra"}) })
		if code != 2 {
			t.Fatalf("runVersion(extra) = %d, want 2", code)
		}
		if !strings.Contains(got, "extra") {
			t.Fatalf("stderr = %q, want mention of extra", got)
		}
	})
	t.Run("--help prints top-level help", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runVersion(&out, []string{"--help"}) })
		if code != 0 {
			t.Fatalf("runVersion(--help) = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runVersion(--help) leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("stdout missing usage: %q", out.String())
		}
		if !strings.Contains(out.String(), "-demo") {
			t.Fatalf("stdout missing generated flags: %q", out.String())
		}
	})
	t.Run("help flag variants print top-level help", func(t *testing.T) {
		for _, arg := range []string{"-h", "--h", "-help", "--help"} {
			var out bytes.Buffer
			var code int
			got := captureStderr(t, func() { code = runVersion(&out, []string{arg}) })
			if code != 0 || got != "" || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("runVersion(%q) = %d, stderr = %q", arg, code, got)
			}
		}
	})
}

func TestInterpretArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantCmd string
		wantN   int
		wantErr string
	}{
		{name: "empty"},
		{name: "help", args: []string{"help"}, wantCmd: "help"},
		{name: "help update", args: []string{"help", "update"}, wantCmd: "help", wantN: 1},
		{name: "version", args: []string{"version"}, wantCmd: "version"},
		{name: "version extra", args: []string{"version", "x"}, wantErr: "toktop version:"},
		{name: "one ssh", args: []string{"ssh://maci@box"}, wantN: 1},
		{name: "two ssh", args: []string{"ssh://a", "ssh://b"}, wantN: 2},
		{name: "http url hints --add", args: []string{"http://127.0.0.1:8000"}, wantErr: "--add"},
		{name: "https url hints --add", args: []string{"https://example:8000"}, wantErr: "--add"},
		{name: "bare word points at --help", args: []string{"helpme"}, wantErr: "toktop --help"},
		{name: "update not first", args: []string{"update"}, wantErr: "toktop update"},
		{name: "ssh then junk", args: []string{"ssh://a", "nope"}, wantErr: "toktop --help"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, remotes, err := interpretArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("interpretArgs(%v) = %q, %v, %v; want error mentioning %q",
						tt.args, cmd, remotes, err, tt.wantErr)
				}
				if !strings.Contains(tt.wantErr, "--add") && strings.Contains(err.Error(), "--add") {
					t.Fatalf("interpretArgs(%v) error %q must not suggest --add", tt.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("interpretArgs(%v) = %v, want nil", tt.args, err)
			}
			if cmd != tt.wantCmd {
				t.Fatalf("cmd = %q, want %q", cmd, tt.wantCmd)
			}
			if len(remotes) != tt.wantN {
				t.Fatalf("remotes = %v, want %d", remotes, tt.wantN)
			}
		})
	}
}

func TestFlagAddRejectsEmpty(t *testing.T) {
	var adds []string
	if err := parseAdd("", &adds); err == nil {
		t.Fatal("empty --add must be rejected")
	}
	if err := parseAdd("   ", &adds); err == nil {
		t.Fatal("whitespace --add must be rejected")
	}
	if err := parseAdd("http://127.0.0.1:8000", &adds); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(adds, ","); got != "http://127.0.0.1:8000" {
		t.Fatalf("adds = %q", got)
	}
}

func TestValidateAddURL(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr string // empty means accepted
	}{
		{raw: "http://127.0.0.1:8000"},
		{raw: "https://engine.example:443/v1"},
		{raw: "http://[::1]:8080"},
		{raw: "127.0.0.1:8000", wantErr: "http:// or https://"},
		{raw: "ftp://127.0.0.1:21", wantErr: "http:// or https://"},
		{raw: "http://", wantErr: "missing host"},
		{raw: "http://:8080", wantErr: "missing host"},
		{raw: "http://[]:8080", wantErr: "http:// or https://"},
		{raw: "http://[::1]:65536", wantErr: "port"},
		{raw: "http://localhost:"},
		{raw: "http://localhost:0"},
		{raw: "http://localhost:65536", wantErr: "port"},
		{raw: "http://localhost:999999999999999999999999", wantErr: "port"},
		{raw: "http://localhost:65535"},
		{raw: "http://user:pass@127.0.0.1:8000", wantErr: "userinfo"},
		{raw: "http://user@127.0.0.1:8000", wantErr: "userinfo"},
	}
	for _, tt := range tests {
		err := validateAddURL(tt.raw)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("validateAddURL(%q) = %v, want nil", tt.raw, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("validateAddURL(%q) = %v, want error mentioning %q", tt.raw, err, tt.wantErr)
		}
	}
	var adds []string
	if err := parseAdd("not-a-url", &adds); err == nil {
		t.Fatal("scheme-less --add must be rejected")
	}
	if err := parseAdd("  https://10.0.0.5:8000  ", &adds); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(adds, ","); got != "https://10.0.0.5:8000" {
		t.Fatalf("trimmed --add stored %q", got)
	}
}

func TestValidateIngestAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr string
	}{
		{addr: "127.0.0.1:8420"},
		{addr: "[::1]:8420"},
		{addr: ":8420"},
		{addr: "0.0.0.0:0"},
		{addr: "localhost:8420"},
		{addr: "", wantErr: "not empty"},
		{addr: "   ", wantErr: "not empty"},
		{addr: "8420", wantErr: "host:port"},
		{addr: "0.0.0.0", wantErr: "host:port"},
		{addr: "127.0.0.1:http", wantErr: "port"},
		{addr: "127.0.0.1:65536", wantErr: "port"},
		{addr: "127.0.0.1:-1", wantErr: "port"},
	}
	for _, tt := range tests {
		err := validateIngestAddr(tt.addr)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("validateIngestAddr(%q) = %v, want nil", tt.addr, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("validateIngestAddr(%q) = %v, want error mentioning %q", tt.addr, err, tt.wantErr)
		}
	}
}

func TestLogActiveConfig(t *testing.T) {
	isolateToktopEnv(t)
	t.Setenv("OMNIROUTE_API_KEY", "")
	t.Setenv("TOKTOP_BEARER", "")

	t.Run("defaults name interval and ingest", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420"}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
		got := buf.String()
		if !strings.Contains(got, "interval=1s") || !strings.Contains(got, "ingest=127.0.0.1:8420") {
			t.Fatalf("logActiveConfig() = %q, want interval and ingest", got)
		}
	})
	t.Run("no-ingest is named off", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, noIngest: true}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
		if !strings.Contains(buf.String(), "ingest=off") {
			t.Fatalf("logActiveConfig() = %q, want ingest=off", buf.String())
		}
	})
	t.Run("bearer value is never printed", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", bearer: "sk-secret"}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{"bearer": true}, 1, 0, false)
		got := buf.String()
		if strings.Contains(got, "sk-secret") {
			t.Fatalf("logActiveConfig() leaked bearer: %q", got)
		}
		if !strings.Contains(got, "bearer=set") {
			t.Fatalf("logActiveConfig() = %q, want bearer=set", got)
		}
	})
	t.Run("unused bearer is omitted", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", bearer: "sk-secret"}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{"bearer": true}, 0, 0, false)
		got := buf.String()
		if strings.Contains(got, "bearer") || strings.Contains(got, "sk-secret") {
			t.Fatalf("logActiveConfig() = %q, want no bearer without --add", got)
		}
	})
	// The startup line must name opencode only when its gate actually
	// resolved: the flag defaults on, but a build without the sqlite driver
	// reads nothing, and claiming otherwise would misreport the knob.
	t.Run("opencode named only when its gate resolved", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", agents: true, opencode: true}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, true)
		if got := buf.String(); !strings.Contains(got, " agents opencode-db") {
			t.Fatalf("logActiveConfig() = %q, want agents opencode-db", got)
		}
		buf.Reset()
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
		if got := buf.String(); strings.Contains(got, "opencode-db") {
			t.Fatalf("logActiveConfig() = %q, want no opencode-db when the gate did not resolve", got)
		}
	})
}

// --opencode-db is on by default: --agents reads opencode's store without
// being asked, and --opencode-db=false is the opt-out.
func TestOpenCodeDBDefaultsOn(t *testing.T) {
	if f := registerFlags(); !f.opencode {
		t.Fatal("--opencode-db must default to true")
	}
}

func TestResolveBearer(t *testing.T) {
	t.Setenv("OMNIROUTE_API_KEY", "omni")
	t.Setenv("TOKTOP_BEARER", "tok")

	if got := resolveBearer("flag", true); got != "flag" {
		t.Errorf("explicit flag = %q, want flag", got)
	}
	if got := resolveBearer("", true); got != "" {
		t.Errorf("explicit empty = %q, want empty (must not fall through)", got)
	}
	if got := resolveBearer("", false); got != "omni" {
		t.Errorf("env fallback = %q, want omni first", got)
	}
	t.Setenv("OMNIROUTE_API_KEY", "")
	if got := resolveBearer("", false); got != "tok" {
		t.Errorf("second env = %q, want tok", got)
	}
	t.Setenv("TOKTOP_BEARER", "")
	if got := resolveBearer("", false); got != "" {
		t.Errorf("unset = %q, want empty", got)
	}
}

func TestWarnBearerFlag(t *testing.T) {
	t.Run("unset is silent", func(t *testing.T) {
		t.Setenv("TOKTOP_BEARER", "x")
		if got := captureStderr(t, func() { warnBearerFlag(false, "") }); got != "" {
			t.Fatalf("unset printed %q", got)
		}
	})
	t.Run("token on argv warns", func(t *testing.T) {
		got := captureStderr(t, func() { warnBearerFlag(true, "sk") })
		if !strings.Contains(got, "process listings") {
			t.Fatalf("printed %q, want process listings", got)
		}
	})
	t.Run("empty overrides env", func(t *testing.T) {
		t.Setenv("TOKTOP_BEARER", "x")
		t.Setenv("OMNIROUTE_API_KEY", "")
		got := captureStderr(t, func() { warnBearerFlag(true, "") })
		if !strings.Contains(got, "empty --bearer") {
			t.Fatalf("printed %q, want empty --bearer", got)
		}
	})
	t.Run("empty with no env is silent", func(t *testing.T) {
		t.Setenv("TOKTOP_BEARER", "")
		t.Setenv("OMNIROUTE_API_KEY", "")
		if got := captureStderr(t, func() { warnBearerFlag(true, "") }); got != "" {
			t.Fatalf("printed %q, want silence", got)
		}
	})
}

func TestRoutableBind(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8420", false},
		{"[::1]:8420", false},
		{"localhost:8420", false}, // resolved form is what Addr reports; a literal name errs into quiet
		{"notanip:8420", false},
		{"invalid-no-port", false},
		{"", false},
		{":8420", true},
		{"0.0.0.0:8420", true},
		{"[::]:8420", true},
		{"192.168.1.7:8420", true},
	}
	for _, tt := range tests {
		if got := routableBind(tt.addr); got != tt.want {
			t.Errorf("routableBind(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
