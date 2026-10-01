package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRunCompletion(t *testing.T) {
	t.Run("prints a script for each shell on stdout", func(t *testing.T) {
		for _, shell := range completionShells {
			t.Run(shell, func(t *testing.T) {
				var out bytes.Buffer
				var code int
				got := captureStderr(t, func() { code = runCompletion(&out, []string{shell}) })
				if code != 0 {
					t.Fatalf("runCompletion(%s) = %d, want 0", shell, code)
				}
				if got != "" {
					t.Fatalf("runCompletion(%s) leaked %q to stderr", shell, got)
				}
				if out.Len() == 0 {
					t.Fatalf("runCompletion(%s) wrote nothing to stdout", shell)
				}
			})
		}
	})
	t.Run("the bash script completes every top-level flag", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"bash"})
		// The word list the function hands to compgen, not the whole script:
		// a flag named anywhere in a comment would pass a substring test.
		list := bashWordList(t, out.String(), "topflags")
		for _, name := range topFlags() {
			if !slices.Contains(list, name) {
				t.Fatalf("bash word list omits %s: %v", name, list)
			}
		}
		if !strings.Contains(out.String(), `complete -F _toktop toktop`) {
			t.Fatalf("bash script never registers completion:\n%s", out.String())
		}
	})
	t.Run("the bash word lists stay disjoint per command", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"bash"})
		top := bashWordList(t, out.String(), "topflags")
		// --help and --version are declared on both commands and belong on
		// both word lists; the rest of update's flags must not leak upward.
		for _, name := range []string{"--check", "--repo"} {
			if slices.Contains(top, name) {
				t.Fatalf("bash word list for the top level offers update's %s: %v", name, top)
			}
		}
	})
	t.Run("the bash script registers the command name, not the binary's", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"bash"})
		// A test binary registers itself as toktop_test; a sourced script
		// carrying that name would be a file named after a test run.
		if strings.Contains(out.String(), "toktop_test") {
			t.Fatalf("bash script registers the test binary:\n%s", out.String())
		}
	})
	t.Run("a flag added to the FlagSet is completed", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"bash"})
		if !strings.Contains(out.String(), "--probe") {
			t.Fatalf("bash script omits the --probe flag:\n%s", out.String())
		}
	})
	t.Run("the zsh script carries the compdef header", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"zsh"})
		if !strings.HasPrefix(out.String(), "#compdef toktop\n") {
			t.Fatalf("zsh script missing the compdef header:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "compdef _toktop toktop") {
			t.Fatalf("zsh script never binds the function:\n%s", out.String())
		}
	})
	t.Run("each shell offers the subcommands", func(t *testing.T) {
		for _, shell := range completionShells {
			var out bytes.Buffer
			runCompletion(&out, []string{shell})
			for _, sub := range completionSubs {
				if !strings.Contains(out.String(), sub) {
					t.Fatalf("%s script omits the %s subcommand:\n%s", shell, sub, out.String())
				}
			}
		}
	})
	t.Run("a path flag gets file completion, and nothing else does", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"fish"})
		if !strings.Contains(out.String(), "complete -c toktop -l ssh-key -r -F") {
			t.Fatalf("fish script does not offer a file list for --ssh-key:\n%s", out.String())
		}
		for _, name := range []string{"add", "bearer", "seed", "demo", "once"} {
			if strings.Contains(out.String(), "-l "+name+" -F") {
				t.Fatalf("fish script offers a file list for --%s:\n%s", name, out.String())
			}
		}
	})
	t.Run("the zsh file arm returns instead of falling through", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"zsh"})
		// Without the return, a path also offers the flag and subcommand
		// words below, so a file name completes into "completion".
		arm := out.String()
		i := strings.Index(arm, "_files")
		if i < 0 {
			t.Fatalf("zsh script has no _files arm:\n%s", arm)
		}
		rest := arm[i:]
		j := strings.Index(rest, ";;")
		if j < 0 || !strings.Contains(rest[:j], "return") {
			t.Fatalf("zsh _files arm does not return before the next case branch:\n%s", arm)
		}
	})
	t.Run("the zsh subcommand branch keys off words[2], not CURRENT", func(t *testing.T) {
		var out bytes.Buffer
		runCompletion(&out, []string{"zsh"})
		// `toktop update --<TAB>` completes at CURRENT 3. A CURRENT == 2
		// test would answer with the top-level flags there.
		if strings.Contains(out.String(), "CURRENT == 2") {
			t.Fatalf("zsh script tests CURRENT == 2 for the subcommand word:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "${words[2]}") {
			t.Fatalf("zsh script does not read the subcommand from words[2]:\n%s", out.String())
		}
	})

	t.Run("both declarative shells name completion's shells", func(t *testing.T) {
		// zsh and fish cannot infer the word list from the FlagSet the way
		// they infer the flags; it has to be written into the script.
		// Absent, `toktop completion <TAB>` offered nothing in either, which
		// is the one command with a word list completing to nothing at all.
		// Each shell spells the list its own way, so each is matched against
		// the line that binds it, not against the whole script: the three
		// names also appear in the comment header and in the help text.
		want := "shells=(" + zshList(completionShells) + ")"
		var zsh, fish bytes.Buffer
		runCompletion(&zsh, []string{"zsh"})
		runCompletion(&fish, []string{"fish"})
		if !strings.Contains(zsh.String(), want) {
			t.Errorf("zsh script has no %q line:\n%s", want, zsh.String())
		}
		wantFish := "-a " + strconv.Quote(strings.Join(completionShells, " "))
		if !strings.Contains(fish.String(), wantFish) {
			t.Errorf("fish script has no completion line carrying %s:\n%s", wantFish, fish.String())
		}
	})
	t.Run("bash and zsh answer nothing after a value-taking flag", func(t *testing.T) {
		// A word being typed as a duration is not a command. Both scripts
		// fell through to the subcommand list here, so `toktop --interval
		// <TAB>` answered with "completion help update version". The arm is
		// built from takesValue, so it covers every value flag, not one.
		for _, shell := range []string{"bash", "zsh"} {
			var out bytes.Buffer
			runCompletion(&out, []string{shell})
			for _, name := range []string{"--interval", "--add", "--bearer", "--repo"} {
				if !strings.Contains(out.String(), name+")") &&
					!strings.Contains(out.String(), name+"|") {
					t.Errorf("%s script has no case arm for %s:\n%s", shell, name, out.String())
				}
			}
		}
	})
	t.Run("no shell is a usage error", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runCompletion(io.Discard, nil) })
		if code != 2 {
			t.Fatalf("runCompletion() = %d, want 2", code)
		}
		if !strings.Contains(got, "no shell given") {
			t.Fatalf("stderr = %q, want the missing shell named", got)
		}
	})
	t.Run("an unknown shell is a usage error naming the ones that work", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runCompletion(io.Discard, []string{"powershell"}) })
		if code != 2 {
			t.Fatalf("runCompletion(powershell) = %d, want 2", code)
		}
		if !strings.Contains(got, "powershell") {
			t.Fatalf("stderr = %q, want the shell named", got)
		}
		for _, shell := range completionShells {
			if !strings.Contains(got, shell) {
				t.Fatalf("stderr = %q, want %s listed", got, shell)
			}
		}
	})
	t.Run("an extra argument is a usage error", func(t *testing.T) {
		var code int
		got := captureStderr(t, func() { code = runCompletion(io.Discard, []string{"bash", "extra"}) })
		if code != 2 {
			t.Fatalf("runCompletion(bash extra) = %d, want 2", code)
		}
		if !strings.Contains(got, "extra") {
			t.Fatalf("stderr = %q, want the extra argument named", got)
		}
	})
	t.Run("--help and --version land on stdout with exit 0", func(t *testing.T) {
		for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}, {"-v"}} {
			var out bytes.Buffer
			var code int
			got := captureStderr(t, func() { code = runCompletion(&out, args) })
			if code != 0 {
				t.Fatalf("runCompletion(%v) = %d, want 0", args, code)
			}
			if got != "" {
				t.Fatalf("runCompletion(%v) leaked %q to stderr", args, got)
			}
			if out.Len() == 0 {
				t.Fatalf("runCompletion(%v) wrote nothing to stdout", args)
			}
		}
	})
}

func TestCompletionInHelp(t *testing.T) {
	t.Run("the top-level screen names the subcommand", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, nil) })
		if code != 0 || got != "" {
			t.Fatalf("runHelp() = %d with stderr %q, want 0 and silence", code, got)
		}
		if !strings.Contains(out.String(), "toktop completion <bash|zsh|fish>") {
			t.Fatalf("top-level usage does not name the completion subcommand:\n%s", out.String())
		}
	})
	t.Run("the completion topic prints its own screen", func(t *testing.T) {
		var out bytes.Buffer
		var code int
		got := captureStderr(t, func() { code = runHelp(&out, []string{"completion"}) })
		if code != 0 {
			t.Fatalf("runHelp(completion) = %d, want 0", code)
		}
		if got != "" {
			t.Fatalf("runHelp(completion) leaked %q to stderr", got)
		}
		if !strings.Contains(out.String(), "toktop completion -") {
			t.Fatalf("runHelp(completion) stdout = %q, want the completion screen", out.String())
		}
	})
	t.Run("a misplaced completion word names the subcommand", func(t *testing.T) {
		// Leftovers after flag.Parse, so the flag is already consumed.
		cmd, _, err := interpretArgs([]string{"completion"})
		if err != nil {
			t.Fatalf("interpretArgs(completion) = %v, want no error", err)
		}
		if cmd != "completion" {
			t.Fatalf("interpretArgs(completion) = %q, want \"completion\"", cmd)
		}
		err = unexpectedArg("completion")
		if err == nil || !strings.Contains(err.Error(), "must be first: toktop completion") {
			t.Fatalf("unexpectedArg(completion) = %v, want the subcommand placement named", err)
		}
	})
}

// TestCompletionScriptsAreValidShell runs the printed script through the
// shell's own parser where that shell is installed, so a syntax error in a
// generated script fails the suite rather than the operator's shell.
func TestCompletionScriptsAreValidShell(t *testing.T) {
	for _, tt := range []struct {
		shell string
		flag  string
	}{
		{"bash", "-n"},
		{"zsh", "-n"},
		{"fish", "--no-execute"},
	} {
		t.Run(tt.shell, func(t *testing.T) {
			bin, err := exec.LookPath(tt.shell)
			if err != nil {
				t.Skipf("%s is not installed", tt.shell)
			}
			var out bytes.Buffer
			if code := runCompletion(&out, []string{tt.shell}); code != 0 {
				t.Fatalf("runCompletion(%s) = %d, want 0", tt.shell, code)
			}
			cmd := exec.Command(bin, tt.flag)
			cmd.Stdin = strings.NewReader(out.String())
			// A completion script runs `complete` / `compdef` at source time,
			// so a bare parse is not enough for bash; -n is the parse-only
			// switch each of the three provides.
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s -%s rejected the script: %v\n%s", tt.shell, strings.TrimPrefix(tt.flag, "-"), err, b)
			}
		})
	}
}

// TestCompletionSourcedInBash loads the script the way an operator installs
// it and asks the function for completions, so a word list that only parses
// but never matches a prefix is caught.
func TestCompletionSourcedInBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	script := &bytes.Buffer{}
	if code := runCompletion(script, []string{"bash"}); code != 0 {
		t.Fatalf("runCompletion(bash) = %d, want 0", code)
	}
	probe := `
source /dev/stdin <<'TOKTOP_COMPLETION'
` + script.String() + `TOKTOP_COMPLETION
try() {
    COMP_WORDS=("$@")
    COMP_CWORD=$(( $# - 1 ))
    COMPREPLY=()
    _toktop
    printf '%s\n' "${COMPREPLY[*]}"
}
printf '%s\n' "top:$(try toktop "")"
printf '%s\n' "dash:$(try toktop -)"
printf '%s\n' "once:$(try toktop --on)"
printf '%s\n' "update:$(try toktop update --)"
printf '%s\n' "shells:$(try toktop completion \"\")"
printf '%s\n' "shellprefix:$(try toktop completion \"z\")"
printf '%s\n' "shellsagain:$(try toktop completion bash \"\")"
printf '%s\n' "valueflag:$(try toktop --interval \"\")"
printf '%s\n' "subvalue:$(try toktop update --repo \"\")"
`
	cmd := exec.Command(bash)
	cmd.Stdin = strings.NewReader(probe)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash probe failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	got := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		name, list, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("probe printed an unparseable line %q", line)
		}
		got[name] = strings.Fields(list)
	}
	for _, sub := range completionSubs {
		if !slices.Contains(got["top"], sub) {
			t.Fatalf("bash completion of `toktop <TAB>` omits %s: %v", sub, got["top"])
		}
	}
	if !slices.Contains(got["once"], "--once") {
		t.Fatalf("bash completion of `toktop --on<TAB>` = %v, want --once", got["once"])
	}
	for _, name := range topFlags() {
		if !slices.Contains(got["dash"], name) {
			t.Fatalf("bash completion of `toktop -<TAB>` omits %s", name)
		}
	}
	for _, name := range updateFlags {
		if !slices.Contains(got["update"], name) {
			t.Fatalf("bash completion of `toktop update --<TAB>` omits %s: %v", name, got["update"])
		}
	}
	// A subcommand's flags must not answer for the top level, and the
	// top-level flags must not answer for a subcommand.
	if slices.Contains(got["update"], "--demo") {
		t.Fatalf("bash completion of `toktop update --<TAB>` offers a top-level flag: %v", got["update"])
	}
	if slices.Contains(got["dash"], "--check") {
		t.Fatalf("bash completion of `toktop -<TAB>` offers update's --check: %v", got["dash"])
	}
	// completion takes a shell, so the shells are what its positional word
	// completes to. The list was absent, which left the one command with a
	// word list offering nothing at all.
	for _, shell := range completionShells {
		if !slices.Contains(got["shells"], shell) {
			t.Errorf("bash completion of `toktop completion <TAB>` omits %s: %v", shell, got["shells"])
		}
	}
	// By prefix, not only on an empty word: "completion z" narrows to zsh.
	if !slices.Equal(got["shellprefix"], []string{"zsh"}) {
		t.Errorf("bash completion of `toktop completion z<TAB>` = %v, want [zsh]", got["shellprefix"])
	}
	// The shells answer for the word after completion and for nothing past
	// it, where the same list kept being offered.
	if len(got["shellsagain"]) != 0 {
		t.Errorf("bash completion of `toktop completion bash <TAB>` = %v, want nothing", got["shellsagain"])
	}
	// A word being typed as a value is not a command. Offering the subcommand
	// names after a value-taking flag is how `--interval <TAB>` used to
	// answer with "completion help update version".
	for _, probe := range []string{"valueflag", "subvalue"} {
		for _, sub := range completionSubs {
			if slices.Contains(got[probe], sub) {
				t.Errorf("bash completion of a value position offers the subcommand %s: %v", sub, got[probe])
			}
		}
	}
}

// TestCompletionFromTheBuiltBinary pins the contract
// `toktop completion bash > file` relies on, against the real program rather
// than the function behind it: the script on stdout, nothing on stderr, exit 0,
// and a redirected run that survives a reader closing the pipe early.
func TestCompletionFromTheBuiltBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/true to close the pipe early on Windows")
	}
	bin := filepath.Join(t.TempDir(), "toktop")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(bin, "completion", shell)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("toktop completion %s: %v\nstderr:\n%s", shell, err, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want silence", stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("stdout is empty, want the %s script", shell)
			}
		})
	}
	t.Run("a reader closing stdout early exits 0", func(t *testing.T) {
		// PIPESTATUS[0] is toktop's status; a bare pipeline would report the
		// reader's, which is always 0 and would prove nothing.
		script := `"` + bin + `" completion bash | true; exit ${PIPESTATUS[0]}`
		if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s = %v, want exit 0\n%s", script, err, out)
		}
	})
	t.Run("a redirected script is byte for byte the piped one", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "toktop.bash")
		redirect := exec.Command(bin, "completion", "bash")
		redirect.Stdout = mustCreate(t, file)
		if err := redirect.Run(); err != nil {
			t.Fatalf("toktop completion bash > file: %v", err)
		}
		piped, err := exec.Command(bin, "completion", "bash").Output()
		if err != nil {
			t.Fatalf("toktop completion bash: %v", err)
		}
		written, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !bytes.Equal(written, piped) {
			t.Fatalf("redirected script differs from the piped one")
		}
	})
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// bashWordList reads one `local <name>="..."` word list out of a generated
// bash script, so a test asserts what the function completes with rather than
// what appears anywhere in the text.
func bashWordList(t *testing.T, script, name string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(script, `local `+name+`="`)
	if !ok {
		t.Fatalf("bash script has no %s list:\n%s", name, script)
	}
	list, _, _ := strings.Cut(rest, `"`)
	return strings.Fields(list)
}
