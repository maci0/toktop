// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/selfupdate"
)

// updateUsage prints the subcommand's help screen to w. Error paths send it
// to stderr; -h/--help sends it to stdout so piping works (`toktop update
// --help | grep repo`), matching how the top-level command treats --help.
func updateUsage(w io.Writer, fs *flag.FlagSet) error {
	var buf strings.Builder
	fmt.Fprint(&buf, `toktop update - install the latest release

Usage:
  toktop update [--check] [--repo owner/name]
  toktop update --help
  toktop update --version

The download is verified against the release's checksums before anything is
replaced; a mismatch leaves the running binary untouched.

Examples:
  toktop update --check        print the latest release URL, install nothing
  toktop update                install the latest release
  toktop update --repo you/toktop   track a fork instead

Flags:
`)
	buf.WriteString(flagDocs(fs))
	fmt.Fprint(&buf, `
$GITHUB_TOKEN authenticates GitHub API calls past the anonymous rate limit.
With --check, stdout is the release URL and nothing else, so
'url=$(toktop update --check)' is a URL whether or not this build is
already current; the comparison and the install progress go to stderr.
Without --check, stdout is the result and progress still goes to stderr.
A failed check or install exits 1, a usage error exits 2, Ctrl+C exits 130.
`)
	_, err := io.WriteString(w, buf.String())
	return err
}

// updateOpts holds what `toktop update` parses, so the subcommand and the
// generated completion scripts read one set of definitions rather than a list
// spelled twice.
type updateOpts struct {
	check    bool
	repo     string
	showHelp bool
	showVer  bool
}

// updateFlagSet builds the FlagSet `toktop update` parses.
//
// Defining -h/--help as real flags keeps the flag package from treating them
// as a parse error, so they can land on stdout with exit 0 the way the
// top-level command's --help does. --version matches the parent.
func updateFlagSet() (*flag.FlagSet, *updateOpts) {
	fs := flag.NewFlagSet("toktop update", flag.ContinueOnError)
	opts := &updateOpts{}
	fs.BoolVar(&opts.check, "check", false, "report the latest release without installing it")
	fs.StringVar(&opts.repo, "repo", selfupdate.DefaultRepo, "GitHub repository to fetch releases from (owner/name)")
	fs.BoolVar(&opts.showHelp, "help", false, "show help and exit")
	fs.BoolVar(&opts.showHelp, "h", false, "show help and exit")
	fs.BoolVar(&opts.showVer, "version", false, "print version and exit")
	fs.BoolVar(&opts.showVer, "v", false, "print version and exit")
	fs.Usage = func() {}
	fs.SetOutput(io.Discard)
	return fs, opts
}

// runUpdate implements `toktop update`, which replaces this binary with the
// latest release after verifying its checksum.
//
// On Unix a running dashboard needs no restart: it watches its own executable
// and re-execs when it changes (see internal/selfreload), so an update applied
// in another terminal lands in the session already open. Windows cannot exec
// over a running image, so the dashboard exits and asks you to start it again.
func runUpdate(ctx context.Context, out io.Writer, args []string) int {
	fs, opts := updateFlagSet()
	parseErr := fs.Parse(args)
	// An unknown flag does not swallow a --help or --version written beside
	// it, the same rule the top-level command applies and for the same
	// reason: `toktop update --check --bogus --help` is a line somebody
	// writes while working out the spelling of a flag, and the screen it
	// asks for is what lists the spellings. Only that one parse failure is
	// answered; a bad value and a missing argument still report themselves.
	switch explicitHelpArg(args, parseErr) {
	case "help":
		return outputStatus(updateUsage(out, fs))
	case "version":
		_, err := fmt.Fprintln(out, "toktop", version)
		return outputStatus(err)
	}
	if parseErr != nil {
		// Reported here rather than by the package, so the message and the
		// usage screen under it both use the long flag spelling the help
		// screen documents.
		fmt.Fprintf(os.Stderr, "toktop update: %s\n", flagParseError(parseErr))
		updateUsage(os.Stderr, fs)
		return 2
	}
	if fs.NArg() > 0 {
		return rejectExtra("toktop update", fs.Arg(0))
	}
	if opts.showHelp {
		return outputStatus(updateUsage(out, fs))
	}
	if opts.showVer {
		_, err := fmt.Fprintln(out, "toktop", version)
		return outputStatus(err)
	}

	if err := selfupdate.ValidateRepo(opts.repo); err != nil {
		// A bad --repo is a usage error, and every other one in this
		// subcommand names its flag in long form and prints the usage screen
		// under it. ValidateRepo writes the bare word "repo" because the
		// library has no flag to name, so the flag is put back here.
		fmt.Fprintf(os.Stderr, "toktop update: --%s\n", err)
		updateUsage(os.Stderr, fs)
		return 2
	}

	warnBlankGitHubToken()

	rel, err := selfupdate.Check(ctx, opts.repo)
	if err != nil {
		return updateErr("cannot check for updates", err)
	}
	code, newer := reportRelease(out, os.Stderr, rel, opts.check)
	if code != 0 || !newer {
		return code
	}
	fmt.Fprintf(os.Stderr, "toktop: installing %s...\n", rel.TagName)
	path, err := selfupdate.Apply(ctx, rel)
	if err != nil {
		return updateErr("update failed", err)
	}
	// Folded like every failure on this path: the install path is the
	// running binary, usually under the operator's home, and this line
	// lands on stdout, the stream a --check run is piped and pasted from.
	_, err = fmt.Fprintf(out, "Installed %s to %s\n", rel.TagName, core.RedactHome(path))
	return outputStatus(err)
}

// warnBlankGitHubToken names a $GITHUB_TOKEN that is set but carries no token.
// The generic rate-limit message it produces advises setting the very variable
// the operator already set, so the misconfiguration is reported before the
// request rather than inferred from an error that reads as a first-run one.
// Checked here, where the variable is read: the top-level command never reads
// it, so a dashboard run has nothing to say about it.
func warnBlankGitHubToken() {
	v, set := os.LookupEnv(selfupdate.TokenEnv)
	if !set || strings.TrimSpace(v) != "" {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop update: $%s is set but blank; the GitHub API is queried unauthenticated\n",
		selfupdate.TokenEnv)
}

// reportRelease prints what the check found and reports whether an install
// should follow. With --check, stdout carries the release URL and nothing
// else, so `url=$(toktop update --check)` is a URL whether or not the running
// binary is already current; the "New release" and "is current" lines are
// status and go to stderr.
func reportRelease(out, status io.Writer, rel *selfupdate.Release, check bool) (code int, newer bool) {
	found := !rel.NewerThan(version)
	if check {
		var err error
		if _, err = fmt.Fprint(status, releaseLine(found, rel)); err != nil {
			return outputStatus(err), false
		}
		// The help screen documents this as `url=$(toktop update --check)`,
		// so the value lands in a shell expansion. It is release data, and
		// the release's own asset URLs are checked against GitHub's hosts
		// before anything is fetched; the page URL is held to the same
		// rule rather than printed as it arrived.
		// A run that declines to print its one documented output has not
		// answered the check: `url=$(toktop update --check)` would capture an
		// empty string and the caller's next command would run against it, so
		// this is the failed check the help screen says exits 1, not a
		// success that wrote nothing.
		if !selfupdate.TrustedReleaseURL(rel.HTMLURL) {
			fmt.Fprintln(status, "toktop: the release names no GitHub release page; not printing it for capture")
			return 1, false
		}
		_, err = fmt.Fprintln(out, rel.HTMLURL)
		return outputStatus(err), false
	}
	_, err := fmt.Fprint(out, releaseLine(found, rel))
	return outputStatus(err), !found
}

// releaseLine is the one wording for both outcomes. The --check path sends it
// to stderr and the plain path to stdout, but the text must not differ: an
// operator reading one and then the other should not see two phrasings of the
// same answer.
func releaseLine(found bool, rel *selfupdate.Release) string {
	if found {
		return fmt.Sprintf("toktop %s is current (latest release: %s)\n", version, rel.TagName)
	}
	return fmt.Sprintf("New release: %s (running %s)\n", rel.TagName, version)
}

// updateErr maps a canceled context to the same 130 the --once path uses
// for Ctrl+C, and prints a short cause instead of leaking context.Canceled.
func updateErr(op string, err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "toktop: interrupted")
		return 130
	}
	msg := core.RedactHome(err.Error())
	fmt.Fprintf(os.Stderr, "toktop: %s: %v\n", op, msg)
	return 1
}
