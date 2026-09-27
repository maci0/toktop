package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/remote"
	"github.com/maci0/toktop/internal/sysmon"
)

// Resolving every measured engine into one provider list: what discovery
// finds on this host, each --add endpoint, and each ssh target. The returned
// sysFn merges remote host readings onto the local sample, and is nil when no
// target attached.

// attachLog records what the run actually attached. Every line here has a
// stderr twin the operator sees while the dashboard is up; the audit copy is
// the one that outlives the alt screen and a closed terminal, which is the
// only record of a target that silently stopped contributing.
var attachLog = logcfg.Logger

// attachLocal adds one --add endpoint to providers. The bearer token rides
// only to endpoints the operator named: discovery probes every well-known port
// on spec, and whatever answers there must not be able to harvest the
// credential. An endpoint that recognizes nothing still polls as a generic
// OpenAI-compatible one, because a gateway toktop has no name for is still
// worth a line on the dashboard.
func attachLocal(ctx context.Context, providers []provider.Provider, raw string) []provider.Provider {
	if err := bearer.Allow(raw); err != nil {
		fmt.Fprintf(os.Stderr, "toktop: %v; requests there go unauthenticated\n", err)
		attachLog().Warn("toktop: bearer refused", "endpoint", logcfg.Field(raw, 256), "error", logcfg.Field(err.Error(), 256))
	}
	if p := provider.Attach(ctx, strings.TrimRight(raw, "/")); p.Poll != nil {
		return append(providers, p)
	}
	fmt.Fprintf(os.Stderr, "toktop: nothing recognized at %s; polling as generic openai anyway\n", raw)
	attachLog().Warn("toktop: nothing recognized at endpoint", "endpoint", logcfg.Field(raw, 256))
	return append(providers, provider.NewOpenAICompat(raw, raw, core.KindOpenAI))
}

// attachTarget attaches one ssh target, chaining its host readings onto the
// wrapper built so far. A target that fails while others attach is a degraded
// run, not a failed one, and the reason is already on stderr. But when every
// target the operator named failed, the dashboard that comes up shows local
// engines only, and the one line explaining why is hidden under the alt
// screen: a typo'd host looks exactly like a host with no engines running.
// That is the same reasoning the ingest endpoint applies to an explicit listen
// address, so it gets the same treatment: refuse to start rather than start
// into a silent substitution.
func attachTarget(ctx context.Context, tgt remote.Target, sshKey string, sysFn func() core.SysSample) ([]provider.Provider, func() core.SysSample, error) {
	// Only when set: an empty flag must keep the IdentityFile resolved from
	// ~/.ssh/config by ParseTarget.
	if sshKey != "" {
		tgt.KeyFile = sshKey
	}
	rp, rsys, err := attachRemote(ctx, tgt)
	if err != nil {
		// The caller prints the same reason; this is the record of an ssh
		// target the run went on without. The reasons are refused keys, an
		// unreadable host-key store and unreachable hosts, none of which the
		// dashboard says anything about once it is up.
		attachLog().Warn("toktop: ssh target not attached",
			"target", logcfg.Field(tgt.UserHost(), 256),
			"port", tgt.Port,
			"error", logcfg.Field(err.Error(), 256))
		return nil, sysFn, err
	}
	fmt.Fprintf(os.Stderr, "toktop: attached %d engine(s) via ssh on %s\n", len(rp), tgt.Host)
	attachLog().Info("toktop: ssh target attached",
		"target", logcfg.Field(tgt.UserHost(), 256),
		"port", tgt.Port,
		"engines", len(rp))
	prev := sysFn
	return rp, func() core.SysSample {
		var s core.SysSample
		if prev != nil {
			s = prev()
		} else {
			s = sysmon.Sample()
		}
		rsys.Merge(&s)
		return s
	}, nil
}

// attachEngines builds the provider list the run measures: local discovery
// first, then every --add endpoint, then every ssh target. The error is
// non-nil only when targets were named and none of them attached, which the
// caller turns into a usage exit.
func attachEngines(ctx context.Context, f *cliFlags, targets []remote.Target) ([]provider.Provider, func() core.SysSample, error) {
	providers := provider.Discover(ctx)
	for _, raw := range f.adds {
		providers = attachLocal(ctx, providers, raw)
	}

	var sysFn func() core.SysSample
	attached, lastErr := 0, error(nil)
	for _, tgt := range targets {
		rp, next, err := attachTarget(ctx, tgt, f.sshKey, sysFn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %v\n", err)
			lastErr = err
			continue
		}
		attached++
		providers = append(providers, rp...)
		sysFn = next
	}
	if len(targets) > 0 && attached == 0 {
		return nil, nil, fmt.Errorf("no ssh target could be attached; last error: %w", lastErr)
	}
	return providers, sysFn, nil
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
			attachLog().Warn("toktop: remote port not forwarded",
				"target", logcfg.Field(tgt.UserHost(), 256),
				"remote_port", p)
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
		fmt.Fprintf(os.Stderr, "toktop: %s:%d is listening but speaks no recognized engine API; skipping\n",
			tgt.Host, rports[i])
		attachLog().Warn("toktop: remote port skipped",
			"target", logcfg.Field(tgt.UserHost(), 256),
			"remote_port", rports[i],
			"reason", "no recognized engine API")
	}
	stats := &remote.Stats{Client: cli}
	go stats.Run(ctx, 5*time.Second)
	return providers, stats, nil
}
