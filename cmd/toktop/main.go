// toktop: btop-style dashboard for LLM inference engines and the agents
// hammering them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/agentwatch"
	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/collector"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/demo"
	"github.com/maci0/toktop/internal/ingest"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/remote"
	"github.com/maci0/toktop/internal/selfreload"
	"github.com/maci0/toktop/internal/sysmon"
	"github.com/maci0/toktop/internal/ui"
)

// version is the release stamp. Empty means unstamped: init fills it from the
// module version Go embeds (`go install @v0.5.0`), then "dev". Release and
// `make build` override it with -ldflags "-X main.version=...", including the
// Makefile's default "dev". The source default must not be a real tag:
// "0.1.0" made `go install @latest` report the first release, so --version
// lied and `toktop update` always thought it was behind.
var version = ""

func init() {
	moduleVersion := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = bi.Main.Version
	}
	version = resolveVersion(version, moduleVersion)
}

// resolveVersion prefers any ldflags stamp, then the module version recorded
// at compile time. "(devel)" is a local tree, not a release. "dev" is a real
// stamp (`make build`); it must not be replaced with a VCS pseudo-version.
func resolveVersion(stamped, moduleVersion string) string {
	stamped = strings.TrimPrefix(stamped, "v")
	if stamped != "" {
		return stamped
	}
	moduleVersion = strings.TrimPrefix(moduleVersion, "v")
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return moduleVersion
	}
	return "dev"
}

// cliFlags is the top-level FlagSet. Registration is independent of
// flag.Parse so `toktop help` can PrintDefaults the same way `--help` does.
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
		flag.IntVar(&cli.probeSecs, "probe", 0, fmt.Sprintf("auto-probe every N seconds (0=off, max %d)", maxProbeSecs))
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
		flag.Func("add", "attach an openai-compatible backend http(s) URL (repeatable)", func(v string) error {
			return parseAdd(v, &cli.adds)
		})
		// Error paths (unknown flag, bad value) print this usage on stderr and
		// exit 2; -h/--help is handled below so it lands on stdout with exit 0.
		flag.Usage = func() { usage(os.Stderr) }
	})
	return &cli
}

func main() {
	f := registerFlags()
	// Subcommands taken before flag parsing so their own flags
	// (`update --check`) are not rejected by the top-level FlagSet, and so
	// `toktop help --anything` never dies as an unknown flag.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			code := runUpdate(ctx, os.Stdout, os.Args[2:])
			stop()
			os.Exit(code)
		case "help":
			os.Exit(runHelp(os.Stdout, os.Args[2:]))
		case "version":
			os.Exit(runVersion(os.Stdout, os.Args[2:]))
		}
	}
	flag.Parse()

	// Leftovers are forwarded so `toktop --help update` matches
	// `toktop help update`, and `toktop --version extra` is a usage error
	// like `toktop version extra`.
	if f.showHelp {
		os.Exit(runHelp(os.Stdout, flag.Args()))
	}
	if f.showVer {
		os.Exit(runVersion(os.Stdout, flag.Args()))
	}
	log.SetFlags(0)

	if err := validateFlags(f.once, f.interval, f.probeSecs, f.frames); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		os.Exit(2)
	}
	if err := validateLogLevelEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		os.Exit(2)
	}
	if f.once {
		if err := validateOnceEnv(); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			os.Exit(2)
		}
	}

	// Leftover args before the TTY check: a piped `toktop help` or
	// `toktop http://host` must name that mistake, not "stdout is not a terminal".
	cmd, remoteTargets, err := interpretArgs(flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch cmd {
	case "help":
		os.Exit(runHelp(os.Stdout, remoteTargets))
	case "version":
		os.Exit(runVersion(os.Stdout, nil))
	}

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	// Both halves of opencode's gate, resolved once before the config line:
	// the sqlite build tag decides whether the driver is linked in, and
	// --opencode-db (on unless explicitly disabled) whether it is opened.
	// EnableOpenCodeDB must run before any watcher for opencode is built.
	opencodeOn := f.agents && f.opencode && agentusage.EnableOpenCodeDB(true)
	if f.agents && f.opencode && !opencodeOn && explicit["opencode-db"] {
		// Silence here would look like an agent that generates nothing.
		fmt.Fprintln(os.Stderr, "toktop: --opencode-db needs a build with -tags sqlite; opencode will report no tokens")
	}
	warnUnknownEnv()
	warnIgnoredFlags(explicit, f.demo, f.once, f.agents, f.noIngest, len(f.adds), len(remoteTargets))
	warnIgnoredFrameEnv(f.once)
	warnUnusedEnv(explicit["bearer"], f.demo, f.noIngest, len(f.adds), len(remoteTargets))
	if !f.noIngest {
		if err := validateIngestAddr(f.ingest); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			os.Exit(2)
		}
	}
	if f.sshKey != "" && !f.demo && len(remoteTargets) > 0 {
		resolved, err := remote.ResolveKeyFile(f.sshKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toktop: --ssh-key: %v\n", err)
			os.Exit(2)
		}
		f.sshKey = resolved
	}

	if f.agents {
		if err := loadAgentDefs(); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			os.Exit(2)
		}
	}

	if !f.once && !term.IsTerminal(int(os.Stdout.Fd())) {
		// The live dashboard paints with alt-screen sequences; piped or
		// redirected they are garbage bytes in the capture, and --once is
		// the supported way to get output without a terminal.
		fmt.Fprintln(os.Stderr, "toktop: stdout is not a terminal; the live dashboard needs one (use --once for static output)")
		os.Exit(2)
	}

	logActiveConfig(os.Stderr, f, explicit, len(f.adds), len(remoteTargets), opencodeOn)

	// Only when --ingest was given explicitly should an unusable listen
	// address abort the run; the default-enabled endpoint degrades gracefully.
	ingestSet := explicit["ingest"]

	// Bearer token for gateways that require API keys (OmniRoute et al).
	// An explicit --bearer, even empty, wins so "not set" and "set to empty"
	// stay distinct; otherwise OMNIROUTE_API_KEY then TOKTOP_BEARER.
	if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
		bearer.Set(tok)
	}
	warnBearerFlag(explicit["bearer"], f.bearer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch := make(chan core.Snapshot, 8)
	var prober func()
	feedErr := make(chan string, 1) // carries the ingest endpoint's death to the UI

	// The agent event endpoint runs in every mode so harnesses can always
	// feed the dashboard.
	var recorder core.AgentRecorder
	// Endpoints toktop is already measuring. An agent generating through one
	// of them has its tokens reported by the engine, which sees every client;
	// counting the agent as well would double the total.
	var engineAddrs agentwatch.Engines
	feedAddr := "" // advertised by the UI only while the endpoint is live
	var demoSrc *demo.Source

	switch {
	case f.demo:
		demoSrc = demo.NewSource(f.interval, f.seed)
		go demoSrc.Run(ctx, ch)
		prober = demoSrc.ProbeAll
		recorder = demoSrc

	default:
		providers := provider.Discover(ctx)
		for _, raw := range f.adds {
			// The token rides only to endpoints the operator named: discovery
			// probes every well-known port on spec, and whatever answers there
			// must not be able to harvest the credential.
			bearer.Allow(raw)
			if p := provider.Attach(ctx, strings.TrimRight(raw, "/")); p.Poll != nil {
				providers = append(providers, p)
			} else {
				fmt.Fprintf(os.Stderr, "toktop: nothing recognized at %s; polling as generic openai anyway\n", raw)
				providers = append(providers, provider.NewOpenAICompat(raw, raw, core.KindOpenAI))
			}
		}

		var sysWrap func() core.SysSample
		for _, raw := range remoteTargets {
			tgt, err := remote.ParseTarget(raw)
			if err != nil {
				fmt.Fprintln(os.Stderr, "toktop:", err)
				os.Exit(2)
			}
			// Only when set: an empty flag must keep the IdentityFile
			// resolved from ~/.ssh/config by ParseTarget.
			if f.sshKey != "" {
				tgt.KeyFile = f.sshKey
			}
			rp, rsys, rerr := attachRemote(ctx, tgt)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "toktop: %v\n", rerr)
				continue
			}
			providers = append(providers, rp...)
			prev := sysWrap
			sysWrap = func() core.SysSample {
				var s core.SysSample
				if prev != nil {
					s = prev()
				} else {
					s = sysmon.Sample()
				}
				rsys.Merge(&s)
				return s
			}
			fmt.Fprintf(os.Stderr, "toktop: attached %d engine(s) via ssh on %s\n",
				len(rp), tgt.Host)
		}

		engineAddrs = func() []string {
			out := make([]string, 0, len(providers))
			for _, p := range providers {
				out = append(out, p.Addr)
			}
			return out
		}

		col := collector.New(providers, f.interval)
		if sysWrap != nil {
			col.SetSysFn(sysWrap)
		}
		go col.Run(ctx, ch)
		prober = col.ProbeAll
		recorder = col
	}

	if f.probeSecs > 0 && prober != nil {
		d := time.Duration(f.probeSecs) * time.Second
		go func() {
			t := time.NewTicker(d)
			defer t.Stop()
			prober()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if ctx.Err() != nil {
						return
					}
					prober()
				}
			}
		}()
	}

	// Agents running on this machine, read from the transcripts they already
	// write. This is the counterpart to the HTTP endpoint below: it needs no
	// cooperation from the agent, so a claude or codex started in a terminal
	// shows up without anyone wiring toktop into it.
	//
	// Opt-in, because it means scanning this machine's processes and reading
	// files the operator never pointed at toktop. Watching engines does not
	// imply consent to that.
	if f.agents && recorder != nil {
		// engineAddrs is nil in demo mode, where nothing real is measured.
		aw := agentwatch.New(recorder, engineAddrs)
		if demoSrc != nil {
			aw.SetNow(demoSrc.Now)
		}
		go aw.Run(ctx)
	}

	if !f.noIngest && recorder != nil {
		srv, err := ingest.New(f.ingest, recorder)
		if err != nil {
			if ingestSet {
				// The operator explicitly asked for this endpoint; continuing
				// would run the dashboard without the event feed they asked
				// for, with only a stderr line lost under the alt screen.
				fmt.Fprintf(os.Stderr, "toktop: --ingest %s unusable: %v\n", f.ingest, err)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "toktop: ingest disabled (%v)\n", err)
		} else {
			feedAddr = srv.Addr()
			if demoSrc != nil {
				srv.SetNow(demoSrc.Now)
			}
			if routableBind(feedAddr) {
				fmt.Fprintf(os.Stderr, "toktop: warning: ingest endpoint %s accepts unauthenticated events from any reachable peer\n", feedAddr)
			}
			go func() {
				if err := srv.Serve(); err != nil {
					fmt.Fprintf(os.Stderr, "toktop: ingest stopped: %v\n", err)
					select { // the alt screen hides stderr; tell the UI too
					case feedErr <- err.Error():
					default:
					}
				}
			}()
			defer srv.Close()
		}
	}

	cfg := ui.Config{
		Version:    version,
		Demo:       f.demo,
		IngestAddr: feedAddr,
		PollEvery:  f.interval,
		Prober:     prober,
		FeedErr:    feedErr,
		Agents:     f.agents,
	}

	if f.once {
		if code := runOnce(ctx, os.Stdout, cfg, ch, f.frames, f.plain); code != 0 {
			os.Exit(code)
		}
		return
	}

	runTUI(ctx, cfg, ch, !f.noReload)
}

// routableBind reports whether a bound listen address is reachable from
// beyond this host: a wildcard or non-loopback interface rather than
// loopback. The ingest endpoint authenticates nothing, so such a bind is
// worth naming at startup instead of leaving the widening silent.
func routableBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false // no port to split: treat as local, not as an alarm
	}
	if host == "" {
		return true // ":port" binds every interface
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback()
}

// runTUI runs the dashboard, restarting into a fresh binary whenever the
// executable on disk is rebuilt (dev hot-reload).
func runTUI(ctx context.Context, cfg ui.Config, ch <-chan core.Snapshot, hotReload bool) {
	self, selfErr := os.Executable()
	var (
		mu       sync.Mutex
		current  *tea.Program
		reloaded atomic.Bool
		ready    chan struct{}
	)
	if hotReload {
		if selfErr != nil {
			fmt.Fprintf(os.Stderr, "toktop: hot reload disabled (%v)\n", selfErr)
			hotReload = false
		} else {
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			// Closed once the program exists so a rebuild during NewProgram
			// still Quits instead of seeing current == nil and giving up.
			ready = make(chan struct{})
			go selfreload.Watch(wctx, self, 400*time.Millisecond, func() {
				reloaded.Store(true)
				select {
				case <-ready:
				case <-wctx.Done():
					return
				}
				mu.Lock()
				p := current
				mu.Unlock()
				if p != nil {
					p.Quit()
				}
			})
		}
	}

	prog := tea.NewProgram(ui.New(cfg, ch), tea.WithAltScreen())
	mu.Lock()
	current = prog
	mu.Unlock()
	if ready != nil {
		close(ready)
	}

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "toktop:", err)
		os.Exit(1)
	}
	if reloaded.Load() {
		fmt.Fprintln(os.Stderr, "toktop: binary changed, restarting…")
		selfreload.Restart(self, os.Args, os.Environ())
	}
}

// errInterrupted reports ctx cancellation while waiting for frames, so the
// caller can exit with the shell's SIGINT convention instead of the timeout
// message.
var errInterrupted = errors.New("interrupted")

// waitForFrames collects n snapshots, each allowed `wait` to arrive. It
// returns early on interrupt: a Ctrl+C during --once must kill the run at
// once, not leave it hanging until the timeout fires.
func waitForFrames(ctx context.Context, ch <-chan core.Snapshot, n int, wait time.Duration) (core.Snapshot, error) {
	var snap core.Snapshot
	for range n { // several ticks so charts carry some history
		t := time.NewTimer(wait)
		select {
		case s, ok := <-ch:
			t.Stop()
			if !ok {
				return snap, errors.New("snapshot channel closed waiting for telemetry")
			}
			snap = s
		case <-ctx.Done():
			t.Stop()
			return snap, errInterrupted
		case <-t.C:
			return snap, errors.New("timed out waiting for telemetry")
		}
	}
	return snap, nil
}

// runOnce prints a single rendered frame sized to the terminal (or 120x38).
// TOKTOP_COLUMNS / TOKTOP_LINES override detection (useful for capture);
// validateOnceEnv rejected unusable values before this runs. With plain, the
// frame is a linear text report instead of the dashboard layout: the braille
// chart rows and box-drawing borders of the visual frame read as noise (or
// silence) through a screen reader.
func runOnce(ctx context.Context, out io.Writer, cfg ui.Config, ch <-chan core.Snapshot, n int, plain bool) int {
	w, h := 120, 38
	if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil && tw >= minFrameColumns && th >= minFrameLines {
		w, h = min(tw, maxFrameColumns), min(th, maxFrameLines)
	}
	if v, set, err := frameEnv("TOKTOP_COLUMNS", minFrameColumns, maxFrameColumns); err == nil && set {
		w = v
	}
	if v, set, err := frameEnv("TOKTOP_LINES", minFrameLines, maxFrameLines); err == nil && set {
		h = v
	}
	// Snapshots land one poll interval apart, so a slow-polling host needs a
	// proportionally patient wait: a fixed cap would abort a healthy
	// --interval 10s run before its second frame ever arrives.
	wait := 5 * time.Second
	if d := 3 * cfg.PollEvery; d > wait {
		wait = d
	}
	snap, err := waitForFrames(ctx, ch, n, wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		if errors.Is(err, errInterrupted) {
			return 130
		}
		return 1
	}
	var frame string
	if plain {
		frame = ui.PlainTextFrame(cfg, snap)
	} else {
		frame = ui.StaticFrame(cfg, snap, w, h)
	}
	_, err = fmt.Fprintln(out, frame)
	return outputStatus(err)
}

func outputStatus(err error) int {
	if err == nil || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "toktop: write stdout: %v\n", err)
	return 1
}

func parseAdd(v string, dst *[]string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty URL")
	}
	if err := validateAddURL(v); err != nil {
		return err
	}
	*dst = append(*dst, v)
	return nil
}

// validateAddURL rejects values that cannot be polled as an OpenAI-compatible
// engine: missing scheme, non-http(s), no host, or credentials in the URL
// (those belong in --bearer / $TOKTOP_BEARER, not in argv or logs).
func validateAddURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL must be http:// or https://, got %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must be http:// or https://, got %q", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("URL missing host, got %q", raw)
	}
	if port := u.Port(); port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 0 || n > 65535 {
			return fmt.Errorf("URL port must be 0-65535, got %q", port)
		}
	}
	if u.User != nil {
		return errors.New("URL must not contain userinfo; set --bearer or $TOKTOP_BEARER")
	}
	return nil
}

// usage prints the full help screen: what toktop is, how to invoke it,
// worked examples, generated flag docs and where the env fallbacks live.
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
// so a typo does not dump the top-level screen and look like success.
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
			fmt.Fprintf(os.Stderr, "toktop help: unexpected argument %q (see 'toktop --help')\n", args[1])
			return 2
		}
		return runUpdate(context.Background(), out, []string{"--help"})
	case "version":
		if len(args) > 1 {
			fmt.Fprintf(os.Stderr, "toktop help: unexpected argument %q (see 'toktop --help')\n", args[1])
			return 2
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
		fmt.Fprintf(os.Stderr, "toktop version: unexpected argument %q (see 'toktop --help')\n", args[0])
		return 2
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
			return "", nil, fmt.Errorf("toktop version: unexpected argument %q (see 'toktop --help')", args[1])
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

// unexpectedArg names the leftover and how to fix it. Only http(s) URLs
// suggest --add; bare words point at --help.
func unexpectedArg(arg string) error {
	switch {
	case arg == "update":
		return fmt.Errorf("toktop: unexpected argument %q (the update subcommand must be first: toktop update)", arg)
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		return fmt.Errorf("toktop: unexpected argument %q (did you mean --add %s?)", arg, arg)
	default:
		return fmt.Errorf("toktop: unexpected argument %q (see 'toktop --help')", arg)
	}
}

// warnIgnoredFlags names flags passed explicitly but with no effect in the
// chosen mode: a silently dropped knob looks like a broken feature.
func warnIgnoredFlags(set map[string]bool, demo, once, agents, noIngest bool, nAdd, nRemote int) {
	if set["opencode-db"] && !agents {
		fmt.Fprintln(os.Stderr, "toktop: --opencode-db has no effect without --agents")
	}
	if set["ingest"] && noIngest {
		fmt.Fprintln(os.Stderr, "toktop: --ingest has no effect with --no-ingest")
	}
	if set["seed"] && !demo {
		fmt.Fprintln(os.Stderr, "toktop: --seed has no effect without --demo")
	}
	if set["frames"] && !once {
		fmt.Fprintln(os.Stderr, "toktop: --frames has no effect without --once")
	}
	if set["plain"] && !once {
		fmt.Fprintln(os.Stderr, "toktop: --plain has no effect without --once")
	}
	if set["no-hot-reload"] && once {
		fmt.Fprintln(os.Stderr, "toktop: --no-hot-reload has no effect with --once")
	}
	if demo && set["add"] {
		fmt.Fprintln(os.Stderr, "toktop: --add has no effect with --demo")
	}
	if demo && set["bearer"] {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect with --demo")
	}
	if set["bearer"] && !demo && nAdd == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --bearer has no effect without --add")
	}
	if demo && set["ssh-key"] {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect with --demo")
	}
	if demo && nRemote > 0 {
		fmt.Fprintln(os.Stderr, "toktop: ssh:// targets have no effect with --demo")
	}
	if set["ssh-key"] && !demo && nRemote == 0 {
		fmt.Fprintln(os.Stderr, "toktop: --ssh-key has no effect without an ssh:// target")
	}
}

// warnIgnoredFrameEnv names TOKTOP_COLUMNS / TOKTOP_LINES when they are set
// but --once is not running: the overrides only size static frames, and a
// silently ignored variable looks like a broken knob, same as a flag passed
// into a mode that never reads it.
func warnIgnoredFrameEnv(once bool) {
	if once {
		return
	}
	for _, name := range [...]string{"TOKTOP_COLUMNS", "TOKTOP_LINES"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			fmt.Fprintf(os.Stderr, "toktop: $%s has no effect without --once\n", name)
		}
	}
}

// warnUnusedEnv names secret and log-level variables that are set but will
// not be read in this mode, matching warnIgnoredFlags for the flag form.
func warnUnusedEnv(bearerFlag, demo, noIngest bool, nAdd, nRemote int) {
	if demo || nRemote == 0 {
		if os.Getenv("TOKTOP_SSH_PASSWORD") != "" {
			if demo {
				fmt.Fprintln(os.Stderr, "toktop: $TOKTOP_SSH_PASSWORD has no effect with --demo")
			} else {
				fmt.Fprintln(os.Stderr, "toktop: $TOKTOP_SSH_PASSWORD has no effect without an ssh:// target")
			}
		}
	}
	if !bearerFlag && (demo || nAdd == 0) {
		reason := "without --add"
		if demo {
			reason = "with --demo"
		}
		for _, name := range [...]string{"OMNIROUTE_API_KEY", "TOKTOP_BEARER"} {
			if os.Getenv(name) != "" {
				fmt.Fprintf(os.Stderr, "toktop: $%s has no effect %s\n", name, reason)
			}
		}
	}
	if noIngest && os.Getenv(ingest.LogLevelEnv) != "" {
		fmt.Fprintf(os.Stderr, "toktop: $%s has no effect with --no-ingest\n", ingest.LogLevelEnv)
	}
}

// maxProbeSecs caps auto-probe scheduling at 24h, well below the point where
// time.Duration(n)*time.Second overflows and NewTicker would panic.
const maxProbeSecs = 24 * 60 * 60

// Poll interval bounds apply after flag.Duration parses a unit-bearing value
// (or zero). The 50ms floor prevents excessive polling; PollTimeout is 1.5s.
// The 1h ceiling also bounds how long --once waits between frames.
const (
	minInterval = 50 * time.Millisecond
	maxInterval = time.Hour
)

// validateFlags rejects out-of-range values at startup: a running dashboard
// that ignores what it was asked to do is a misconfiguration nobody can see.
func validateFlags(once bool, interval time.Duration, probeSecs, frames int) error {
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", interval)
	}
	if interval < minInterval {
		if interval < time.Millisecond {
			return fmt.Errorf("--interval must be >= %s, got %s (bare numbers are nanoseconds; use 1s or 500ms)", minInterval, interval)
		}
		return fmt.Errorf("--interval must be >= %s, got %s", minInterval, interval)
	}
	if interval > maxInterval {
		return fmt.Errorf("--interval must be <= 1h, got %s", interval)
	}
	if probeSecs < 0 {
		return fmt.Errorf("--probe must be >= 0 (0 disables auto-probe), got %d", probeSecs)
	}
	if probeSecs > maxProbeSecs {
		return fmt.Errorf("--probe must be <= %d (seconds), got %d", maxProbeSecs, probeSecs)
	}
	if once && frames < 1 {
		return fmt.Errorf("--frames must be >= 1, got %d", frames)
	}
	if once && frames > core.HistoryLen {
		return fmt.Errorf("--frames must be <= %d (chart history length), got %d", core.HistoryLen, frames)
	}
	return nil
}

// validateLogLevelEnv rejects a set-but-unknown TOKTOP_LOG_LEVEL before the
// ingest logger is built. Empty means the info default.
func validateLogLevelEnv() error {
	_, err := ingest.ParseLogLevel(os.Getenv(ingest.LogLevelEnv))
	return err
}

// validateIngestAddr rejects listen addresses that are empty or not host:port
// before net.Listen sees them. An empty string is equivalent to ":0" (every
// interface, ephemeral port), which would silently expose the unauthenticated
// ingest endpoint; a missing port is a common typo for "use the default".
func validateIngestAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("--ingest address must be host:port, not empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--ingest address must be host:port, got %q", addr)
	}
	n, convErr := strconv.Atoi(port)
	if convErr != nil || n < 0 || n > 65535 {
		return fmt.Errorf("--ingest port must be 0-65535, got %q", port)
	}
	return nil
}

// resolveBearer returns the token sent to --add endpoints. An explicit
// --bearer (including empty) wins so clearing the token does not fall through
// to the environment; otherwise OMNIROUTE_API_KEY, then TOKTOP_BEARER.
func resolveBearer(flagVal string, flagSet bool) string {
	if flagSet {
		return flagVal
	}
	if v := os.Getenv("OMNIROUTE_API_KEY"); v != "" {
		return v
	}
	return os.Getenv("TOKTOP_BEARER")
}

// warnBearerFlag names the two --bearer footguns: a token on argv is
// readable from process listings, and an explicit empty value suppresses
// the env fallbacks rather than meaning "unset".
func warnBearerFlag(flagSet bool, flagVal string) {
	if !flagSet {
		return
	}
	if flagVal != "" {
		fmt.Fprintln(os.Stderr, "toktop: --bearer is visible in process listings; prefer $TOKTOP_BEARER or $OMNIROUTE_API_KEY")
		return
	}
	if os.Getenv("OMNIROUTE_API_KEY") != "" || os.Getenv("TOKTOP_BEARER") != "" {
		fmt.Fprintln(os.Stderr, "toktop: empty --bearer overrides $OMNIROUTE_API_KEY / $TOKTOP_BEARER")
	}
}

// Frame floors and caps for TOKTOP_COLUMNS / TOKTOP_LINES. Below the floors
// the static frame cannot lay out legibly. Above the caps, composeFrame would
// allocate a pane of newlines/cells big enough to OOM a capture from a typo
// (TOKTOP_LINES=1000000000). 1024x512 is larger than any real terminal.
const (
	minFrameColumns = 41
	minFrameLines   = 21
	maxFrameColumns = 1024
	maxFrameLines   = 512
)

// frameEnv reads one TOKTOP_COLUMNS / TOKTOP_LINES override. Unset or empty
// means default (set is false). Surrounding whitespace is ignored so a value
// copied with a trailing newline still parses, matching TOKTOP_LOG_LEVEL.
func frameEnv(name string, least, most int) (n int, set bool, err error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false, nil
	}
	n, convErr := strconv.Atoi(v)
	if convErr != nil || n < least || n > most {
		return 0, false, fmt.Errorf("$%s must be an integer %d-%d, got %q", name, least, most, v)
	}
	return n, true, nil
}

// validateOnceEnv rejects a set-but-unusable frame override before --once
// renders: a capture sized by a typo'd variable must fail loudly rather than
// come out at the fallback size with nothing explaining why. Unset or empty
// means default, matching how every other optional setting reads here.
func validateOnceEnv() error {
	for _, e := range [...]struct {
		name        string
		least, most int
	}{
		{"TOKTOP_COLUMNS", minFrameColumns, maxFrameColumns},
		{"TOKTOP_LINES", minFrameLines, maxFrameLines},
	} {
		if _, _, err := frameEnv(e.name, e.least, e.most); err != nil {
			return err
		}
	}
	return nil
}

// logActiveConfig writes one startup line of the knobs that will actually
// apply. Secrets are named as set/unset, never printed. The live dashboard
// hides stderr under the alt screen; --once and a journal after quit keep it.
// opencodeOn is the resolved gate, not the flag: a build without the sqlite
// driver reads no opencode database however the flag is set.
func logActiveConfig(w io.Writer, f *cliFlags, explicit map[string]bool, nAdd, nRemote int, opencodeOn bool) {
	var b strings.Builder
	b.WriteString("toktop: interval=")
	b.WriteString(f.interval.String())
	if f.noIngest {
		b.WriteString(" ingest=off")
	} else {
		b.WriteString(" ingest=")
		b.WriteString(f.ingest)
	}
	if f.demo {
		b.WriteString(" demo")
	}
	if f.agents {
		b.WriteString(" agents")
		if opencodeOn {
			b.WriteString(" opencode-db")
		}
	}
	if f.once {
		b.WriteString(" once")
		if f.plain {
			b.WriteString(" plain")
		}
	}
	if f.probeSecs > 0 {
		fmt.Fprintf(&b, " probe=%ds", f.probeSecs)
	}
	if nRemote > 0 && !f.demo {
		fmt.Fprintf(&b, " ssh=%d", nRemote)
	}
	if nAdd > 0 && !f.demo {
		if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
			b.WriteString(" bearer=set")
		}
	}
	fmt.Fprintln(w, b.String())
}

// toktopEnvVars are the TOKTOP_* names this process recognizes. Most are
// read here; TOKTOP_SCREENSHOT_FONT is used only by scripts/screenshot.py
// and is listed so a developer export is not reported as a typo.
// See also OMNIROUTE_API_KEY, SSH_AUTH_SOCK and the ssh defaults.
var toktopEnvVars = map[string]bool{
	"TOKTOP_BEARER":          true,
	"TOKTOP_SSH_PASSWORD":    true,
	"TOKTOP_COLUMNS":         true,
	"TOKTOP_LINES":           true,
	"TOKTOP_LOG_LEVEL":       true,
	"TOKTOP_SCREENSHOT_FONT": true, // scripts/screenshot.py; this binary ignores it
}

// warnUnknownEnv reports unrecognized TOKTOP_* variables once at startup:
// a misspelled knob would otherwise be ignored silently and look like a
// no-op feature.
func warnUnknownEnv() {
	var unknown []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "TOKTOP_") || toktopEnvVars[name] {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "toktop: ignoring unknown environment variable(s): %s\n",
			strings.Join(unknown, ", "))
	}
}

// loadAgentDefs pulls in ~/.gauntlet/agents.json so --agents can follow
// agents toktop was not built to know (in-house wrappers, the pi family),
// including where they keep their transcripts. A missing file is the normal
// case; a malformed or unreadable one is returned so the caller can refuse to
// start: agents silently missing from the watch look exactly like agents
// doing nothing.
func loadAgentDefs() error {
	path := agentusage.DefinitionsPath()
	if path == "" {
		return nil
	}
	return agentusage.LoadDefinitions(path)
}

// attachRemote connects to an ssh target, discovers engines, relays their
// ports through the connection and starts remote stats sampling. Everything
// shares one in-process ssh client; its death mid-run is reported once.
func attachRemote(ctx context.Context, tgt remote.Target) ([]provider.Provider, *remote.Stats, error) {
	cli, err := remote.Connect(ctx, tgt)
	if err != nil {
		return nil, nil, err
	}

	wellKnown := provider.CandidatePorts()
	disc, err := remote.Discover(ctx, cli, wellKnown)
	if err != nil {
		cli.Close()
		return nil, nil, err
	}
	ports := disc.ForwardSet(wellKnown)
	if len(ports) == 0 {
		cli.Close()
		return nil, nil, fmt.Errorf("no inference ports listening on %s", tgt.Host)
	}
	fwd, err := cli.Forward(ports)
	if err != nil {
		cli.Close()
		return nil, nil, err
	}
	// Forward skips a port whose local listener cannot be bound; without
	// this line the engine behind it silently vanishes from the dashboard.
	for _, p := range ports {
		if _, ok := fwd[p]; !ok {
			fmt.Fprintf(os.Stderr, "toktop: %s:%d could not be forwarded locally; engines on that port are invisible\n",
				tgt.Host, p)
		}
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-cli.Done():
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "toktop: ssh connection to %s lost (%v)\n", tgt.Host, cli.Err())
			}
		}
		// Close on both paths: watchClose reclaims listeners after a drop,
		// but nothing else tears the client down, and a cancelled Run
		// context would otherwise leave the conn, keepalive, and any
		// still-bound forwards until process exit.
		cli.Close()
	}()

	// Ascending remote ports: backend order must not depend on map iteration.
	rports := slices.Sorted(maps.Keys(fwd))
	bases := make([]string, len(rports))
	for i, rport := range rports {
		bases[i] = fmt.Sprintf("http://127.0.0.1:%d", fwd[rport])
	}
	// Identify concurrently: provider fans the probes out per candidate.
	kinds := provider.IdentifyAll(ctx, bases)

	var providers []provider.Provider
	var skipped []int
	for i, kind := range kinds {
		if kind != "" {
			label := fmt.Sprintf("%s:%d", tgt.Host, rports[i])
			p := provider.NewOpenAICompat(bases[i], label, kind)
			if kind == core.KindOllama {
				p = provider.NewOllama(bases[i])
				p.Label = label
			}
			providers = append(providers, p)
			continue
		}
		skipped = append(skipped, rports[i])
	}
	for _, p := range skipped {
		fmt.Fprintf(os.Stderr, "toktop: %s:%d is listening but speaks no recognized engine API; skipping\n",
			tgt.Host, p)
	}
	stats := &remote.Stats{Client: cli}
	go stats.Run(ctx, 5*time.Second)
	return providers, stats, nil
}
