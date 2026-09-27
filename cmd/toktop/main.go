package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
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
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/remote"
	"github.com/maci0/toktop/internal/selfreload"
	"github.com/maci0/toktop/internal/sysmon"
	"github.com/maci0/toktop/internal/ui"
)

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
	// The plain report renders no sized frame, so a frame override is named
	// as unused there rather than validated: rejecting TOKTOP_COLUMNS=10 for
	// a frame that is never composed aborts a run whose output does not read
	// the variable at all.
	if f.once && !f.plain {
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
	// Targets are parsed here, before the TTY check below, so a malformed
	// ssh:// URL is named as the mistake it is rather than reported as
	// "stdout is not a terminal" when the run is piped or redirected.
	targets, err := parseTargets(remoteTargets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "toktop:", err)
		os.Exit(2)
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
	warnIgnoredFlags(explicit, f.demo, f.once, f.plain, f.agents, f.noIngest, len(f.adds), len(remoteTargets))
	warnIgnoredFrameEnv(f.once, f.plain)
	warnUnusedEnv(explicit["bearer"], f.demo, f.noIngest, len(f.adds), len(remoteTargets))
	warnIgnoredGauntletHome(f.agents)
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
			// Every LoadDefinitions failure names the file it read, which
			// lives under $HOME unless GAUNTLET_HOME says otherwise, so the
			// home is folded to "~": this line is the only record of what is
			// wrong with the file, and it gets pasted into issues.
			fmt.Fprintf(os.Stderr, "toktop: %s\n", core.RedactHome(err.Error()))
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
		if err := bearer.Set(tok); err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v; the engine will be queried unauthenticated\n", err)
		}
		warnInsecureAdd(f.adds)
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
			if err := bearer.Allow(raw); err != nil {
				fmt.Fprintf(os.Stderr, "toktop: %v; requests there go unauthenticated\n", err)
			}
			if p := provider.Attach(ctx, strings.TrimRight(raw, "/")); p.Poll != nil {
				providers = append(providers, p)
			} else {
				fmt.Fprintf(os.Stderr, "toktop: nothing recognized at %s; polling as generic openai anyway\n", raw)
				providers = append(providers, provider.NewOpenAICompat(raw, raw, core.KindOpenAI))
			}
		}

		var sysWrap func() core.SysSample
		for _, tgt := range targets {
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
		startProbeTicker(ctx, prober, time.Duration(f.probeSecs)*time.Second)
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
		aw.SetOnError(func(err error) {
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
	if demoSrc != nil {
		cfg.DemoSeed = demoSrc.Seed()
	}

	if f.once {
		if code := runOnce(ctx, os.Stdout, cfg, ch, f.frames, f.plain); code != 0 {
			os.Exit(code)
		}
		return
	}

	runTUI(ctx, cfg, ch, !f.noReload)
}

// startProbeTicker fires prober once straight away and then every d, until
// the context is canceled. The first call is not deferred to the first tick:
// --probe should put a request on the wire when the dashboard comes up, not
// one interval later.
func startProbeTicker(ctx context.Context, prober func(), d time.Duration) {
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
	if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil && tw >= frameColumnsMin && th >= frameLinesMin {
		w, h = min(tw, frameColumnsMax), min(th, frameLinesMax)
	}
	if v, set, err := frameEnv("TOKTOP_COLUMNS", frameColumnsMin, frameColumnsMax); err == nil && set {
		w = v
	}
	if v, set, err := frameEnv("TOKTOP_LINES", frameLinesMin, frameLinesMax); err == nil && set {
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
	if err == nil || isBrokenPipe(err) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "toktop: write stdout: %v\n", err)
	return 1
}

// parseTargets resolves every ssh:// target up front. A bad URL is a usage
// error, and the caller is still deciding whether stdout is a terminal, so
// parsing here keeps the two complaints from competing.
func parseTargets(raws []string) ([]remote.Target, error) {
	targets := make([]remote.Target, 0, len(raws))
	for _, raw := range raws {
		tgt, err := remote.ParseTarget(raw)
		if err != nil {
			return nil, err
		}
		targets = append(targets, tgt)
	}
	return targets, nil
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
	if lvl := strings.TrimSpace(os.Getenv(logcfg.LevelEnv)); lvl != "" {
		// The resolved level, not the raw string: "warning" and "WARN" print
		// as warn, so the line matches what the audit log actually applies.
		// main rejects an unparseable value, so the error case is unreachable.
		if parsed, err := logcfg.ParseLogLevel(lvl); err == nil {
			fmt.Fprintf(&b, " log=%s", logcfg.LogLevelName(parsed))
		}
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
