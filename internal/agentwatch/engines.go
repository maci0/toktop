// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// engineEndpoints parses the monitored engines' advertised URLs into addresses
// that can be compared against a process's open connections. A malformed URL
// is returned as an error: dropping it silently would leave the agent's
// tokens counted both by the engine and by its transcript, a double count
// with no symptom the operator could trace back to a bad address.
func (w *Watcher) engineEndpoints() ([]netip.AddrPort, []string, error) {
	if w.engines == nil {
		return nil, nil, nil
	}
	raw := w.engines()
	eps := make([]netip.AddrPort, 0, len(raw))
	labels := make([]string, 0, len(raw))
	var bad []string
	for _, addr := range raw {
		ap, label, err := parseEngineAddr(addr)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if ap == (netip.AddrPort{}) {
			continue
		}
		eps = append(eps, ap)
		labels = append(labels, label)
	}
	if len(bad) > 0 {
		return eps, labels, errors.New(strings.Join(bad, "; "))
	}
	return eps, labels, nil
}

// parseEngineAddr turns "http://127.0.0.1:11434" into an endpoint and a label.
// A bare "127.0.0.1:8080" is accepted as well. A hostname that is not an
// address is skipped, reported as the zero AddrPort and a nil error: it
// cannot be compared against a connection table, and resolving it would make
// a monitor do DNS on a timer. URLs that omit the port (http://127.0.0.1,
// https://…) use the scheme default: without it ParseAddrPort fails and an
// agent talking to that engine would not be labelled via, so its tokens would
// be counted twice. A string carrying a scheme that will not parse is an
// error, not a hostname: the operator wrote something malformed and needs to
// be told which. So is a port of 0, which parses but names no endpoint.
func parseEngineAddr(addr string) (netip.AddrPort, string, error) {
	host := addr
	// Only a scheme-bearing string is a URL. url.Parse rejects a bare
	// "127.0.0.1:8080" (a colon in the first path segment), and that spelling
	// is a documented input, so the parse is gated on the marker rather than
	// on whether it succeeds.
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil {
			// The library's message quotes the address back, so it carries
			// engine-supplied text to the dashboard banner. The snippet is
			// what keeps that text renderable and bounded.
			return netip.AddrPort{}, "", fmt.Errorf("engine address is not a URL: %s", core.Snippet([]byte(err.Error())))
		}
		if u.Host != "" {
			host = u.Host
			if u.Port() == "" {
				port := "80"
				if u.Scheme == "https" {
					port = "443"
				}
				host = net.JoinHostPort(u.Hostname(), port)
			}
		}
	}
	ap, err := netip.ParseAddrPort(host)
	if err != nil {
		return netip.AddrPort{}, "", nil
	}
	if ap.Port() == 0 {
		// Port 0 parses but is not an endpoint: no connection table holds it,
		// so the entry would sit in the sweep forever and match nothing,
		// labelling no agent and hiding the reason. An explicit :0 is a
		// misspelled address, which is the operator's to fix.
		return netip.AddrPort{}, "", fmt.Errorf("engine address %q names port 0, which is not a connectable endpoint", core.Snippet([]byte(addr)))
	}
	return ap, host, nil
}

// engineError reports err, and repeats it only when it differs from the last
// one reported. A misconfigured engine address fails on every discovery tick,
// so an undeduplicated report would be a permanent error banner over a
// condition the operator has already seen.
//
// A clean tick clears the latch, so a condition that recovers and then comes
// back is reported again. Without that, an address fixed at runtime (a
// gateway that finished starting, a forward that reconnected) and broken
// again an hour later would be silenced by the first report: the operator
// fixed it, saw the banner go, and is never told it is back.
func (w *Watcher) engineError(err error) {
	w.mu.Lock()
	if err == nil {
		w.engineErr = ""
		w.mu.Unlock()
		return
	}
	repeat := w.engineErr == err.Error()
	w.engineErr = err.Error()
	w.mu.Unlock()
	if repeat {
		return
	}
	w.reportError(err)
}
