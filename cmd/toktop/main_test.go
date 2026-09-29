package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/selfupdate"
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

// The help screen promises exit 0 "including a reader such as head closing
// stdout early". Go's runtime re-raises SIGPIPE with its default disposition
// for a write to stdout, so a reader that exits before the write returned
// killed toktop with signal 13 (141 to a shell) and the documented contract
// never held. This drives the real binary through a shell pipeline, which is
// the only way to observe the signal.
func TestPipingToAnEarlyExitingReaderExitsZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/true to close the pipe early on Windows")
	}
	bin := filepath.Join(t.TempDir(), "toktop")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	for _, args := range [][]string{
		{"version"},
		{"--help"},
		{"--version"},
		{"update", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// PIPESTATUS[0] is toktop's status; a bare pipeline would report
			// the reader's, which is always 0 and would prove nothing.
			script := `"` + bin + `" ` + strings.Join(args, " ") + ` | true; exit ${PIPESTATUS[0]}`
			out, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s | true = %v, want exit 0\n%s", script, err, out)
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
					code = runOnce(context.Background(), w, cfg, ch, 1, plain, false)
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

// --json replaces the frame with the snapshot itself, and what lands on
// stdout has to parse: the mode exists for a script reading it, so a valid
// object (and nothing else) is the contract.
func TestRunOnceJSON(t *testing.T) {
	cfg := ui.Config{Version: "test", PollEvery: time.Second}
	snap := core.Snapshot{
		Uptime: 7 * time.Second,
		Providers: []core.ProviderSnapshot{
			{Label: "engine-a", Addr: "127.0.0.1:8000", OK: true, OutTokPS: 12.5},
			{Label: "engine-b", Addr: "127.0.0.1:8001"},
		},
	}
	ch := make(chan core.Snapshot, 1)
	ch <- snap
	var out bytes.Buffer
	var code int
	stderr := captureStderr(t, func() {
		code = runOnce(context.Background(), &out, cfg, ch, 1, false, true)
	})
	if code != 0 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q; want 0 and silence", code, stderr)
	}
	var got struct {
		Version   string `json:"version"`
		EnginesUp int    `json:"engines_up"`
		Engines   []struct {
			Label    string  `json:"label"`
			OK       bool    `json:"ok"`
			OutTokPS float64 `json:"out_tok_per_s"`
		} `json:"engines"`
		Agents []any `json:"agents"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	if got.Version != "test" || got.EnginesUp != 1 || len(got.Engines) != 2 {
		t.Fatalf("decoded %+v, want version test, 1 engine up and 2 engines", got)
	}
	if got.Engines[0].Label != "engine-a" || !got.Engines[0].OK || got.Engines[0].OutTokPS != 12.5 {
		t.Fatalf("decoded engine[0] = %+v, want engine-a up at 12.5 tok/s", got.Engines[0])
	}
	if got.Agents == nil {
		t.Error("agents key missing, want an empty list so jq '.agents[]' does not fail")
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
		{name: "below floor rejected", interval: intervalMin - time.Millisecond, wantErr: "--interval"},
		{name: "floor accepted", interval: intervalMin},
		{name: "cap accepted", interval: intervalMax},
		{name: "above cap rejected", interval: intervalMax + time.Second, wantErr: "--interval"},
		{name: "negative probe rejected", interval: time.Second, probeSecs: -5, wantErr: "--probe"},
		{name: "probe at cap accepted", interval: time.Second, probeSecs: probeSecsMax},
		{name: "probe above cap rejected", interval: time.Second, probeSecs: probeSecsMax + 1, wantErr: "--probe"},
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
	// The pipe holds roughly 64 KiB. Reading only after f returns wedges the
	// whole suite on any help screen that outgrows the buffer, so drain it
	// concurrently.
	out := make(chan string, 1)
	go func() {
		b, err := io.ReadAll(r)
		if err != nil {
			out <- ""
			return
		}
		out <- string(b)
	}()
	f()
	w.Close()
	s := <-out
	r.Close()
	return s
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
	t.Run("several names come out sorted", func(t *testing.T) {
		t.Setenv("TOKTOP_ZULU", "x")
		t.Setenv("TOKTOP_ALPHA", "x")
		got := captureWarnUnknownEnv(t)
		want := "TOKTOP_ALPHA, TOKTOP_ZULU"
		if !strings.Contains(got, want) {
			t.Fatalf("warnUnknownEnv() printed %q, want %q in that order", got, want)
		}
	})
}

func TestFrameEnv(t *testing.T) {
	t.Run("trimmed value is used", func(t *testing.T) {
		t.Setenv("TOKTOP_COLUMNS", " 120 ")
		n, set, err := frameEnv("TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax)
		if err != nil || !set || n != 120 {
			t.Fatalf("frameEnv() = %d, %v, %v, want 120, true, nil", n, set, err)
		}
	})
	t.Run("unset is not set", func(t *testing.T) {
		t.Setenv("TOKTOP_COLUMNS", "")
		n, set, err := frameEnv("TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax)
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
		{name: "caps accepted", columns: strconv.Itoa(frameColumnsMax), lines: strconv.Itoa(frameLinesMax)},
		{name: "whitespace trimmed", columns: " 120 ", lines: " 38 "},
		{name: "below floor rejected", columns: "40", wantErr: "TOKTOP_COLUMNS"},
		{name: "above cap rejected", columns: strconv.Itoa(frameColumnsMax + 1), wantErr: "TOKTOP_COLUMNS"},
		{name: "lines above cap rejected", lines: strconv.Itoa(frameLinesMax + 1), wantErr: "TOKTOP_LINES"},
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

// A GAUNTLET_HOME that cannot be used is named at startup under --agents:
// nothing else would report it, and the agents.json it names is never read.
func TestWarnIgnoredGauntletHome(t *testing.T) {
	// t.TempDir is absolute on every platform; a hand-built "/srv/gauntlet"
	// is drive-relative on Windows, where the warning is then correct.
	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, "agents.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		agents     bool
		gauntlet   string
		wantStderr string
	}{
		{name: "unset passes", agents: true},
		{name: "absolute with definitions passes", agents: true, gauntlet: populated},
		{name: "absolute without definitions is named", agents: true, gauntlet: t.TempDir(), wantStderr: "no agent definitions"},
		{name: "relative is named", agents: true, gauntlet: "gauntlet", wantStderr: "$GAUNTLET_HOME"},
		{name: "not read without agents", gauntlet: "gauntlet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GAUNTLET_HOME", tt.gauntlet)
			got := captureStderr(t, func() { warnIgnoredGauntletHome(tt.agents) })
			if tt.wantStderr == "" {
				if got != "" {
					t.Fatalf("warnIgnoredGauntletHome() printed %q, want silence", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantStderr) {
				t.Fatalf("warnIgnoredGauntletHome() printed %q, want mention of %q", got, tt.wantStderr)
			}
		})
	}
}

// An XDG base directory that cannot be used is named at startup wherever it
// would have been read: the same rule GAUNTLET_HOME follows, applied to the
// variables that otherwise fall back to a default directory in silence.
func TestWarnIgnoredXDGHome(t *testing.T) {
	// t.TempDir is absolute on every platform; a hand-built "/srv/xdg" is
	// drive-relative on Windows, where the warning is then correct.
	abs := t.TempDir()
	kimiHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(kimiHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		opencodeDB bool
		sshTarget  bool
		agents     bool
		dataHome   string
		configHome string
		kimiHome   string
		wantStderr []string
	}{
		{name: "unset passes", opencodeDB: true, sshTarget: true, agents: true},
		{name: "absolute passes", opencodeDB: true, sshTarget: true, agents: true, dataHome: abs, configHome: abs, kimiHome: kimiHome},
		{name: "relative data home is named", opencodeDB: true, dataHome: "share", wantStderr: []string{"$XDG_DATA_HOME"}},
		{name: "relative config home is named", sshTarget: true, configHome: "cfg", wantStderr: []string{"$XDG_CONFIG_HOME"}},
		{name: "relative kimi home is named", agents: true, kimiHome: "kimi", wantStderr: []string{"$KIMI_CODE_HOME"}},
		{
			name: "both are named", opencodeDB: true, sshTarget: true,
			dataHome: "share", configHome: "cfg", wantStderr: []string{"$XDG_DATA_HOME", "$XDG_CONFIG_HOME"},
		},
		{name: "data home unread without the opencode database", sshTarget: true, dataHome: "share"},
		{name: "config home unread without an ssh target", opencodeDB: true, configHome: "cfg"},
		{name: "kimi home unread without --agents", kimiHome: "kimi"},
		// An absolute home with no sessions under it is named the way
		// GAUNTLET_HOME names a missing agents.json: kimi never ran there, and
		// every session would read as an agent producing no tokens. t.TempDir
		// is absolute on every platform and exists with no sessions, so it is
		// the missing-store case without a hand-built path.
		{name: "absolute kimi home with no store is named", agents: true, kimiHome: abs, wantStderr: []string{"$KIMI_CODE_HOME"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", tt.dataHome)
			t.Setenv("XDG_CONFIG_HOME", tt.configHome)
			t.Setenv("KIMI_CODE_HOME", tt.kimiHome)
			got := captureStderr(t, func() { warnIgnoredXDGHome(tt.opencodeDB, tt.sshTarget, tt.agents) })
			if len(tt.wantStderr) == 0 {
				if got != "" {
					t.Fatalf("warnIgnoredXDGHome() printed %q, want silence", got)
				}
				return
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(got, want) {
					t.Fatalf("warnIgnoredXDGHome() printed %q, want mention of %q", got, want)
				}
			}
		})
	}
}

// A home the built-in stores cannot be built from is named at startup, next to
// the definitions file that did resolve: without it every compiled-in agent
// reports no tokens and nothing else says why.
func TestWarnIgnoredUserHome(t *testing.T) {
	t.Run("absolute home is silent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
		if got := captureStderr(t, func() { warnIgnoredUserHome() }); got != "" {
			t.Fatalf("warnIgnoredUserHome() printed %q, want silence", got)
		}
	})

	t.Run("relative home is named", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join("relative", "home"))
		t.Setenv("USERPROFILE", filepath.Join("relative", "home"))
		got := captureStderr(t, func() { warnIgnoredUserHome() })
		if !strings.Contains(got, "not an absolute path") {
			t.Fatalf("warnIgnoredUserHome() printed %q, want the relative home named", got)
		}
	})

	t.Run("unlocatable home is named", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		got := captureStderr(t, func() { warnIgnoredUserHome() })
		if !strings.Contains(got, "cannot be located") {
			t.Fatalf("warnIgnoredUserHome() printed %q, want the cause named", got)
		}
	})
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

	t.Run("an unread usage key is named, not refused", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", writeAgentsJSON(t,
			`{"deftest-agent":{"usage":{"roots":["~/.deftest/sessions"],"sufix":".jsonl"}}}`))
		var err error
		got := captureStderr(t, func() { err = loadAgentDefs() })
		if err != nil {
			t.Fatalf("loadAgentDefs() = %v, want nil: an unknown key is a version this build is older than", err)
		}
		if !strings.Contains(got, "deftest-agent: sufix") {
			t.Fatalf("loadAgentDefs() printed %q, want the unknown key named", got)
		}
		if !strings.Contains(got, "header_cwd") {
			t.Fatalf("loadAgentDefs() printed %q, want the keys this build reads", got)
		}
	})

	t.Run("no home directory is an error, not a silent skip", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", "")
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		err := loadAgentDefs()
		if err == nil {
			t.Fatal("loadAgentDefs() = nil, want an error when the home directory cannot be found")
		}
		if !strings.Contains(err.Error(), "home directory") {
			t.Fatalf("loadAgentDefs() = %v, want the cause named", err)
		}
	})
}

func TestUsage(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	got := buf.String()
	for _, want := range []string{
		"toktop -",                         // what the tool is
		"Usage:",                           // invocation line
		"[ssh://user@host ...",             // positional targets documented
		"toktop help",                      // git-style help command
		"toktop version",                   // git-style version command
		"help [update|version|completion]", // the extra commands are help topics
		"Examples:",                        // worked examples section
		"-demo",                            // generated flag docs survive
		"-interval",                        // PrintDefaults, not just the examples
		"-add",                             // repeatable backend flag
		"1s or 500ms",                      // --interval names the duration format
		"min 50ms",                         // --interval floor (bare numbers are nanoseconds)
		"OMNIROUTE_API_KEY",                // env fallbacks named
		"--add",                            // http(s) leftovers hint at --add
		"userinfo",                         // --add must not embed credentials
		"TOKTOP_SSH_PASSWORD",              // ssh URL must not embed a password
		"ssh://[user@]host",                // ssh target shape
		"host:port",                        // --ingest listen address shape
		"piping",                           // --once is the non-TTY path
		"needs a terminal",                 // live dashboard vs --once
		"Exit codes:",                      // the scripting contract
		"130",                              // Ctrl+C
		"stderr",                           // status never lands on stdout
		"Environment",                      // env table lives in --help, not only the README
		"TOKTOP_LOG_LEVEL",                 // the env vars a run actually reads
		"TOKTOP_COLUMNS",                   // --once frame overrides
		"a flag always wins",               // flag beats the variable it mirrors
	} {
		if !strings.Contains(got, want) {
			t.Errorf("usage() missing %q", want)
		}
	}
}

// The Environment block names every variable this binary reads, so --help is
// a complete reference on its own. A knob only the README mentions is one a
// user learns by reading source, and a new variable added to the code without
// a row here fails this test rather than shipping undocumented.
func TestUsageDocumentsEveryEnvVar(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	got := buf.String()
	for _, name := range []string{
		"OMNIROUTE_API_KEY",
		"TOKTOP_BEARER",
		"TOKTOP_SSH_PASSWORD",
		"TOKTOP_COLUMNS",
		"TOKTOP_LINES",
		"TOKTOP_LOG_LEVEL",
		"GAUNTLET_HOME",
		"XDG_DATA_HOME",
		"XDG_CONFIG_HOME",
		"GITHUB_TOKEN",
		"SSH_AUTH_SOCK",
		"NO_COLOR",
	} {
		if !strings.Contains(got, name) {
			t.Errorf("usage() Environment block does not document $%s", name)
		}
	}
}

// Every flag is documented in the same long form the examples, the prose and
// the README use, with its argument word matching the README's flag table.
// Go's PrintDefaults would print "-add value" and a separate "-h" line, which
// read as a different CLI from the rest of the screen.
func TestUsageDocumentsFlagsInLongForm(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	got := buf.String()

	for _, want := range []string{
		"--add URL", "--bearer TOKEN", "--frames N", "--ingest ADDR",
		"--interval D", "--json", "--probe N", "--seed N", "--ssh-key PATH",
		"--help, -h",
	} {
		if !strings.Contains(got, "\n  "+want+"\n") {
			t.Errorf("usage() flag list missing entry %q", want)
		}
	}
	// The short spelling belongs beside its long form, never as its own entry.
	if strings.Contains(got, "\n  -h\n") {
		t.Error("usage() lists -h as a flag of its own")
	}
	// No entry may fall back to Go's single-dash spelling. Scoped to the flag
	// list, since under `go test` flag.CommandLine also carries the test
	// binary's own --test.* flags.
	for _, line := range strings.Split(flagSection(t, got), "\n") {
		if strings.HasPrefix(line, "  -") && !strings.HasPrefix(line, "  --") {
			t.Errorf("usage() has a single-dash flag entry: %q", line)
		}
	}
}

// defaultDoc appends "(default X)" to every flag whose default is not the
// zero value, so a description that also states the default prints it twice
// and in two spellings ("default on" beside "default true").
func TestFlagDefaultStatedOnce(t *testing.T) {
	registerFlags()
	topFS.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Usage, "default") {
			t.Errorf("--%s states its default in the description, which defaultDoc appends again: %q",
				f.Name, f.Usage)
		}
	})
}

// Each flag's description hangs under the flag name by spaces. Go's
// PrintDefaults indents it with a tab, which lands every description on a
// tab stop instead of under the two-space flag column.
func TestFlagDescriptionsIndentWithSpaces(t *testing.T) {
	var buf strings.Builder
	usage(&buf)
	section := flagSection(t, buf.String())
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "  --") || line == "" {
			continue
		}
		if strings.ContainsRune(line, '\t') {
			t.Errorf("usage() description line is tab-indented: %q", line)
		}
	}
}

// A parse failure must name the flag the way the help screen and the README
// spell it. The flag package reports its own single-dash form, which is not
// the spelling shown anywhere else and reads as a different flag.
func TestFlagParseErrorUsesLongForm(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "unknown flag",
			in:   "flag provided but not defined: -bogus",
			want: "flag provided but not defined: --bogus",
		},
		{
			name: "unparseable value",
			in:   `invalid value "1" for flag -interval: parse error`,
			want: `invalid value "1" for flag --interval: parse error`,
		},
		{
			name: "non-boolean value for a boolean",
			in:   `invalid boolean value "x" for -demo: parse error`,
			want: `invalid boolean value "x" for flag --demo: parse error`,
		},
		{
			name: "missing value",
			in:   "flag needs an argument: -ingest",
			want: "flag needs an argument: --ingest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := flagParseError(errors.New(tc.in)); got != tc.want {
				t.Errorf("flagParseError(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A single-letter alias the user typed keeps its single dash: "-h" is how it
// was written, and "---h" would name a flag that does not exist.
func TestLongFlagKeepsOneLetterAlias(t *testing.T) {
	if got := longFlag("-h"); got != "--h" {
		t.Errorf("longFlag(-h) = %q, want %q", got, "--h")
	}
}

// A flag that belongs to a subcommand is answered with the command line that
// would work, the way a misplaced subcommand word is.
func TestSubcommandFlagHintNamesTheSubcommand(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "update flag, long spelling",
			in:   "flag provided but not defined: --check",
			want: " (toktop update --check)",
		},
		{
			name: "update flag, the single-dash spelling the package also parses",
			in:   "flag provided but not defined: -repo",
			want: " (toktop update --repo owner/name)",
		},
		{
			name: "a typo is not a misplaced flag",
			in:   "flag provided but not defined: --chek",
			want: "",
		},
		{
			name: "a flag the top level does declare",
			in:   "flag provided but not defined: --demo",
			want: "",
		},
		{
			name: "a failure that is not an unknown flag",
			in:   `invalid value "abc" for flag -frames: parse error`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subcommandFlagHint(errors.New(tc.in)); got != tc.want {
				t.Errorf("subcommandFlagHint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A value the flag package rejected as unparseable is told what it should have
// been. "parse error" names neither the expectation nor a value that would
// work, and every numeric flag reported it identically.
func TestValueHintNamesTheExpectedValue(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "whole number flag",
			in:   `invalid value "abc" for flag -frames: parse error`,
			want: " (expected a whole number of snapshots, 1-180)",
		},
		{
			name: "seconds flag",
			in:   `invalid value "1.5" for flag -probe: parse error`,
			want: " (expected a whole number of seconds, 0-86400)",
		},
		{
			name: "seed flag",
			in:   `invalid value "1.5" for flag -seed: parse error`,
			want: " (expected a whole number)",
		},
		{
			name: "duration with a misspelled unit",
			in:   `invalid value "1x" for flag -interval: parse error`,
			want: " (expected a Go duration such as 1s or 500ms)",
		},
		{
			name: "duration as a bare number is read as nanoseconds",
			in:   `invalid value "1" for flag -interval: parse error`,
			want: " (a bare number is nanoseconds; use 1s or 500ms)",
		},
		{
			name: "a flag's own error already says what is wrong",
			in:   `invalid value "ftp://x" for flag -add: URL must be http:// or https://`,
			want: "",
		},
		{
			name: "a flag not in the table",
			in:   `invalid value "x" for flag -ssh-key: parse error`,
			want: "",
		},
		{
			name: "an unknown flag names no value",
			in:   "flag provided but not defined: -bogus",
			want: "",
		},
		{
			name: "a non-boolean value is a different failure",
			in:   `invalid boolean value "x" for -demo: parse error`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := valueHint(errors.New(tc.in)); got != tc.want {
				t.Errorf("valueHint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// flagSection returns the lines between the "Flags:" heading and the prose
// that follows the list.
func flagSection(t *testing.T, help string) string {
	t.Helper()
	_, rest, ok := strings.Cut(help, "\nFlags:\n")
	if !ok {
		t.Fatal("help has no Flags: section")
	}
	section, _, _ := strings.Cut(rest, "\n\n")
	return section
}

// toktop update documents its flags the same way the top-level command does.
func TestUpdateUsageDocumentsFlagsInLongForm(t *testing.T) {
	var out bytes.Buffer
	if code := runUpdate(context.Background(), &out, []string{"--help"}); code != 0 {
		t.Fatalf("runUpdate(--help) = %d, want 0", code)
	}
	// The argument word matches the "Usage:" line above it (owner/name), not
	// the type name the flag package would print ("string").
	for _, want := range []string{"--check", "--repo owner/name", "--help, -h"} {
		if !strings.Contains(out.String(), "\n  "+want+"\n") {
			t.Errorf("update usage flag list missing entry %q", want)
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
			for _, want := range []string{"Usage:", "--check", "--repo", "owner/name", "--version", "GITHUB_TOKEN", "Examples:", "url=$(toktop update --check)"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("update help missing %q", want)
				}
			}
		})
	}
}

// `toktop update --check` is pipeable: stdout is the release URL and nothing
// else, whether or not the running binary is already current. The status lines
// that describe the comparison are stderr, and only a newer release asks for
// an install to follow.
func TestReportRelease(t *testing.T) {
	const url = "https://github.com/maci0/toktop/releases/tag/v9.9.9"
	newer := &selfupdate.Release{TagName: "v9.9.9", HTMLURL: url}
	current := &selfupdate.Release{TagName: "v" + version, HTMLURL: url}
	for _, tt := range []struct {
		name       string
		rel        *selfupdate.Release
		check      bool
		wantStdout string
		wantStderr []string
		wantNewer  bool
	}{
		{
			name:       "check newer",
			rel:        newer,
			check:      true,
			wantStdout: url + "\n",
			wantStderr: []string{"New release: v9.9.9", version},
		},
		{
			name:       "check current",
			rel:        current,
			check:      true,
			wantStdout: url + "\n",
			wantStderr: []string{"is current", version},
		},
		{
			name:       "install newer",
			rel:        newer,
			wantStdout: "New release: v9.9.9 (running " + version + ")\n",
			wantNewer:  true,
		},
		{
			name:       "install current",
			rel:        current,
			wantStdout: "toktop " + version + " is current (latest release: v" + version + ")\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, status bytes.Buffer
			code, newer := reportRelease(&out, &status, tt.rel, tt.check)
			if code != 0 {
				t.Fatalf("reportRelease = %d, want 0", code)
			}
			if newer != tt.wantNewer {
				t.Errorf("newer = %v, want %v", newer, tt.wantNewer)
			}
			if got := out.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(status.String(), want) {
					t.Errorf("stderr = %q, want mention of %q", status.String(), want)
				}
			}
		})
	}
}

func TestRunUpdateUsageErrors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantSub  string // required substring on stderr
		wantSubs []string
	}{
		{name: "unknown flag", args: []string{"--bogus"}, wantSub: "flag provided but not defined"},
		{name: "unexpected argument", args: []string{"extra"}, wantSub: "unexpected argument"},
		{name: "unexpected argument points at help", args: []string{"extra"}, wantSub: "toktop update --help"},
		// A bad --repo is a usage error like any other, so it names the flag
		// in the long form the help screen documents and prints that screen,
		// the way an unparseable --check value does.
		{name: "repo path traversal", args: []string{"--repo", "maci0/toktop/../../../users/octocat"},
			wantSubs: []string{`--repo "maci0/toktop/../../../users/octocat" must be owner/name`, "Usage:"}},
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
			for _, want := range append([]string{tt.wantSub}, tt.wantSubs...) {
				if want == "" {
					continue
				}
				if !strings.Contains(got, want) {
					t.Fatalf("stderr = %q, want mention of %q", got, want)
				}
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
		plain    bool
		agents   bool
		noIngest bool
		jsonOut  bool
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
		{name: "frames with plain is explained", set: map[string]bool{"frames": true}, once: true, plain: true,
			wantSub: "--frames only sets how long --once waits with --plain"},
		{name: "frames with json is explained", set: map[string]bool{"frames": true}, once: true, jsonOut: true,
			wantSub: "--frames only sets how long --once waits with --json"},
		{name: "frames with json names the json report", set: map[string]bool{"frames": true}, once: true, jsonOut: true,
			wantSub: "the JSON report renders the last snapshot"},
		{name: "both no-ops warn twice", set: map[string]bool{"seed": true, "frames": true},
			wantSub: "--seed", wantAlso: "--frames"},
		{name: "opencode-db without agents warns", set: map[string]bool{"opencode-db": true},
			wantSub: "--opencode-db"},
		{name: "opencode-db with agents silent", set: map[string]bool{"opencode-db": true}, agents: true},
		// --plain on its own is the live text report, not a no-op: a flag that
		// warned here would send a screen-reader user back to the drawn frame
		// they cannot read.
		{name: "plain outside once silent", set: map[string]bool{"plain": true}},
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
		{name: "json outside once warns", set: map[string]bool{"json": true}, wantSub: "--json"},
		{name: "json inside once silent", set: map[string]bool{"json": true}, once: true},
		{name: "json with plain is explained", set: map[string]bool{"json": true}, once: true, plain: true, jsonOut: true,
			wantSub: "--plain has no effect with --json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureStderr(t, func() {
				f := &cliFlags{
					demo: tt.demo, once: tt.once, plain: tt.plain,
					agents: tt.agents, noIngest: tt.noIngest, jsonOut: tt.jsonOut,
				}
				warnIgnoredFlags(tt.set, f, tt.nAdd, tt.nRemote)
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
	// Distinct uptimes per frame, so returning the first instead of the last,
	// or reading a single frame and returning nil, cannot pass.
	mark := core.Snapshot{Uptime: 7 * time.Second}
	t.Run("collects the requested frames", func(t *testing.T) {
		ch := make(chan core.Snapshot, 2)
		ch <- mark
		ch <- core.Snapshot{Uptime: 9 * time.Second}
		got, err := waitForFrames(context.Background(), ch, 2, time.Second)
		if err != nil || got.Uptime != 9*time.Second {
			t.Fatalf("waitForFrames() = %+v, %v; want the last of two frames, nil", got, err)
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
		plain   bool
		jsonOut bool
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
		{name: "with plain warns about the report", once: true, plain: true, columns: "120",
			wantSub: "$TOKTOP_COLUMNS has no effect with --plain"},
		{name: "with json warns about the report", once: true, jsonOut: true, columns: "120",
			wantSub: "$TOKTOP_COLUMNS has no effect with --json"},
		{name: "with json and plain names the text report", once: true, plain: true, jsonOut: true, lines: "38",
			wantSub: "$TOKTOP_LINES has no effect with --plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_COLUMNS", tt.columns)
			t.Setenv("TOKTOP_LINES", tt.lines)
			got := captureStderr(t, func() { warnIgnoredFrameEnv(tt.once, tt.plain, tt.jsonOut) })
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
		agents     bool
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
		{name: "log level with demo and no-ingest warns", logLevel: "warn", demo: true, noIngest: true, wantSub: "TOKTOP_LOG_LEVEL"},
		{name: "log level with demo, no-ingest and agents silent", logLevel: "warn", demo: true, noIngest: true, agents: true},
		{name: "log level with no-ingest but a collector silent", logLevel: "warn", noIngest: true},
		{name: "log level with ingest silent", logLevel: "warn"},
		{name: "blank log level is the unset default", logLevel: "  ", demo: true, noIngest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TOKTOP_SSH_PASSWORD", tt.sshPass)
			t.Setenv("OMNIROUTE_API_KEY", tt.omni)
			t.Setenv("TOKTOP_BEARER", tt.bearer)
			t.Setenv("TOKTOP_LOG_LEVEL", tt.logLevel)
			got := captureStderr(t, func() {
				warnUnusedEnv(tt.bearerFlag, tt.demo, tt.noIngest, tt.agents, tt.nAdd, tt.nRemote)
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

// The live dashboard quits on q and Ctrl+C at 0, but a SIGTERM reaches it
// through the run context instead: main's NotifyContext replaced that signal's
// default disposition, so a process that ignored the context stayed on screen
// with every backend canceled behind it. The signal is the same one --once and
// toktop update already exit 130 on, and the help screen documents it.
func TestTUIExitOnCanceledContext(t *testing.T) {
	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	if code, settled := tuiExit(live); settled || code != 0 {
		t.Fatalf("tuiExit(live ctx) = %d, %t; want 0, false", code, settled)
	}
	cancelLive()
	code, settled := tuiExit(live)
	if !settled || code != 130 {
		t.Fatalf("tuiExit(canceled ctx) = %d, %t; want 130, true", code, settled)
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
		{"update short version", func(w io.Writer, args []string) int {
			return runUpdate(context.Background(), w, args)
		}, []string{"-v", "extra"}},
		{"help version", runHelp, []string{"--version", "extra"}},
		{"help short version", runHelp, []string{"-v", "extra"}},
		{"version short version", runVersion, []string{"-v", "extra"}},
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
	t.Run("version flag prints the version on every command", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			run  func(io.Writer, []string) int
			args []string
		}{
			{"help", runHelp, []string{"--version"}},
			{"help short", runHelp, []string{"-v"}},
			{"version", runVersion, []string{"-v"}},
			{"update", func(w io.Writer, args []string) int {
				return runUpdate(context.Background(), w, args)
			}, []string{"-v"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				var out bytes.Buffer
				var code int
				got := captureStderr(t, func() { code = tt.run(&out, tt.args) })
				if code != 0 {
					t.Fatalf("= %d, want 0", code)
				}
				if got != "" {
					t.Fatalf("leaked %q to stderr", got)
				}
				if out.String() != "toktop "+version+"\n" {
					t.Fatalf("stdout = %q, want the version line", out.String())
				}
			})
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
		if !strings.Contains(got, "extra") || !strings.Contains(got, "toktop update --help") {
			t.Fatalf("stderr = %q, want extra and the update help", got)
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
		if !strings.Contains(got, "extra") || !strings.Contains(got, "toktop version --help") {
			t.Fatalf("stderr = %q, want extra and the version help", got)
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
	t.Run("--version prints the version", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runVersion(&out, []string{"--version"}) })
		if code != 0 {
			t.Fatalf("runVersion(--version) = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runVersion(--version) leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "toktop "+version) {
			t.Fatalf("stdout = %q, want toktop %s", out.String(), version)
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
		{name: "one ssh", args: []string{"ssh://user@box"}, wantN: 1},
		{name: "two ssh", args: []string{"ssh://a", "ssh://b"}, wantN: 2},
		{name: "http url hints --add", args: []string{"http://127.0.0.1:8000"}, wantErr: "--add"},
		{name: "https url hints --add", args: []string{"https://example:8000"}, wantErr: "--add"},
		{name: "bare word points at --help", args: []string{"helpme"}, wantErr: "toktop --help"},
		{name: "update not first", args: []string{"update"}, wantErr: "toktop update"},
		{name: "help not first", args: []string{"ssh://a", "help"}, wantErr: "toktop help"},
		{name: "version not first", args: []string{"ssh://a", "version"}, wantErr: "toktop version"},
		{name: "ssh then junk", args: []string{"ssh://a", "nope"}, wantErr: "toktop --help"},
		{name: "flag after ssh", args: []string{"ssh://a", "--add", "http://x"}, wantErr: `"--add" must come before the ssh:// targets`},
		{name: "help flag after ssh", args: []string{"ssh://a", "--help"}, wantErr: `"--help" must come before the ssh:// targets`},
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
		// A credential in the query is one the audit log, the dashboard and
		// both reports echo whole, and it never works: every request appends
		// a path to the base.
		{raw: "http://127.0.0.1:8000?api_key=sk-secret", wantErr: "query"},
		{raw: "http://127.0.0.1:8000/?", wantErr: "query"},
		{raw: "http://127.0.0.1:8000/v1#secret", wantErr: "fragment"},
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

// The same endpoint named twice would be polled twice and summed twice, so it
// is a usage error rather than a second provider.
func TestParseAddRejectsDuplicate(t *testing.T) {
	var adds []string
	if err := parseAdd("http://127.0.0.1:8000/v1", &adds); err != nil {
		t.Fatal(err)
	}
	for _, dup := range []string{"http://127.0.0.1:8000/v1", " http://127.0.0.1:8000/v1/ "} {
		if err := parseAdd(dup, &adds); err == nil {
			t.Fatalf("parseAdd(%q) = nil error, want rejection as a duplicate", dup)
		}
	}
	if err := parseAdd("http://127.0.0.1:8000/v2", &adds); err != nil {
		t.Fatalf("a distinct endpoint must be accepted: %v", err)
	}
	if got := strings.Join(adds, ","); got != "http://127.0.0.1:8000/v1,http://127.0.0.1:8000/v2" {
		t.Fatalf("adds = %q", got)
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

// swapConfigLog points the config audit record at a buffer the test can read,
// and returns the func that puts the process logger back.
func swapConfigLog(w io.Writer) func() {
	prev := configLog
	configLog = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return func() { configLog = prev }
}

func TestLogActiveConfig(t *testing.T) {
	// The prose line is what these subtests read; the audit record is read
	// only where a subtest asks for it, so the rest goes nowhere.
	configLog = func() *slog.Logger { return slog.New(slog.DiscardHandler) }
	t.Cleanup(func() { configLog = logcfg.Logger })
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
	t.Run("log level is named as it applies", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420"}
		var buf strings.Builder
		t.Setenv(logcfg.LevelEnv, "WARNING")
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
		if !strings.Contains(buf.String(), " log=warn") {
			t.Fatalf("logActiveConfig() = %q, want log=warn", buf.String())
		}
		buf.Reset()
		t.Setenv(logcfg.LevelEnv, "")
		logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
		if strings.Contains(buf.String(), " log=") {
			t.Fatalf("logActiveConfig() = %q, want no log= when the variable is unset", buf.String())
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
	// A capture run is reproduced from the startup line: the render is a
	// bitmap that says nothing about the size it was asked for, so an
	// override the frame took must be on the line. The two reports that
	// replace the frame make the override unreachable, and an unreachable
	// one is named by warnIgnoredFrameEnv rather than claimed here.
	t.Run("a frame override in force is named", func(t *testing.T) {
		t.Setenv("TOKTOP_COLUMNS", "100")
		t.Setenv("TOKTOP_LINES", "40")
		var buf strings.Builder
		logActiveConfig(&buf, &cliFlags{interval: time.Second, once: true}, map[string]bool{}, 0, 0, false)
		got := buf.String()
		if !strings.Contains(got, " columns=100") || !strings.Contains(got, " lines=40") {
			t.Fatalf("logActiveConfig() = %q, want the frame size", got)
		}
		for _, tc := range []struct {
			name  string
			flags cliFlags
		}{
			{"plain", cliFlags{interval: time.Second, once: true, plain: true}},
			{"json", cliFlags{interval: time.Second, once: true, jsonOut: true}},
			{"no --once", cliFlags{interval: time.Second}},
		} {
			buf.Reset()
			logActiveConfig(&buf, &tc.flags, map[string]bool{}, 0, 0, false)
			if got := buf.String(); strings.Contains(got, " columns=") || strings.Contains(got, " lines=") {
				t.Fatalf("logActiveConfig() with %s = %q, want no frame size named", tc.name, got)
			}
		}
	})
	// A demo run replays from its seed and, when the operator pinned one,
	// from its origin. A run that crashes leaves the audit log and no report,
	// so the line has to carry both or the run cannot be reproduced from what
	// survived it.
	t.Run("demo names the seed and the pinned origin", func(t *testing.T) {
		var buf strings.Builder
		logActiveConfig(&buf, &cliFlags{interval: time.Second, demo: true, seed: 7, origin: "2026-01-01T00:00:00Z"},
			map[string]bool{}, 0, 0, false)
		got := buf.String()
		if !strings.Contains(got, " seed=7") || !strings.Contains(got, " origin=2026-01-01T00:00:00Z") {
			t.Fatalf("logActiveConfig() = %q, want the demo seed and origin", got)
		}
		buf.Reset()
		logActiveConfig(&buf, &cliFlags{interval: time.Second, demo: true, seed: 42},
			map[string]bool{}, 0, 0, false)
		if got := buf.String(); !strings.Contains(got, " seed=42") || strings.Contains(got, " origin=") {
			t.Fatalf("logActiveConfig() = %q, want the seed alone on a run that started on the wall clock", got)
		}
		buf.Reset()
		logActiveConfig(&buf, &cliFlags{interval: time.Second, seed: 7},
			map[string]bool{}, 0, 0, false)
		if strings.Contains(buf.String(), "seed=") {
			t.Fatalf("logActiveConfig() = %q, want no seed outside a demo run", buf.String())
		}
	})
	// The line records the knobs that will actually apply, so a --plain that
	// --json replaced is not named: "once plain json" reads as two reports.
	t.Run("only the report that renders is named", func(t *testing.T) {
		for _, tc := range []struct {
			plain, jsonOut bool
			want           string
		}{
			{plain: true, want: "once plain"},
			{jsonOut: true, want: "once json"},
			{plain: true, jsonOut: true, want: "once json"},
		} {
			f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", once: true, plain: tc.plain, jsonOut: tc.jsonOut}
			var buf strings.Builder
			logActiveConfig(&buf, f, map[string]bool{}, 0, 0, false)
			got := buf.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("logActiveConfig(plain=%v, json=%v) = %q, want %q", tc.plain, tc.jsonOut, got, tc.want)
			}
			if tc.jsonOut && strings.Contains(got, "plain") {
				t.Errorf("logActiveConfig(plain=%v, json=%v) = %q, want no plain: the JSON report replaced it", tc.plain, tc.jsonOut, got)
			}
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
	// The live dashboard hides stderr under the alt screen, so the startup
	// line alone is gone the moment the run reaches steady state. The audit
	// record is the copy that outlives it, and it has to carry the same knobs
	// as fields: a run cannot be reconstructed from prose.
	t.Run("knobs reach the audit log as fields", func(t *testing.T) {
		t.Setenv(logcfg.LevelEnv, "warning")
		var auditBuf bytes.Buffer
		restore := swapConfigLog(&auditBuf)
		t.Cleanup(restore)
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:0", agents: true}
		logActiveConfig(io.Discard, f, map[string]bool{}, 0, 2, true)
		got := auditBuf.String()
		for _, want := range []string{
			"level=INFO", `msg="toktop: config"`,
			"interval=1s", "ingest=127.0.0.1:0", "log=warn",
			"agents=true", "opencode-db=true", "ssh=2",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("audit record %q, want %s", got, want)
			}
		}
	})
	// A secret reaches neither copy: an audit record naming a bearer value is a
	// credential on disk, and the record outlives the run.
	t.Run("bearer stays unnamed in the audit record", func(t *testing.T) {
		var auditBuf bytes.Buffer
		restore := swapConfigLog(&auditBuf)
		t.Cleanup(restore)
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", bearer: "sk-secret"}
		logActiveConfig(io.Discard, f, map[string]bool{"bearer": true}, 1, 0, false)
		got := auditBuf.String()
		if strings.Contains(got, "sk-secret") {
			t.Fatalf("audit record leaked bearer: %q", got)
		}
		if !strings.Contains(got, "bearer=set") {
			t.Fatalf("audit record %q, want bearer=set", got)
		}
	})
	// bearer.Set runs after the config line and turns down a token carrying
	// CR or LF, so the run queries the --add endpoints unauthenticated. A
	// record reading bearer=set there describes a credential that is not in
	// force, and the 401s it would explain arrive with nothing naming the
	// cause.
	t.Run("a refused token is named refused, not set", func(t *testing.T) {
		f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420", bearer: "sk-secret\nghp_other"}
		var buf strings.Builder
		logActiveConfig(&buf, f, map[string]bool{"bearer": true}, 1, 0, false)
		got := buf.String()
		if strings.Contains(got, "bearer=set") {
			t.Fatalf("logActiveConfig() = %q, want bearer=refused: the token is turned down at Set", got)
		}
		if !strings.Contains(got, "bearer=refused") {
			t.Fatalf("logActiveConfig() = %q, want bearer=refused", got)
		}
		if strings.Contains(got, "ghp_other") {
			t.Fatalf("logActiveConfig() leaked bearer: %q", got)
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

// A bearer variable holding nothing usable is a wrapper that failed to read
// its file, not a token: `export TOKTOP_BEARER=$(cat key)` on a missing file
// sets it to the empty string, and a token file with a trailing space trims
// away to nothing. Both used to win the precedence chain and authenticate
// nothing, while the startup line still read bearer=set.
func TestResolveBearerTrimsBlankValues(t *testing.T) {
	t.Setenv("OMNIROUTE_API_KEY", "omni")
	t.Setenv("TOKTOP_BEARER", "tok")

	if got := resolveBearer("  sk  ", true); got != "sk" {
		t.Errorf("whitespace-padded flag = %q, want sk", got)
	}
	t.Setenv("OMNIROUTE_API_KEY", "\n")
	if got := resolveBearer("", false); got != "tok" {
		t.Errorf("blank first env = %q, want the second source to win", got)
	}
	t.Setenv("TOKTOP_BEARER", "   ")
	if got := resolveBearer("", false); got != "" {
		t.Errorf("all blank = %q, want empty", got)
	}
	// The startup line reads the same resolution, so a blank variable cannot
	// leave it claiming bearer=set for a run that sends no token.
	f := &cliFlags{interval: time.Second, ingest: "127.0.0.1:8420"}
	var buf strings.Builder
	logActiveConfig(&buf, f, map[string]bool{}, 1, 0, false)
	if strings.Contains(buf.String(), "bearer") {
		t.Errorf("config line %q claims a bearer for blank env variables", buf.String())
	}
}

// A set-but-blank bearer variable is named where it can take effect: a --add
// endpoint attached and no --demo, which is the only run that sends a token.
// The wording follows the token actually in force, so a blank variable another
// source covered is not reported as leaving the run unauthenticated.
func TestWarnBlankBearer(t *testing.T) {
	tests := []struct {
		name       string
		omni       string
		setOmni    bool
		toktop     string
		setToktop  bool
		nAdd       int
		demo       bool
		inForce    bool
		wantStderr []string
		wantAbsent []string
	}{
		{name: "unset passes", nAdd: 1},
		{name: "a token is silent", omni: "sk", setOmni: true, nAdd: 1, inForce: true},
		{name: "no add endpoint passes", setToktop: true, nAdd: 0},
		{name: "demo passes", setToktop: true, nAdd: 1, demo: true},
		{name: "empty is named", setToktop: true, nAdd: 1, wantStderr: []string{"$TOKTOP_BEARER", "without a token"}},
		{name: "whitespace is named", omni: " \n", setOmni: true, nAdd: 1, wantStderr: []string{"$OMNIROUTE_API_KEY", "without a token"}},
		{name: "both blank are named", setOmni: true, nAdd: 1, setToktop: true, wantStderr: []string{"$OMNIROUTE_API_KEY", "TOKTOP_BEARER", "without a token"}},
		// The two sources the resolver falls through to: the sibling variable
		// and an explicit --bearer. Both leave the run authenticated, so
		// claiming it is not would send the operator after a 401 this run
		// never produces.
		{name: "blank source under a token from the next one", setOmni: true, nAdd: 1, inForce: true, wantStderr: []string{"$OMNIROUTE_API_KEY", "ignored"}, wantAbsent: []string{"without a token"}},
		{name: "blank source under a flag token", setToktop: true, nAdd: 1, inForce: true, wantStderr: []string{"$TOKTOP_BEARER", "another source"}, wantAbsent: []string{"without a token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, set := range map[string]bool{"OMNIROUTE_API_KEY": tt.setOmni, "TOKTOP_BEARER": tt.setToktop} {
				// Setenv marks the variable present, and a present-but-empty
				// one is the case this names, so the silent cases unset it
				// outright and the others set only the value under test.
				if !set {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Setenv(name, "") })
					continue
				}
				t.Setenv(name, map[string]string{"OMNIROUTE_API_KEY": tt.omni, "TOKTOP_BEARER": tt.toktop}[name])
			}
			got := captureStderr(t, func() { warnBlankBearer(tt.nAdd, tt.demo, tt.inForce) })
			for _, want := range tt.wantStderr {
				if !strings.Contains(got, want) {
					t.Errorf("warnBlankBearer() printed %q, want mention of %q", got, want)
				}
			}
			for _, unwanted := range tt.wantAbsent {
				if strings.Contains(got, unwanted) {
					t.Errorf("warnBlankBearer() printed %q, want no mention of %q", got, unwanted)
				}
			}
			if len(tt.wantStderr) == 0 && got != "" {
				t.Errorf("warnBlankBearer() printed %q, want silence", got)
			}
		})
	}
}

func TestValidateSSHKeyFlag(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		val     string
		wantErr string
	}{
		{name: "unset is fine", val: ""},
		{name: "a path is fine", set: true, val: "/home/u/.ssh/id_ed25519"},
		{name: "explicit empty rejected", set: true, val: "", wantErr: "not empty"},
		{name: "whitespace rejected", set: true, val: "   ", wantErr: "not empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSSHKeyFlag(tt.set, tt.val)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSSHKeyFlag(%v, %q) = %v, want nil", tt.set, tt.val, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateSSHKeyFlag(%v, %q) = %v, want %q", tt.set, tt.val, err, tt.wantErr)
			}
		})
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
		{"localhost:8420", false},
		{"localhost.:8420", false},
		{"box.internal:8420", true}, // a name is routable unless it says localhost
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

func TestWarnInsecureAdd(t *testing.T) {
	tests := []struct {
		name string
		adds []string
		want string
	}{
		{"plain http remote", []string{"http://gpu-box:8000/v1"}, "cleartext"},
		{"plain http local", []string{"http://127.0.0.1:11434/v1"}, ""},
		{"plain http localhost", []string{"http://localhost:8000/v1"}, ""},
		{"https remote", []string{"https://api.example.com/v1"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureStderr(t, func() { warnInsecureAdd(tt.adds) })
			if tt.want == "" {
				if got != "" {
					t.Fatalf("printed %q, want silence", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("printed %q, want %q", got, tt.want)
			}
		})
	}
}

// A $GITHUB_TOKEN that is set but blank sends no Authorization header, and the
// anonymous rate-limit error then advises setting the very variable the
// operator already set. It is named at startup so the misconfiguration reads
// as itself rather than as a first run that has never authenticated.
func TestWarnBlankGitHubToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		set   bool
		want  string
	}{
		{name: "unset is silent", set: false},
		{name: "empty is named", set: true, token: "", want: "is set but blank"},
		{name: "whitespace is named", set: true, token: "  \t", want: "is set but blank"},
		{name: "trailing newline alone is named", set: true, token: "\n", want: "is set but blank"},
		{name: "a token is silent", set: true, token: "ghp_x"},
		{name: "a token with a stripped newline is silent", set: true, token: "ghp_x\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// os.Unsetenv has no restoring counterpart, so the unset case
			// would take the caller's $GITHUB_TOKEN with it and every later
			// test in the package would see it gone.
			if prev, ok := os.LookupEnv(selfupdate.TokenEnv); ok {
				t.Cleanup(func() { _ = os.Setenv(selfupdate.TokenEnv, prev) })
			} else {
				t.Cleanup(func() { _ = os.Unsetenv(selfupdate.TokenEnv) })
			}
			if tt.set {
				t.Setenv(selfupdate.TokenEnv, tt.token)
			} else {
				os.Unsetenv(selfupdate.TokenEnv)
			}
			got := captureStderr(t, warnBlankGitHubToken)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("printed %q, want silence", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("printed %q, want %q", got, tt.want)
			}
		})
	}
}
