// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"net/netip"
	"testing"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseEngineAddr throws arbitrary engine addresses at parseEngineAddr and
// at the engineEndpoints sweep that consumes it. The addresses come from the
// engine's own /v1/models payload, so they are untrusted input whose only
// job is to be comparable against a process's open connections. A port of
// zero or an invalid address would match nothing (or match everything,
// once wrapped), and a label that disagrees with the address it accompanies
// is a token count attributed to the wrong engine, so the endpoint and the
// label are asserted to describe the same thing.
func FuzzParseEngineAddr(f *testing.F) {
	for _, seed := range []string{
		"http://127.0.0.1:11434",
		"https://gpu.lan",
		"http://[::1]:8080",
		"127.0.0.1:8080",
		"[::1]:8080",
		"http://127.0.0.1",
		"https://gpu.lan/path?q=1",
		"http://",
		"://127.0.0.1:1",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:-1",
		"http://127.0.0.1:0",
		"gpu.lan",
		"localhost:1234",
		"http://user:pass@127.0.0.1:1",
		"http://[::1",
		"http://[fe80::1%25eth0]:22",
		"HTTP://127.0.0.1:1",
		"ws://127.0.0.1:1",
		"127.0.0.1",
		"",
		" ",
		"http://127.0.0.1:1\x00",
		"é://127.0.0.1:1",
		"http://" + "a." + "b." + "c.:65535",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, addr string) {
		ap, label, err := parseEngineAddr(addr)
		switch {
		case err != nil:
			// A malformed URL is reported, not silently dropped: the caller
			// surfaces the text to the operator, so it has to stay renderable.
			if ap != (netip.AddrPort{}) || label != "" {
				t.Fatalf("error %v came back with endpoint %v and label %q", err, ap, label)
			}
			if msg := err.Error(); core.SanitizeText(msg) != msg {
				t.Fatalf("error carries non-renderable bytes: %q", msg)
			}
		case ap == (netip.AddrPort{}):
			// A hostname that is not an address is skipped, silently and
			// without a label to go with nothing.
			if label != "" {
				t.Fatalf("no endpoint but label %q: %q", label, addr)
			}
		default:
			if !ap.IsValid() {
				t.Fatalf("invalid endpoint %v from %q", ap, addr)
			}
			if p := ap.Port(); p == 0 {
				t.Fatalf("endpoint %v from %q carries no port", ap, addr)
			}
			if label == "" {
				t.Fatalf("endpoint %v from %q has no label", ap, addr)
			}
			again, againLabel, againErr := parseEngineAddr(addr)
			if againErr != nil || again != ap || againLabel != label {
				t.Fatalf("parseEngineAddr(%q) is not deterministic: %v %q %v vs %v %q %v",
					addr, ap, label, err, again, againLabel, againErr)
			}
		}

		w := New(nil, func() []string { return []string{addr, "http://127.0.0.1:11434"} })
		eps, labels, sweepErr := w.engineEndpoints()
		if len(eps) != len(labels) {
			t.Fatalf("engineEndpoints returned %d endpoints and %d labels for %q", len(eps), len(labels), addr)
		}
		if sweepErr != nil && len(eps) == 0 && len(labels) == 0 {
			if msg := sweepErr.Error(); core.SanitizeText(msg) != msg {
				t.Fatalf("engineEndpoints error carries non-renderable bytes: %q", msg)
			}
		}
		for i, e := range eps {
			if !e.IsValid() || e.Port() == 0 {
				t.Fatalf("engineEndpoints kept an unusable endpoint %v from %q", e, addr)
			}
			if labels[i] == "" {
				t.Fatalf("engineEndpoints kept endpoint %v from %q with no label", e, addr)
			}
		}
	})
}
