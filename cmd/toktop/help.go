package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// The help screen and the help/version/update subcommands.

// -h/--help sends it to stdout so piping works (`toktop --help | grep
// probe`); flag-package error paths call it with stderr.
func usage(w io.Writer) error {
	registerFlags()
	var buf strings.Builder
	out := flag.CommandLine.Output()
	flag.CommandLine.SetOutput(&buf)
	defer flag.CommandLine.SetOutput(out)
	fmt.Fprint(&buf, `toktop - btop-style dashboard for LLM inference engines and the agents hammering them

Usage:
  toktop [flags] [ssh://user@host ...]
  toktop update [--check] [--repo owner/name]   install the latest release
  toktop help [update|version]                  show help for toktop or a subcommand
  toktop version                                print version and exit

Examples:
  toktop --demo                simulated fleet, works instantly
  toktop                       auto-discover engines on this machine
  toktop ssh://maci@box        watch another host's engines over ssh
  toktop --add http://10.0.0.5:8000   attach an endpoint (repeatable)
  toktop --agents              also watch coding agents on this machine
                               (opencode's session database included)
  toktop --agents --opencode-db=false   ...without opencode's session database
  toktop --once >frame.txt     render one static frame and exit
  toktop --once --plain        one frame as a linear text report (screen readers)

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(&buf, `
Positional arguments are ssh:// targets and may repeat; help and version
are also accepted as commands. http(s) URLs are rejected with an --add hint;
anything else points at --help. --add URLs must be http(s) with a host and
must not embed userinfo. ssh:// targets must be ssh://[user@]host[:port]
and must not embed a password (use $TOKTOP_SSH_PASSWORD or --ssh-key).
Bearer tokens fall back to $OMNIROUTE_API_KEY then $TOKTOP_BEARER (an
explicit --bearer, even empty, wins) and are sent only to --add endpoints.
The live dashboard needs a terminal; use --once when piping or redirecting.
See README.md for all environment variables.
`)
	_, err := io.WriteString(w, buf.String())
	return err
}

// isHelpArg reports whether arg is one of Go's supported help flag spellings.
func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--h" || arg == "-help" || arg == "--help"
}

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
	switch args[0] {
	case "update":
		if len(args) > 1 {
			return rejectExtra("toktop update", args[1])
		}
		return runUpdate(context.Background(), out, []string{"--help"})
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

// runVersion implements `toktop version`. --help prints top-level usage;
// any other extra argument is a usage error.
func runVersion(out io.Writer, args []string) int {
	if len(args) > 0 {
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
	case "version":
		if len(args) > 1 {
			return "", nil, fmt.Errorf("toktop version: unexpected argument %q (see 'toktop version --help')", args[1])
		}
		return "version", nil, nil
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "ssh://") {
			remotes = append(remotes, arg)
			continue
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
	case arg == "update" || arg == "help" || arg == "version":
		return fmt.Errorf("toktop: unexpected argument %q (the %s subcommand must be first: toktop %s)", arg, arg, arg)
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		return fmt.Errorf("toktop: unexpected argument %q (did you mean --add %s?)", arg, arg)
	default:
		return fmt.Errorf("toktop: unexpected argument %q (see 'toktop --help')", arg)
	}
}

// warnIgnoredFlags names flags passed explicitly but with no effect in the
