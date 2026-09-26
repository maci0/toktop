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
	"path/filepath"
	"strings"

	"github.com/maci0/toktop/internal/selfupdate"
)

// updateUsage prints the subcommand's help screen to w. Error paths send it
// to stderr; -h/--help sends it to stdout so piping works (`toktop update
// --help | grep repo`), matching how the top-level command treats --help.
func updateUsage(w io.Writer, fs *flag.FlagSet) error {
	var buf strings.Builder
	prev := fs.Output()
	fs.SetOutput(&buf)
	defer fs.SetOutput(prev)
	fmt.Fprint(&buf, `toktop update - install the latest release

Usage:
  toktop update [--check] [--repo owner/name]
  toktop update --help
  toktop update --version

The download is verified against the release's checksums before anything is
replaced; a mismatch leaves the running binary untouched.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(&buf, `
$GITHUB_TOKEN authenticates GitHub API calls past the anonymous rate limit.
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
	fs.SetOutput(os.Stderr)
	check := fs.Bool("check", false, "report the latest release without installing it")
	repo := fs.String("repo", selfupdate.DefaultRepo, "GitHub repository to fetch releases from (owner/name)")
	var showHelp, showVer bool
	fs.BoolVar(&showHelp, "help", false, "show help and exit")
	fs.BoolVar(&showHelp, "h", false, "show help and exit")
	fs.BoolVar(&showVer, "version", false, "print version and exit")
	// Defining -h/--help as real flags keeps the flag package from treating
	// them as a parse error, so they can land on stdout with exit 0 the way
	// the top-level command's --help does. --version matches the parent.
	fs.Usage = func() { updateUsage(os.Stderr, fs) }
	if err := fs.Parse(args); err != nil {
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
		fmt.Fprintf(os.Stderr, "toktop update: %v\n", err)
		return 2
	}

	rel, err := selfupdate.Check(ctx, *repo)
	if err != nil {
		return updateErr("cannot check for updates", err)
	}
	if !rel.NewerThan(version) {
		_, err := fmt.Fprintf(out, "toktop %s is current (latest release: %s)\n", version, rel.TagName)
		return outputStatus(err)
	}
	if _, err := fmt.Fprintf(out, "New release: %s (running %s)\n", rel.TagName, version); err != nil {
		return outputStatus(err)
	}
	if *check {
		_, err := fmt.Fprintln(out, rel.HTMLURL)
		return outputStatus(err)
	}
	fmt.Fprintf(os.Stderr, "toktop: installing %s...\n", rel.TagName)
	path, err := selfupdate.Apply(ctx, rel)
	if err != nil {
		return updateErr("update failed", err)
	}
	_, err = fmt.Fprintf(out, "Installed %s to %s\n", rel.TagName, path)
	return outputStatus(err)
}

// updateErr maps a canceled context to the same 130 the --once path uses
// for Ctrl+C, and prints a short cause instead of leaking context.Canceled.
func updateErr(op string, err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "toktop: interrupted")
		return 130
	}
	msg := err.Error()
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.IsAbs(home) {
		home = filepath.Clean(home)
		if filepath.Dir(home) != home {
			sep := string(filepath.Separator)
			msg = strings.ReplaceAll(msg, home+sep, "~"+sep)
		}
	}
	fmt.Fprintf(os.Stderr, "toktop: %s: %v\n", op, msg)
	return 1
}
