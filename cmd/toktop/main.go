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
		case "completion":
			return runCompletion(os.Stdout, os.Args[2:])
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
	case "completion":
		return runCompletion(os.Stdout, remoteTargets)
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
	// targets, not remoteTargets, in every remote count below: see the note by
	// logActiveConfig. ParseTargets only collapses repeats, so no branch here
	// changes answer, but a count that means "ssh connections this run opens"
	// has to come from one place.
	warnIgnoredFlags(explicit, f, len(f.adds), len(targets))
	warnIgnoredFrameEnv(f.once, f.plain, f.jsonOut)
	warnUnusedEnv(explicit["bearer"], f.demo, f.noIngest, f.agents, len(f.adds), len(targets))
	warnIgnoredGauntletHome(f.agents)
	warnIgnoredXDGHome(opencodeOn, !f.demo && len(targets) > 0, f.agents)
	if !f.noIngest {
		if err := validateIngestAddr(f.ingest); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			return 2
		}
	}
	if f.sshKey != "" && !f.demo && len(targets) > 0 {
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
	// inForce records whether a token was actually installed, since a token
	// Set refuses crosses no request and the blank-variable warning below
	// reports against what the run sends rather than what was offered.
	inForce := false
	if tok := resolveBearer(f.bearer, explicit["bearer"]); tok != "" {
		if err := bearer.Set(tok); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v; the engine will be queried unauthenticated\n", err)
		} else {
			inForce = true
			// Only when a token is in force: the cleartext warning is about
			// the token crossing the network, and a refused one crosses
			// nothing. Warning anyway trains the operator to read this line
			// as routine rather than as the credential warning it is.
			warnInsecureAdd(f.adds)
		}
	}
	warnBearerFlag(explicit["bearer"], f.bearer)
	warnBlankBearer(len(f.adds), f.demo, inForce)

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
		go func() {
			// Run's only error is a second concurrent Run, and the collector
			// has no other supervisor to notice: dropped, the dashboard came
			// up collecting nothing and said nothing.
			if err := col.Run(ctx, ch); err != nil {
				logcfg.Logger().Error("toktop: collector stopped",
					"error", logcfg.Field(core.RedactHome(err.Error()), 256))
				select {
				case feedErr <- "collector stopped: " + core.RedactHome(err.Error()):
				default:
				}
			}
		}()
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
			case feedErr <- "agent watch: " + core.RedactHome(err.Error()):
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
					case feedErr <- "ingest stopped: " + core.RedactHome(err.Error()):
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
		// --plain without --once is the live text report: the same words in
		// the same order, redrawn in place every frame, so a screen-reader
		// user gets the running dashboard rather than one frozen snapshot.
		Plain: f.plain,
	}
	if demoSrc != nil {
		cfg.DemoSeed = demoSrc.Seed()
		cfg.DemoOrigin = origin
	}

	if f.once {
		return runOnce(ctx, os.Stdout, cfg, ch, f.frames, f.plain, f.jsonOut)
	}

	// The dashboard's header clock rides the simulated timeline in a demo run,
	// like the agent watcher and the ingest server above. Left on wall time it
	// read a different year than every frame it drew a second after launch, and
	// two runs of one seed could not be compared.
	var uiNow func() time.Time
	if demoSrc != nil {
		uiNow = demoSrc.Now
	}

	return runTUI(ctx, cfg, ch, uiNow, !f.noReload)
}

// reloadPoll is how often the hot-reload watcher stats the executable. A dev
// rebuild finishes well inside a second, and the check is one stat, so a short
// poll is what makes the new build land while the operator is still looking at
// the old one.
const reloadPoll = 400 * time.Millisecond

// runTUI runs the dashboard, restarting into a fresh binary whenever the
// executable on disk is rebuilt (dev hot-reload). now overrides the clock the
// header ticks on; nil leaves it on wall time. It returns the process exit
// code: 0 for the q and Ctrl+C quit, 130 for a signal that reached the run
// context, 1 for a dashboard that could not run.
func runTUI(ctx context.Context, cfg ui.Config, ch <-chan core.Snapshot, now func() time.Time, hotReload bool) int {
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
			go selfreload.Watch(wctx, self, reloadPoll, func() {
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
	opts := []tea.ProgramOption{tea.WithContext(ctx)}
	if !cfg.Plain {
		// The alt screen is what a screen reader cannot follow: every frame
		// repaints the same rows in place, so the linear report would be
		// announced as a wall of changed text on every poll. The plain view
		// scrolls normally instead, which is how a terminal is read.
		opts = append(opts, tea.WithAltScreen())
	}
	m := ui.New(cfg, ch)
	m.SetNow(now)
	prog := tea.NewProgram(m, opts...)
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

// frameColumnsDefault and frameLinesDefault are the --once frame size used
// when stdout is not a terminal. The help screen prints the same two numbers
// as the fallback it promises, so they are named here rather than spelled in
// both places.
const (
	frameColumnsDefault = 120
	frameLinesDefault   = 38
)

// onceWaitFloor and onceWaitPolls bound how long --once waits for each
// snapshot. Snapshots land one poll interval apart, so a slow-polling host
// needs a proportionally patient wait: a fixed cap would abort a healthy
// --interval 10s run before its second frame ever arrives.
const (
	onceWaitFloor = 5 * time.Second
	onceWaitPolls = 3
)

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
	w, h := frameColumnsDefault, frameLinesDefault
	if !plain && !jsonOut {
		if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			// Per dimension, against its own floor: the help promises the
			// terminal as the default for each of the two, and a window too
			// short to lay out is no reason to stop reading its width and
			// render 120 cells into a narrower terminal, where every row
			// wraps.
			if tw >= frameColumnsMin {
				w = min(tw, frameColumnsMax)
			}
			if th >= frameLinesMin {
				h = min(th, frameLinesMax)
			}
		}
		if v, set, err := frameEnv("TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax); err == nil && set {
			w = v
		}
		if v, set, err := frameEnv("TOKTOP_LINES", frameLinesMin, frameLinesMax); err == nil && set {
			h = v
		}
	}
	wait := max(onceWaitFloor, onceWaitPolls*cfg.PollEvery)
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
