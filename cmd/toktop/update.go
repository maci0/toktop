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

// runUpdate implements `toktop update`, which replaces this binary with the
// latest release after verifying its checksum.
//
// On Unix a running dashboard needs no restart: it watches its own executable
// and re-execs when it changes (see internal/selfreload), so an update applied
// in another terminal lands in the session already open. Windows cannot exec
// over a running image, so the dashboard exits and asks you to start it again.
func runUpdate(ctx context.Context, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("toktop update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report the latest release without installing it")
	repo := fs.String("repo", selfupdate.DefaultRepo, "GitHub repository to fetch releases from (owner/name)")
	var showHelp, showVer bool
	fs.BoolVar(&showHelp, "help", false, "show help and exit")
	fs.BoolVar(&showHelp, "h", false, "show help and exit")
	fs.BoolVar(&showVer, "version", false, "print version and exit")
	// Defining -h/--help as real flags keeps the flag package from treating
	// them as a parse error, so they can land on stdout with exit 0 the way
	// the top-level command's --help does. --version matches the parent.
	fs.Usage = func() {}
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		// Reported here rather than by the package, so the message and the
		// usage screen under it both use the long flag spelling the help
		// screen documents.
		fmt.Fprintf(os.Stderr, "toktop update: %s\n", flagParseError(err))
		updateUsage(os.Stderr, fs)
		return 2
	}
	if fs.NArg() > 0 {
		return rejectExtra("toktop update", fs.Arg(0))
	}
	if showHelp {
		return outputStatus(updateUsage(out, fs))
	}
	if showVer {
		_, err := fmt.Fprintln(out, "toktop", version)
		return outputStatus(err)
	}

	if err := selfupdate.ValidateRepo(*repo); err != nil {
		// A bad --repo is a usage error, and every other one in this
		// subcommand names its flag in long form and prints the usage screen
		// under it. ValidateRepo writes the bare word "repo" because the
		// library has no flag to name, so the flag is put back here.
		fmt.Fprintf(os.Stderr, "toktop update: --%s\n", err)
		updateUsage(os.Stderr, fs)
		return 2
	}

	rel, err := selfupdate.Check(ctx, *repo)
	if err != nil {
		return updateErr("cannot check for updates", err)
	}
	code, newer := reportRelease(out, os.Stderr, rel, *check)
	if code != 0 || !newer {
		return code
	}
	fmt.Fprintf(os.Stderr, "toktop: installing %s...\n", rel.TagName)
	path, err := selfupdate.Apply(ctx, rel)
	if err != nil {
		return updateErr("update failed", err)
	}
	_, err = fmt.Fprintf(out, "Installed %s to %s\n", rel.TagName, path)
	return outputStatus(err)
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
		if found {
			_, err = fmt.Fprintf(status, "toktop %s is current (latest release: %s)\n", version, rel.TagName)
		} else {
			_, err = fmt.Fprintf(status, "New release: %s (running %s)\n", rel.TagName, version)
		}
		if err != nil {
			return outputStatus(err), false
		}
		_, err = fmt.Fprintln(out, rel.HTMLURL)
		return outputStatus(err), false
	}
	if found {
		_, err := fmt.Fprintf(out, "toktop %s is current (latest release: %s)\n", version, rel.TagName)
		return outputStatus(err), false
	}
	_, err := fmt.Fprintf(out, "New release: %s (running %s)\n", rel.TagName, version)
	return outputStatus(err), true
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
