// Command toktop watches local and remote inference engines, reports token
// throughput and host vitals, and accepts usage events from AI coding agents.
//
// It is the only package allowed to wire the rest of the module together: every
// internal package is a leaf that something here starts and stops, so the
// layering in docs/ARCHITECTURE.md is enforced rather than merely described.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
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
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/remote"
	"github.com/maci0/toktop/internal/selfreload"
	"github.com/maci0/toktop/internal/ui"
)

func main() { os.Exit(runMain()) }

// runMain carries the exit code rather than exiting. os.Exit runs no defers,
// so an exit from the body below released the signal context and the ingest
// listener by kernel teardown alone, and the deferred srv.Close read as a
// guaranteed close when nothing called it.
func runMain() int {
	// A reader that closes the pipe early (`toktop version | true`,
	// `toktop --once | head -1`) must leave the exit code at 0, which is what
	// the help screen promises and what outputStatus returns for a broken
	// pipe. The Go runtime re-raises SIGPIPE with its default disposition for
	// a write to stdout, so the process died of signal 13 (141 to a shell)
	// before the write ever returned the EPIPE isBrokenPipe looks for. With
	// SIGPIPE ignored the write returns EPIPE instead, and the documented
	// contract holds whatever the reader does.
	signal.Ignore(syscall.SIGPIPE)

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
			return code
		case "help":
			return runHelp(os.Stdout, os.Args[2:])
		case "version":
			return runVersion(os.Stdout, os.Args[2:])
		}
	}
	// The flag package reports a bad flag in its own single-dash spelling;
	// flagParseError restores the long form so the error names the flag as
	// the help screen and the README document it.
	if err := topFS.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %s%s%s\n", flagParseError(err), valueHint(err), subcommandFlagHint(err))
		usage(os.Stderr)
		return 2
	}

	// Leftovers are forwarded so `toktop --help update` matches
	// `toktop help update`, and `toktop --version extra` is a usage error
	// like `toktop version extra`.
	if f.showHelp {
		return runHelp(os.Stdout, topFS.Args())
	}
	if f.showVer {
		return runVersion(os.Stdout, topFS.Args())
	}
	log.SetFlags(0)

	if err := validateFlags(f.once, f.interval, f.probeSecs, f.frames); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		return 2
	}
	if err := validateLogLevelEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		return 2
	}
	// Parsed before anything reads it: a bad --origin aborts the run rather
	// than leaving the demo on the wall clock, which is the one input the
	// operator pinned the origin to remove.
	origin, err := parseOrigin(f.origin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		return 2
	}
	// agentusage is a package other programs embed, so it audits through the
	// process logger until a host hands it one. Here that host is toktop:
	// after this line a transcript walk that could not finish lands in the
	// same audit stream as everything else, at the floor validated above.
	agentusage.SetLogger(logcfg.Logger())
	// The plain report and the JSON report render no sized frame, so a frame
	// override is named as unused there rather than validated: rejecting
	// TOKTOP_COLUMNS=10 for a frame that is never composed aborts a run
	// whose output does not read the variable at all.
	if f.once && !f.plain && !f.jsonOut {
		if err := validateOnceEnv(); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			return 2
		}
	}

	// Leftover args before the TTY check: a piped `toktop help` or
	// `toktop http://host` must name that mistake, not "stdout is not a terminal".
	cmd, remoteTargets, err := interpretArgs(topFS.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch cmd {
	case "help":
		return runHelp(os.Stdout, remoteTargets)
	case "version":
		return runVersion(os.Stdout, nil)
	}
	// Targets are parsed here, before the TTY check below, so a malformed
	// ssh:// URL is named as the mistake it is rather than reported as
	// "stdout is not a terminal" when the run is piped or redirected.
	targets, dupTargets, err := remote.ParseTargets(remoteTargets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "toktop:", err)
		return 2
	}
	for _, dup := range dupTargets {
		fmt.Fprintf(os.Stderr, "toktop: %s named more than once; attaching it once\n", dup.Host)
	}

	explicit := map[string]bool{}
	topFS.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if err := validateSSHKeyFlag(explicit["ssh-key"], f.sshKey); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		return 2
	}
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
	warnIgnoredFlags(explicit, f, len(f.adds), len(remoteTargets))
	warnIgnoredFrameEnv(f.once, f.plain, f.jsonOut)
	warnUnusedEnv(explicit["bearer"], f.demo, f.noIngest, f.agents, len(f.adds), len(remoteTargets))
	warnIgnoredGauntletHome(f.agents)
	warnIgnoredXDGHome(opencodeOn, !f.demo && len(remoteTargets) > 0, f.agents)
	if !f.noIngest {
		if err := validateIngestAddr(f.ingest); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			return 2
		}
	}
	if f.sshKey != "" && !f.demo && len(remoteTargets) > 0 {
		resolved, err := remote.ResolveKeyFile(f.sshKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toktop: --ssh-key: %v\n", err)
			return 2
		}
		f.sshKey = resolved
	}

	if f.agents {
		if err := loadAgentDefs(); err != nil {
			// Every LoadDefinitions failure names the file it read, which
			// lives under $HOME unless GAUNTLET_HOME says otherwise, so the
			// home is folded to "~": this line is the only record of what is
			// wrong with the file, and it gets pasted into issues.
			fmt.Fprintf(os.Stderr, "toktop: %s\n", core.RedactHome(err.Error()))
			return 2
		}
		// The definitions file was found, so a home that cannot place the
		// built-in stores did not stop the run; say so rather than leaving
		// every compiled-in agent reporting no tokens.
		warnIgnoredUserHome()
	}

	if !f.once && !term.IsTerminal(int(os.Stdout.Fd())) {
		// The live dashboard paints with alt-screen sequences; piped or
		// redirected they are garbage bytes in the capture, and --once is
		// the supported way to get output without a terminal.
		fmt.Fprintln(os.Stderr, "toktop: stdout is not a terminal; the live dashboard needs one (use --once for static output)")
		return 2
	}

	// targets, not remoteTargets: remote.ParseTargets collapsed the repeated
	// spellings of one host, so counting the raw arguments reported more
	// ssh connections than the run opened.
	logActiveConfig(os.Stderr, f, explicit, len(f.adds), len(targets), opencodeOn)

	// Only when --ingest was given explicitly should an unusable listen
	// address abort the run; the default-enabled endpoint degrades gracefully.
	ingestSet := explicit["ingest"]

	// Bearer token for gateways that require API keys (OmniRoute et al).
	// An explicit --bearer, even empty, wins so "not set" and "set to empty"
	// stay distinct; otherwise OMNIROUTE_API_KEY then TOKTOP_BEARER.
	if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
		if err := bearer.Set(tok); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v; the engine will be queried unauthenticated\n", err)
		} else {
			// Only when a token is in force: the cleartext warning is about
			// the token crossing the network, and a refused one crosses
			// nothing. Warning anyway trains the operator to read this line
			// as routine rather than as the credential warning it is.
			warnInsecureAdd(f.adds)
		}
	}
	warnBearerFlag(explicit["bearer"], f.bearer)
	warnBlankBearer(len(f.adds), f.demo)

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
		// Pinned before the first Now: the origin is the one input the seed
		// does not decide, and left on the wall clock two runs of one seed
		// differ in every stamp while agreeing on every value.
		if !origin.IsZero() {
			demoSrc.SetOrigin(origin)
		}
		// Armed before Run, which the cadence can no longer be added to
		// afterwards. A wall-clock ticker here instead would make how many
		// waves ran, and the instant each is stamped, a function of how long
		// the process happened to take, and the seed plus origin would stop
		// replaying the run.
		if f.probeSecs > 0 {
			demoSrc.ProbeEvery(time.Duration(f.probeSecs) * time.Second)
		}
		go demoSrc.Run(ctx, ch)
		prober = demoSrc.ProbeAll
		recorder = demoSrc

	default:
		providers, sysFn, err := attachEngines(ctx, f, targets)
		if err != nil {
			fmt.Fprintln(os.Stderr, "toktop:", err)
			return 2
		}

		engineAddrs = func() []string {
			out := make([]string, 0, len(providers))
			for _, p := range providers {
				out = append(out, p.Addr)
			}
			return out
		}

		col := collector.New(providers, f.interval)
		if sysFn != nil {
			col.SetSysFn(sysFn)
		}
		go col.Run(ctx, ch)
		prober = col.ProbeAll
		recorder = col

		if f.probeSecs > 0 {
			// Fire one probe at startup rather than waiting out the first
			// interval: --probe should put a request on the wire when the
			// dashboard comes up. Nothing joins the returned channel, so the
			// loop ends with ctx.
			core.Tick(ctx, time.Duration(f.probeSecs)*time.Second, prober, prober)
		}
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
		// Run has no error return, so an engine address that will not parse
		// is reported here. Left unreported it silently double counts every
		// agent's tokens against the engine it is already generating through.
		// The UI shows the condition; the audit log keeps it, because the
		// banner is gone with the run and a watch that stopped following an
		// agent looks the same as one that never saw it.
		aw.SetOnError(func(err error) {
			logcfg.Logger().Warn("toktop: agent watch failed",
				"error", logcfg.Field(core.RedactHome(err.Error()), 256))
			select {
			case feedErr <- "agent watch: " + err.Error():
			default:
			}
		})
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
				return 2
			}
			fmt.Fprintf(os.Stderr, "toktop: ingest disabled (%v)\n", err)
			// The dashboard comes up without the event feed the operator asked
			// for by default, and the only reason for it lives under the alt
			// screen: a run that silently ingests nothing looks exactly like a
			// run whose agents post nothing.
			logcfg.Logger().Warn("toktop: ingest disabled",
				"addr", logcfg.Field(f.ingest, 256),
				"error", logcfg.Field(logcfg.RedactAddrs(err.Error()), 256))
		} else {
			feedAddr = srv.Addr()
			if demoSrc != nil {
				srv.SetNow(demoSrc.Now)
			}
			// The address the endpoint actually bound, not the one the config
			// line asked for: a --ingest on port 0 names an ephemeral port only
			// this run knows. Without it the audit log holds the request and no
			// way to post to what answered it.
			logcfg.Logger().Info("toktop: ingest listening", "addr", logcfg.Field(feedAddr, 256))
			if routableBind(feedAddr) {
				fmt.Fprintf(os.Stderr, "toktop: warning: ingest endpoint %s accepts unauthenticated events from any reachable peer\n", feedAddr)
				// The one state change in the run that widens who can write to
				// this machine's feed, and the only record of it was a stderr
				// line the alt screen hides for the life of the run.
				logcfg.Logger().Warn("toktop: ingest bound off loopback",
					"addr", logcfg.Field(feedAddr, 256))
			}
			go func() {
				if err := srv.Serve(); err != nil {
					// Serve already audited the failure on the ingest logger,
					// so stderr gets one line from here at most and the UI
					// needs its own: the alt screen hides stderr, and the
					// channel carries the agent watch's failures too, so the
					// message names the subsystem that stopped.
					select {
					case feedErr <- "ingest stopped: " + err.Error():
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
	if demoSrc != nil {
		cfg.DemoSeed = demoSrc.Seed()
		cfg.DemoOrigin = origin
	}

	if f.once {
		if code := runOnce(ctx, os.Stdout, cfg, ch, f.frames, f.plain, f.jsonOut); code != 0 {
			return code
		}
		return 0
	}

	return runTUI(ctx, cfg, ch, !f.noReload)
}

// runTUI runs the dashboard, restarting into a fresh binary whenever the
// executable on disk is rebuilt (dev hot-reload). It returns the process exit
// code: 0 for the q and Ctrl+C quit, 130 for a signal that reached the run
// context, 1 for a dashboard that could not run.
func runTUI(ctx context.Context, cfg ui.Config, ch <-chan core.Snapshot, hotReload bool) int {
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

	// WithContext is what makes a signal from outside the process stop the
	// dashboard. main registers SIGINT and SIGTERM on ctx, which replaces
	// their default dispositions, so without it a `kill` left the program on
	// screen with every backend already canceled behind it: no input could
	// reach it on a terminal that had scrolled away, and it never exited.
	prog := tea.NewProgram(ui.New(cfg, ch), tea.WithAltScreen(), tea.WithContext(ctx))
	mu.Lock()
	current = prog
	mu.Unlock()
	if ready != nil {
		close(ready)
	}

	if _, err := prog.Run(); err != nil {
		if code, settled := tuiExit(ctx); settled {
			return code
		}
		fmt.Fprintln(os.Stderr, "toktop:", err)
		return 1
	}
	if reloaded.Load() {
		fmt.Fprintln(os.Stderr, "toktop: binary changed, restarting…")
		// The exec below replaces this process, so the audit log holds the two
		// startup lines of the run it ends and the two of the run that follows
		// with nothing between them. The line names the gap: a log read across
		// a hot reload would otherwise show one dashboard that stopped and
		// another that began, with nothing connecting the two.
		logcfg.Logger().Info("toktop: binary changed, restarting",
			"path", logcfg.Field(self, 1024))
		selfreload.Restart(self, os.Args, os.Environ())
	}
	return 0
}

// tuiExit maps a canceled run context onto toktop's exit code, and reports
// whether it settles the run at all. A canceled ctx is the one main's signal
// handler writes to: the dashboard was stopped from outside, and that is the
// 130 the --once and update paths already use for the same signal rather than
// the 1 a failure gets. A hot-reload Quit is not a cancellation (the ctx is
// untouched there, and the reload is what follows), and a dashboard that quit
// on q or Ctrl+C leaves the ctx live, so both fall through to the 0.
func tuiExit(ctx context.Context) (code int, settled bool) {
	if ctx.Err() == nil {
		return 0, false
	}
	return 130, true
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
// silence) through a screen reader. With jsonOut, the snapshot itself is
// printed instead of either report, for a script reading the numbers.
func runOnce(ctx context.Context, out io.Writer, cfg ui.Config, ch <-chan core.Snapshot, n int, plain, jsonOut bool) int {
	// Only the visual frame is sized. The JSON report is one object and the
	// plain report a linear list, so neither composes a frame: the terminal
	// size and TOKTOP_COLUMNS / TOKTOP_LINES have nothing to fill, which is
	// why validateOnceEnv and warnIgnoredFrameEnv both leave them alone in
	// those two modes. Reading them here anyway would size a value nothing
	// consumes.
	w, h := 120, 38
	if !plain && !jsonOut {
		if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil && tw >= frameColumnsMin && th >= frameLinesMin {
			w, h = min(tw, frameColumnsMax), min(th, frameLinesMax)
		}
		if v, set, err := frameEnv("TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax); err == nil && set {
			w = v
		}
		if v, set, err := frameEnv("TOKTOP_LINES", frameLinesMin, frameLinesMax); err == nil && set {
			h = v
		}
	}
	// Snapshots land one poll interval apart, so a slow-polling host needs a
	// proportionally patient wait: a fixed cap would abort a healthy
	// --interval 10s run before its second frame ever arrives.
	wait := max(5*time.Second, 3*cfg.PollEvery)
	snap, err := waitForFrames(ctx, ch, n, wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
		if errors.Is(err, errInterrupted) {
			return 130
		}
		return 1
	}
	if jsonOut {
		report, err := ui.JSONFrame(cfg, snap)
		if err != nil {
			// A marshal failure is toktop's own, not the operator's, so it
			// is a runtime failure (1) rather than a usage error.
			fmt.Fprintf(os.Stderr, "toktop: render JSON: %v\n", err)
			return 1
		}
		_, err = fmt.Fprintln(out, report)
		return outputStatus(err)
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
	if err == nil || isBrokenPipe(err) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "toktop: write stdout: %v\n", err)
	return 1
}

// configLog is where the startup config record goes. A var so a test can read
// it, the same reason attach.go has attachLog.
var configLog = logcfg.Logger

// configFlag is one knob of the startup line: the name the audit record gives
// it, the value it takes, and whether the stderr line prints it bare. A knob
// not in force is absent from the list, so the prose line and the audit record
// are rendered from one derivation and cannot name different things.
type configFlag struct {
	key   string
	value string
	bare  bool
}

// activeConfig resolves the knobs that will actually apply. Secrets are named
// as set/unset, never carried. opencodeOn is the resolved gate, not the flag: a
// build without the sqlite driver reads no opencode database however the flag
// is set.
func activeConfig(f *cliFlags, explicit map[string]bool, nAdd, nRemote int, opencodeOn bool) []configFlag {
	cfg := []configFlag{{key: "interval", value: f.interval.String()}}
	if f.noIngest {
		cfg = append(cfg, configFlag{key: "ingest", value: "off"})
	} else {
		cfg = append(cfg, configFlag{key: "ingest", value: f.ingest})
	}
	if lvl := strings.TrimSpace(os.Getenv(logcfg.LevelEnv)); lvl != "" {
		// The resolved level, not the raw string: "warning" and "WARN" print
		// as warn, so the line matches what the audit log actually applies.
		// main rejects an unparseable value, so the error case is unreachable.
		if parsed, err := logcfg.ParseLogLevel(lvl); err == nil {
			cfg = append(cfg, configFlag{key: "log", value: logcfg.LogLevelName(parsed)})
		}
	}
	if f.demo {
		cfg = append(cfg, configFlag{key: "demo", bare: true})
		// The two inputs a demo run replays from, on the startup line and in
		// the audit log. A run that ends in a crash leaves the log behind and
		// nothing else: the report that carries the seed is never written, so
		// without these the run that produced the lines around them cannot be
		// reproduced from what survived it. The origin is recorded as the flag
		// value the operator gave, so the replay is a copy of the command
		// rather than a re-derivation of it.
		cfg = append(cfg, configFlag{key: "seed", value: strconv.FormatInt(f.seed, 10)})
		if f.origin != "" {
			cfg = append(cfg, configFlag{key: "origin", value: f.origin})
		}
	}
	if f.agents {
		cfg = append(cfg, configFlag{key: "agents", bare: true})
		if opencodeOn {
			cfg = append(cfg, configFlag{key: "opencode-db", bare: true})
		}
	}
	if f.once {
		cfg = append(cfg, configFlag{key: "once", bare: true})
		// Only the report that is actually rendered is named: --json
		// replaces the text report, so a line reading "once plain json"
		// claims a knob is in force that the run ignored.
		if f.plain && !f.jsonOut {
			cfg = append(cfg, configFlag{key: "plain", bare: true})
		}
		if f.jsonOut {
			cfg = append(cfg, configFlag{key: "json", bare: true})
		}
	}
	if f.probeSecs > 0 {
		cfg = append(cfg, configFlag{key: "probe", value: fmt.Sprintf("%ds", f.probeSecs)})
	}
	if nRemote > 0 && !f.demo {
		cfg = append(cfg, configFlag{key: "ssh", value: strconv.Itoa(nRemote)})
	}
	if nAdd > 0 && !f.demo {
		if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
			// set or refused, never bare "set": bearer.Set runs after this
			// line and turns down a token carrying CR or LF, leaving the run
			// to query the --add endpoints unauthenticated. A record reading
			// bearer=set there describes a credential that is not in force,
			// and the 401s it explains arrive with nothing naming the cause.
			state := "set"
			if !bearer.Usable(tok) {
				state = "refused"
			}
			cfg = append(cfg, configFlag{key: "bearer", value: state})
		}
	}
	return cfg
}

// logActiveConfig writes the knobs of the run twice: as the startup line on w,
// and as one record in the audit log. The live dashboard hides stderr under the
// alt screen, and every other subsystem (the collector, the ssh client, the
// agent watch, the ingest endpoint) audits there, so a run whose config never
// reaches the audit log is a run nobody can reconstruct from it: the record
// says which interval, which endpoints and which log floor produced the lines
// around it.
func logActiveConfig(w io.Writer, f *cliFlags, explicit map[string]bool, nAdd, nRemote int, opencodeOn bool) {
	cfg := activeConfig(f, explicit, nAdd, nRemote, opencodeOn)
	var b strings.Builder
	b.WriteString("toktop:")
	attrs := make([]any, 0, len(cfg))
	for _, c := range cfg {
		if c.bare {
			fmt.Fprintf(&b, " %s", c.key)
			attrs = append(attrs, c.key, true)
			continue
		}
		fmt.Fprintf(&b, " %s=%s", c.key, c.value)
		attrs = append(attrs, c.key, logcfg.Field(c.value, 128))
	}
	fmt.Fprintln(w, b.String())
	configLog().Info("toktop: config", attrs...)
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
	logcfg.LevelEnv:          true,
	"TOKTOP_SCREENSHOT_FONT": true, // scripts/screenshot.py; this binary ignores it
}

// warnUnknownEnv reports unrecognized TOKTOP_* variables once at startup:
// a misspelled knob would otherwise be ignored silently and look like a
// no-op feature. Sorted, so the same set of names reads the same way in
// every capture of the startup output.
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
		slices.Sort(unknown)
		fmt.Fprintf(os.Stderr, "toktop: ignoring unknown environment variable(s): %s\n",
			strings.Join(reportedNames(unknown), ", "))
	}
}

// maxReportedName caps one externally supplied name printed in a startup
// warning. The names come from a definitions file and from the environment,
// so a wrapper or a supervisor can put a megabyte-long key in one, and the
// startup output is read in a terminal and pasted into issues.
const maxReportedName = 64

// reportedField makes one externally supplied name safe to print on stderr.
// Both name sets are text this program did not write: a key out of
// ~/.gauntlet/agents.json and a name out of the environment. Untreated, an
// escape sequence in either reaches the operator's terminal, and an
// unknown-key warning is the first thing a run prints, so it is the cheapest
// place in the tree to plant a clipboard write or a title change. SanitizeText
// drops the sequences, the bidi controls and the zero-width marks that render
// one key as another, and the cap bounds what one name can spend, both
// cutting between grapheme clusters so a name ending in an emoji or a
// decomposed accent is never sliced mid-character.
func reportedField(s string) string {
	return core.TruncateClusters(core.SingleLine(s), maxReportedName)
}

// loadAgentDefs pulls in ~/.gauntlet/agents.json so --agents can follow
// agents toktop was not built to know (in-house wrappers, the pi family),
// including where they keep their transcripts. A missing file is the normal
// case; a malformed or unreadable one is returned so the caller can refuse to
// start: agents silently missing from the watch look exactly like agents
// doing nothing.
//
// An empty path is the home directory lookup failing inside DefinitionsPath.
// It is an error, not an absent file: without the path no agents file is read
// at all, and a startup that says nothing about it leaves the operator looking
// at a watch that reports no in-house agents for the whole run.
func loadAgentDefs() error {
	path := agentusage.DefinitionsPath()
	if path == "" {
		return errors.New("cannot locate agents.json: no home directory and no absolute GAUNTLET_HOME")
	}
	if err := agentusage.LoadDefinitions(path); err != nil {
		return err
	}
	warnUnknownUsageKeys(path)
	return nil
}

// warnUnknownUsageKeys names the usage keys a definitions file spells that
// agentusage has no field for. The file is gauntlet's, so an unrecognized key
// is a version this build is older than rather than an error, and refusing to
// start over one would break a run on a newer gauntlet than this build. It is
// still named, because a key nobody reads is usually a key nobody spelled: one
// with no counterpart in the known set leaves the agent with whatever the rest
// of its block said, and a block naming only that key registers no transcripts
// at all, which on the dashboard is an agent that used no tokens.
//
// One line, on stderr with the rest of the startup warnings, with the home
// folded out of the path the way every other line naming the file folds it.
func warnUnknownUsageKeys(path string) {
	unknown := agentusage.UnknownUsageKeys()
	if len(unknown) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: %s: ignoring unknown usage key(s) %s (this build reads %s)\n",
		core.RedactHome(path), strings.Join(reportedNames(unknown), ", "),
		strings.Join(agentusage.UsageKeyNames(), ", "))
}

// reportedNames is reportedField over a list, so both startup warnings read
// one sorted list of names the same way.
func reportedNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = reportedField(name)
	}
	return out
}
