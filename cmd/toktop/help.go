package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// The help screen and the help/version/update subcommands.

// flagAliases are the one-letter spellings listed beside a long flag. The flag
// package records no alias relationship and flagDocs visits every defined
// flag, so without this pairing -h and --help would print as two unrelated
// entries, one per line.
var flagAliases = map[string]string{
	"help":    "h",
	"version": "v",
}

// flagPlaceholders overrides the argument word the flag package would print.
// Its type-derived words are accurate but generic ("--add value", "--seed
// int64"), which reads as a different CLI from the words the README's flag
// table already commits to ("--add URL", "--seed N").
var flagPlaceholders = map[string]string{
	"add":      "URL",
	"bearer":   "TOKEN",
	"frames":   "N",
	"ingest":   "ADDR",
	"interval": "D",
	"origin":   "TIME",
	"probe":    "N",
	"repo":     "owner/name",
	"seed":     "N",
	"ssh-key":  "PATH",
}

// flagDocs renders one entry per flag, in long form. flag.PrintDefaults
// prints Go's own spelling ("-add value", "-frames int", "(default true)"),
// which contradicts the --long-form used by the examples, the prose below the
// flag list, the README, and every error message naming a flag.
func flagDocs(fs *flag.FlagSet) string {
	// NFlag counts the flags that were set, not the ones defined, so it sizes
	// this at zero on the help path. VisitAll decides the length instead.
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	slices.Sort(names)

	var b strings.Builder
	for _, name := range names {
		if isFlagAlias(name) {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		b.WriteString("  --" + name)
		if short, ok := flagAliases[name]; ok {
			b.WriteString(", -" + short)
		}
		if ph := flagPlaceholder(name, f); ph != "" {
			b.WriteString(" " + ph)
		}
		// Six spaces, not the four-then-tab the flag package's PrintDefaults
		// uses: a tab stop lands the description at column 8, which reads
		// as a stray indent rather than a hanging one under a flag name
		// starting at column 3.
		b.WriteString("\n      " + f.Usage)
		if def := defaultDoc(f.DefValue); def != "" {
			b.WriteString(" (" + def + ")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// isFlagAlias reports whether name is the short spelling of a long flag, in
// which case it is printed beside that flag rather than as an entry of its
// own: a separate "-h" line made it read as a second help flag.
func isFlagAlias(name string) bool {
	for _, short := range flagAliases {
		if short == name {
			return true
		}
	}
	return false
}

// flagPlaceholder is the argument word for a flag, empty for the booleans
// that take none. The flag package's value types are unexported, so the word
// comes from the type name it reports ("*flag.durationValue" -> "duration"),
// which is the same word PrintDefaults would have used.
func flagPlaceholder(name string, f *flag.Flag) string {
	if ph, ok := flagPlaceholders[name]; ok {
		return ph
	}
	word := strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%T", f.Value), "*flag."), "Value")
	if word == "bool" || word == "func" {
		return ""
	}
	return word
}

// defaultDoc renders a flag's default, or nothing when it is the zero value
// (a bare "false", "0" or empty string says nothing a reader did not already
// assume).
func defaultDoc(def string) string {
	if def == "" || def == "false" || def == "0" {
		return ""
	}
	return "default " + def
}

// -h/--help sends it to stdout so piping works (`toktop --help | grep
// probe`); flag-package error paths call it with stderr.
func usage(w io.Writer) error {
	registerFlags()
	var buf strings.Builder
	fmt.Fprint(&buf, `toktop - btop-style dashboard for LLM inference engines and the agents hammering them

Usage:
  toktop [flags] [ssh://user@host ...]
  toktop update [--check] [--repo owner/name]   install the latest release
  toktop completion <bash|zsh|fish>              print a shell completion script
  toktop help [update|version|completion]        this screen; update and
                                                 completion have their own
  toktop version                                print version and exit

Examples:
  toktop --demo                simulated fleet, works instantly
  toktop                       auto-discover engines on this machine
  toktop ssh://user@box        watch another host's engines over ssh
  toktop --add http://10.0.0.5:8000   attach an endpoint (repeatable)
  toktop --agents              also watch coding agents on this machine
                               (opencode's session database included)
  toktop --agents --opencode-db=false   ...without opencode's session database
  toktop --once >frame.txt     render one static frame and exit
  toktop --once --plain        one frame as a linear text report (screen readers)
  toktop --once --json         one snapshot as JSON, for scripts
  toktop --demo --seed 7 --origin 2026-01-01T00:00:00Z
                               demo run a seed reproduces byte for byte

Flags:
`)
	buf.WriteString(flagDocs(topFS))
	fmt.Fprint(&buf, `
Positional arguments are ssh:// targets and may repeat; help, version and
completion are also accepted as commands. 'toktop help update', 'toktop
--help update' and 'toktop update --help' print the update screen; the same
three spellings print the completion screen for 'toktop help completion'.
'toktop help version' and 'toktop version --help' both print this one, since
version takes no flags of its own. http(s) URLs are rejected with an --add
hint; anything else points at --help. --add URLs must be http(s) with a host
and must not embed userinfo. ssh:// targets must be ssh://[user@]host[:port]
and must not embed a password (use $TOKTOP_SSH_PASSWORD or --ssh-key); a
host or user carrying a bidi control, zero-width or other invisible
character is refused, since the name shown is not the one ssh dials.
The live dashboard needs a terminal; use --once when piping or redirecting.

Environment (a flag always wins over the variable it mirrors):
  TOKTOP_BEARER           bearer token for --add endpoints; OMNIROUTE_API_KEY
                          is consulted first, and an explicit --bearer (even
                          empty) suppresses both. Surrounding whitespace is
                          trimmed; a blank value carries no token and is
                          skipped in favor of the next source
  TOKTOP_SSH_PASSWORD     ssh password for ssh:// targets, for headless runs
`+fmt.Sprintf(`  TOKTOP_COLUMNS          --once frame width, %d-%d (default: the terminal,
                          else %d when stdout is not one)
  TOKTOP_LINES            --once frame height, %d-%d (default: the terminal,
                          else %d when stdout is not one)`,
		frameColumnsMin, frameColumnsMax, frameColumnsDefault,
		frameLinesMin, frameLinesMax, frameLinesDefault)+`
  TOKTOP_LOG_LEVEL        audit log floor for every subsystem that writes one
                          (engine, ssh, ingest): debug, info, warn (or
                          warning), error; the name is case-insensitive
  GAUNTLET_HOME           directory holding agents.json (--agents), default
                          ~/.gauntlet; a relative value is ignored
  XDG_DATA_HOME           where opencode's session database is read
                          (--opencode-db), default ~/.local/share; a relative
                          value is ignored
  XDG_CONFIG_HOME         where the ssh host-key store lives, default
                          ~/.config; a relative value is ignored
  KIMI_CODE_HOME          where kimi's session logs are read (--agents),
                          default ~/.kimi-code; a relative value is ignored,
                          and so is one with no sessions directory in it
  GITHUB_TOKEN            optional, authenticates toktop update's GitHub calls;
                          set but blank is named, and carries no token
  SSH_AUTH_SOCK           ssh-agent socket for ssh:// targets
  NO_COLOR                recognized by the terminal renderer, as usual
An unrecognized TOKTOP_* name is reported as a typo at startup, and one that
cannot take effect in the chosen mode is named rather than silently ignored.
See README.md for the full environment reference.

Exit codes:
  0    success, including a reader such as head closing stdout early
  1    runtime failure (no telemetry arrived, a write or the update failed)
  2    usage error (unknown flag, command or ssh:// target, bad value)
  130  interrupted (--once, toktop update, and the live dashboard when
       SIGINT or SIGTERM stops it; the dashboard's own q and Ctrl+C are a
       clean 0)

Results go to stdout (the rendered frame, the JSON report, the version, the
release URL); progress, warnings and errors go to stderr, so a script can
read stdout without filtering status lines out of it.
`)
	_, err := io.WriteString(w, buf.String())
	return err
}

// flagParseError renders a parse failure in the long flag spelling. The flag
// package reports "-interval" for a flag the help screen, the README and every
// example in both document as "--interval", so its message named a spelling
// the reader was never shown and that is not the one accepted elsewhere.
func flagParseError(err error) string {
	msg := err.Error()
	const unknownFlag = "flag provided but not defined: "
	if name, rest, ok := strings.Cut(msg, unknownFlag); ok && name == "" {
		return unknownFlag + longFlag(rest)
	}
	// The package names the flag two ways: "for flag -x" for a value that
	// failed to parse, and a bare "for -x" for a boolean given a non-boolean.
	// Both are rebuilt as "for flag --x", so the two read the same and the
	// boolean case gains the noun the other already had.
	for _, sep := range [...]string{" for flag -", " for -"} {
		if before, after, ok := strings.Cut(msg, sep); ok {
			name, tail, _ := strings.Cut(after, ":")
			return before + " for flag " + longFlag(name) + ":" + tail
		}
	}
	const needsArg = "flag needs an argument: "
	if name, ok := strings.CutPrefix(msg, needsArg); ok {
		return needsArg + longFlag(name)
	}
	return msg
}

// subcommandFlags are the flags that live on a subcommand rather than on the
// top-level command. Written at the top level they are a placement mistake,
// and the flag package's "flag provided but not defined" names neither the
// subcommand they belong to nor the command line that would work. This is the
// same treatment unexpectedArg gives a subcommand word that did not come
// first.
var subcommandFlags = map[string]string{
	"check": "toktop update --check",
	"repo":  "toktop update --repo owner/name",
}

// subcommandFlagHint names the subcommand a misplaced flag belongs to. Only a
// flag that is genuinely undefined is answered, and only with a command line
// that would work; a name no subcommand declares gets nothing, as a misplaced
// word that names no command does.
func subcommandFlagHint(err error) string {
	const unknownFlag = "flag provided but not defined: "
	name, ok := strings.CutPrefix(err.Error(), unknownFlag)
	if !ok {
		return ""
	}
	line, ok := subcommandFlags[strings.TrimLeft(name, "-")]
	if !ok {
		return ""
	}
	return " (" + line + ")"
}

// valueForms names what each value flag's value has to look like. The flag
// package reports every one of them the same way ("parse error"), which names
// neither the expectation nor a value that would work, so `--frames abc` and
// `--seed 1.5` read the same as a malformed duration.
// The two bounds are the constants the run is gated on, not numbers typed
// beside them: flags.go already builds the --probe bound the same way.
var valueForms = map[string]string{
	"frames":   fmt.Sprintf("a whole number of snapshots, 1-%d", core.HistoryLen),
	"interval": "a Go duration such as 1s or 500ms",
	"probe":    fmt.Sprintf("a whole number of seconds, 0-%d", probeSecsMax),
	"seed":     "a whole number",
}

// valueHint appends to a parse failure what the value was supposed to be. It
// reads the flag name in the single-dash spelling the flag package emits, the
// same spelling flagParseError rewrites, so both run over the same message.
//
// --interval has a second spelling worth naming: a bare number there is read as
// nanoseconds and the package says nothing about it, so `--interval 1` named
// neither the unit nor a value that would work. The unit hint is given only for
// a value that really is a bare number; one carrying a unit and still failing
// was misspelled, and the unit is not the answer.
func valueHint(err error) string {
	const prefix = "invalid value \""
	raw, rest, ok := strings.Cut(strings.TrimPrefix(err.Error(), prefix), "\"")
	if !ok {
		return ""
	}
	const forFlag = " for flag -"
	after, ok := strings.CutPrefix(rest, forFlag)
	if !ok {
		return ""
	}
	name, tail, _ := strings.Cut(after, ": ")
	if tail != "parse error" {
		return ""
	}
	if name == "interval" {
		if _, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			return " (a bare number is nanoseconds; use 1s or 500ms)"
		}
	}
	form, ok := valueForms[name]
	if !ok {
		return ""
	}
	return " (expected " + form + ")"
}

// longFlag renders a flag name the help screen and the README use. The flag
// package's messages already carry the single dash it prints, so the leading
// one is dropped before the long form is put back.
func longFlag(name string) string { return "--" + strings.TrimPrefix(name, "-") }

// isHelpArg reports whether arg is one of Go's supported help flag spellings.
func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--h" || arg == "-help" || arg == "--help"
}

// versionFlags are the spellings of --version that every command carrying it
// accepts: the long form, the single-dash spelling the flag package also
// parses, and the -v short form declared in flagAliases.
var versionFlags = []string{"--version", "-version", "-v"}

// isVersionArg reports whether arg is one of the spellings of --version.
func isVersionArg(arg string) bool { return slices.Contains(versionFlags, arg) }

// runHelp implements `toktop help [topic]`. Unknown topics are a usage error
// so a typo does not dump the top-level screen and look like success. A topic
// names the command whose help applies, so the extra-argument message points
// at the screen that would have answered it.
func runHelp(out io.Writer, args []string) int {
	if len(args) == 0 || isHelpArg(args[0]) || args[0] == "help" {
		if len(args) > 1 {
			return rejectExtra("toktop help", args[1])
		}
		return outputStatus(usage(out))
	}
	// --version under help prints the version, as it does under `version` and
	// under `update`: a flag that means the same thing in all three commands
	// must not be a usage error in one of them.
	if isVersionArg(args[0]) {
		if len(args) > 1 {
			return rejectExtra("toktop help", args[1])
		}
		return runVersion(out, nil)
	}
	switch args[0] {
	case "update":
		if len(args) > 1 {
			return rejectExtra("toktop update", args[1])
		}
		return runUpdate(context.Background(), out, []string{"--help"})
	case "completion":
		if len(args) > 1 {
			return rejectExtra("toktop completion", args[1])
		}
		return outputStatus(completionUsage(out))
	case "version":
		if len(args) > 1 {
			return rejectExtra("toktop version", args[1])
		}
		return outputStatus(usage(out))
	}
	if strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "toktop: unknown option %q (see 'toktop --help')\n", args[0])
		return 2
	}
	fmt.Fprintf(os.Stderr, "toktop: no help topic for %q (see 'toktop --help')\n", args[0])
	return 2
}

func rejectExtra(cmd, arg string) int {
	fmt.Fprintf(os.Stderr, "%s: unexpected argument %q (see '%s --help')\n", cmd, arg, cmd)
	return 2
}

// runVersion implements `toktop version`. --help prints top-level usage and
// --version prints the version, matching what `toktop update` accepts; any
// other extra argument is a usage error.
func runVersion(out io.Writer, args []string) int {
	if len(args) > 0 {
		if isVersionArg(args[0]) {
			if len(args) > 1 {
				return rejectExtra("toktop version", args[1])
			}
			_, err := fmt.Fprintln(out, "toktop", version)
			return outputStatus(err)
		}
		if isHelpArg(args[0]) {
			if len(args) > 1 {
				return rejectExtra("toktop version", args[1])
			}
			return outputStatus(usage(out))
		}
		return rejectExtra("toktop version", args[0])
	}
	_, err := fmt.Fprintln(out, "toktop", version)
	return outputStatus(err)
}

// interpretArgs classifies leftovers after flag.Parse. help/version cover
// `toktop --once help` (the first-arg dispatch already handled `toktop help`);
// everything else is an ssh:// target or a usage error.
func interpretArgs(args []string) (cmd string, remotes []string, err error) {
	if len(args) == 0 {
		return "", nil, nil
	}
	switch args[0] {
	case "help":
		return "help", args[1:], nil
	case "completion":
		return "completion", args[1:], nil
	case "version":
		if len(args) > 1 {
			return "", nil, fmt.Errorf("toktop version: unexpected argument %q (see 'toktop version --help')", args[1])
		}
		return "version", nil, nil
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "ssh://") {
			remotes = append(remotes, arg)
			continue
		}
		if i > 0 && strings.HasPrefix(arg, "-") {
			// flag parsing stopped at the first positional, so a flag written
			// after an ssh:// target comes back as a leftover. Naming only the
			// argument would leave the reader with nothing to change.
			return "", nil, fmt.Errorf("toktop: %q must come before the ssh:// targets (see 'toktop --help')", arg)
		}
		return "", nil, unexpectedArg(arg)
	}
	return "", remotes, nil
}

// unexpectedArg names the leftover and how to fix it. A subcommand word that
// did not come first says so and names the command that would work; only
// http(s) URLs suggest --add; other bare words point at --help.
func unexpectedArg(arg string) error {
	switch {
	case arg == "update":
		return fmt.Errorf("toktop: unexpected argument %q (the update subcommand must be first: toktop update)", arg)
	case arg == "help" || arg == "version" || arg == "completion":
		return fmt.Errorf("toktop: unexpected argument %q (the %s subcommand must be first: toktop %s)", arg, arg, arg)
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		return fmt.Errorf("toktop: unexpected argument %q (did you mean --add %s?)", arg, arg)
	default:
		return fmt.Errorf("toktop: unexpected argument %q (see 'toktop --help')", arg)
	}
}
