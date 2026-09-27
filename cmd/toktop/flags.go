package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The top-level FlagSet: registration, defaults and mode validation.

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
	frames    int
	noReload  bool
	seed      int64
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
		flag.BoolVar(&cli.demo, "demo", false, "run against a simulated fleet instead of real backends")
		flag.IntVar(&cli.probeSecs, "probe", 0, fmt.Sprintf("auto-probe every N seconds (0=off, max %d)", probeSecsMax))
		flag.DurationVar(&cli.interval, "interval", time.Second, "poll interval as a Go duration such as 1s or 500ms (min 50ms, max 1h)")
		flag.StringVar(&cli.ingest, "ingest", "127.0.0.1:8420", "agent event ingest listen address (host:port)")
		flag.BoolVar(&cli.noIngest, "no-ingest", false, "disable the agent event HTTP endpoint")
		flag.BoolVar(&cli.agents, "agents", false, "watch AI coding agents on this machine by reading their session transcripts")
		flag.BoolVar(&cli.opencode, "opencode-db", true, "with --agents: read opencode's SQLite session database (default on; needs a build with -tags sqlite; --opencode-db=false skips it)")
		flag.BoolVar(&cli.once, "once", false, "render one frame and exit (non-interactive; use when piping)")
		flag.BoolVar(&cli.plain, "plain", false, "with --once: render a linear text report instead of the dashboard frame (screen-reader friendly)")
		flag.IntVar(&cli.frames, "frames", 2, fmt.Sprintf("with --once: snapshots to accumulate before rendering (max %d)", core.HistoryLen))
		flag.BoolVar(&cli.noReload, "no-hot-reload", false, "disable restart-on-rebuild (dev convenience)")
		flag.Int64Var(&cli.seed, "seed", 42, "demo RNG seed")
		flag.StringVar(&cli.sshKey, "ssh-key", "", "private key for ssh:// targets (overrides ~/.ssh/config)")
		flag.StringVar(&cli.bearer, "bearer", "", "bearer token sent to --add endpoints only (OmniRoute etc.)")
		flag.BoolVar(&cli.showVer, "version", false, "print version and exit")
		flag.BoolVar(&cli.showHelp, "help", false, "show help and exit")
		flag.BoolVar(&cli.showHelp, "h", false, "show help and exit")
		flag.Func("add", "attach an openai-compatible backend http(s) URL (repeatable, once per endpoint)", func(v string) error {
			return parseAdd(v, &cli.adds)
		})
		// Error paths (unknown flag, bad value) print this usage on stderr and
		// exit 2; -h/--help is handled below so it lands on stdout with exit 0.
		flag.Usage = func() { usage(os.Stderr) }
	})
	return &cli
}
