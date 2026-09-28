package main

import (
	"flag"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The top-level FlagSet: registration, defaults and mode validation.

// topFS is toktop's own FlagSet rather than flag.CommandLine, so a parse
// failure is reported in the long flag spelling the help screen and the
// README use, and so `toktop --help` under a test binary does not list that
// binary's own --test.* flags. ContinueOnError keeps Parse from calling
// os.Exit itself: main prints the error and the usage screen, then exits 2.
var topFS = flag.NewFlagSet("toktop", flag.ContinueOnError)

type cliFlags struct {
	demo      bool
	adds      []string
	probeSecs int
	interval  time.Duration
	ingest    string
	noIngest  bool
	agents    bool
	opencode  bool
	once      bool
	plain     bool
	jsonOut   bool
	frames    int
	noReload  bool
	seed      int64
	origin    string
	sshKey    string
	bearer    string
	showVer   bool
	showHelp  bool
}

var (
	cli       cliFlags
	flagsOnce sync.Once
)

func registerFlags() *cliFlags {
	flagsOnce.Do(func() {
		topFS.BoolVar(&cli.demo, "demo", false, "run against a simulated fleet instead of real backends")
		topFS.IntVar(&cli.probeSecs, "probe", 0, fmt.Sprintf("auto-probe every N seconds (0=off, max %d; with --demo, every N simulated seconds)", probeSecsMax))
		topFS.DurationVar(&cli.interval, "interval", time.Second, "poll interval as a Go duration such as 1s or 500ms (min 50ms, max 1h)")
		topFS.StringVar(&cli.ingest, "ingest", "127.0.0.1:8420", "agent event ingest listen address (host:port)")
		topFS.BoolVar(&cli.noIngest, "no-ingest", false, "disable the agent event HTTP endpoint")
		topFS.BoolVar(&cli.agents, "agents", false, "watch AI coding agents on this machine by reading their session transcripts")
		// The default is appended by defaultDoc as "(default true)", the same
		// way --ingest and --frames carry theirs; a "default on" left in the
		// usage string printed the default twice, in two spellings.
		topFS.BoolVar(&cli.opencode, "opencode-db", true, "with --agents: read opencode's SQLite session database (needs a build with -tags sqlite; --opencode-db=false skips it)")
		topFS.BoolVar(&cli.once, "once", false, "render one frame and exit (non-interactive; use when piping)")
		topFS.BoolVar(&cli.plain, "plain", false, "with --once: render a linear text report instead of the dashboard frame (screen-reader friendly; --json replaces it)")
		topFS.BoolVar(&cli.jsonOut, "json", false, "with --once: print the final snapshot as JSON on stdout instead of a frame (for scripts; replaces the text report)")
		// The plain and JSON reports render the last snapshot alone, so a count
		// above one buys only the wait before rendering. Stated here because
		// warnIgnoredFlags says exactly that at run time, and a warning the help
		// screen did not predict reads as a bug rather than as a limit.
		topFS.IntVar(&cli.frames, "frames", 2, fmt.Sprintf("with --once: snapshots to accumulate before rendering (max %d); with --plain or --json only the wait before rendering changes", core.HistoryLen))
		topFS.BoolVar(&cli.noReload, "no-hot-reload", false, "disable restart-on-rebuild (dev convenience)")
		topFS.Int64Var(&cli.seed, "seed", 42, "demo RNG seed")
		topFS.StringVar(&cli.origin, "origin", "", "with --demo: RFC3339 or Unix-seconds instant the simulated timeline starts at, so a seed replays byte for byte; unpinned, it starts at the wall clock")
		topFS.StringVar(&cli.sshKey, "ssh-key", "", "private key for ssh:// targets (overrides ~/.ssh/config)")
		topFS.StringVar(&cli.bearer, "bearer", "", "bearer token sent to --add endpoints only (OmniRoute etc.)")
		topFS.BoolVar(&cli.showVer, "version", false, "print version and exit")
		topFS.BoolVar(&cli.showHelp, "help", false, "show help and exit")
		topFS.BoolVar(&cli.showHelp, "h", false, "show help and exit")
		topFS.Func("add", "attach an openai-compatible backend http(s) URL (repeatable, once per endpoint)", func(v string) error {
			return parseAdd(v, &cli.adds)
		})
		// Parse errors are reported by main, not by the flag package: the
		// package's own message and usage would be printed in its single-dash
		// spelling, beside the long form main prints. -h/--help are real flags
		// so they are parsed, not intercepted, and land on stdout with exit 0.
		topFS.SetOutput(io.Discard)
		topFS.Usage = func() {}
	})
	return &cli
}
