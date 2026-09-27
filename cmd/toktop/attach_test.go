package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/remote"
)

// attachEngine serves just enough of one engine's surface for identify() to
// settle on it, and counts the requests so a test can tell which base the
// provider was built for.
func attachEngine(t *testing.T, routes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func labels(ps []provider.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Label
	}
	return out
}

// An --add endpoint that answers as a known engine attaches as that engine,
// not as a generic OpenAI box: the kind decides which scrape runs.
func TestAttachLocalUsesIdentifiedKind(t *testing.T) {
	base := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})

	var got []provider.Provider
	stderr := captureStderr(t, func() {
		got = attachLocal(context.Background(), nil, base)
	})

	if len(got) != 1 {
		t.Fatalf("providers = %+v, want exactly one", labels(got))
	}
	if got[0].Kind != core.KindSGLang {
		t.Errorf("kind = %q, want %q", got[0].Kind, core.KindSGLang)
	}
	if got[0].Addr != base {
		t.Errorf("addr = %q, want %q", got[0].Addr, base)
	}
	if got[0].Poll == nil {
		t.Error("attached provider has no poll function")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want silence for a recognized engine", stderr)
	}
}

// A trailing slash on the flag would otherwise reach the engine as a double
// slash on every request path, which most routers answer with a 404, and the
// endpoint would drop to the generic fallback with the wrong kind.
func TestAttachLocalTrimsTrailingSlash(t *testing.T) {
	base := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})

	var got []provider.Provider
	stderr := captureStderr(t, func() {
		got = attachLocal(context.Background(), nil, base+"/")
	})

	if len(got) != 1 {
		t.Fatalf("providers = %+v, want exactly one", labels(got))
	}
	if got[0].Addr != base {
		t.Errorf("addr = %q, want %q (trailing slash kept)", got[0].Addr, base)
	}
	if got[0].Kind != core.KindSGLang {
		t.Errorf("kind = %q, want %q: the trailing slash cost the identification", got[0].Kind, core.KindSGLang)
	}
	if strings.Contains(stderr, "nothing recognized") {
		t.Errorf("stderr = %q, want the endpoint identified despite the trailing slash", stderr)
	}
}

// A gateway toktop has no name for is still worth a dashboard line, and the
// operator has to be told the kind is a guess rather than an identification.
func TestAttachLocalFallsBackToGenericOpenAI(t *testing.T) {
	base := attachEngine(t, nil)

	var got []provider.Provider
	stderr := captureStderr(t, func() {
		got = attachLocal(context.Background(), nil, base)
	})

	if len(got) != 1 {
		t.Fatalf("providers = %+v, want exactly one", labels(got))
	}
	if got[0].Kind != core.KindOpenAI {
		t.Errorf("kind = %q, want %q", got[0].Kind, core.KindOpenAI)
	}
	if got[0].Poll == nil {
		t.Error("generic fallback has no poll function")
	}
	if !strings.Contains(stderr, "nothing recognized") || !strings.Contains(stderr, base) {
		t.Errorf("stderr = %q, want the unrecognized-endpoint warning naming %s", stderr, base)
	}
}

// attachLocal appends; discovery's providers must survive it, in order.
func TestAttachLocalAppendsToExistingProviders(t *testing.T) {
	base := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})
	first := provider.Provider{Label: "local:0", Addr: "http://127.0.0.1:1", Kind: core.KindOllama, Poll: func(context.Context) (*provider.Metrics, error) { return nil, nil }}

	got := attachLocal(context.Background(), []provider.Provider{first}, base)
	if len(got) != 2 {
		t.Fatalf("providers = %+v, want the existing one plus the endpoint", labels(got))
	}
	if got[0].Label != "local:0" || got[1].Addr != base {
		t.Errorf("order = %+v, want the discovered provider first", labels(got))
	}
}

// A target that could not be attached must not take the run down with a
// nil provider list: the local engines already found still poll, and the
// caller's host-vitals chain is handed back untouched for the next target.
func TestAttachTargetFailureKeepsSysChain(t *testing.T) {
	tgt := remote.Target{User: "nobody", Host: "127.0.0.1", Port: closedPort(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prev := func() core.SysSample { return core.SysSample{RemoteHost: "earlier"} }
	var got []provider.Provider
	var gotSys func() core.SysSample
	var err error
	captureStderr(t, func() {
		got, gotSys, err = attachTarget(ctx, tgt, "", prev)
	})

	if err == nil {
		t.Fatal("attachTarget on a closed port returned no error")
	}
	if got != nil {
		t.Errorf("providers = %+v, want none from a target that never attached", labels(got))
	}
	if gotSys == nil {
		t.Fatal("sysFn chain dropped, so later targets would lose earlier host readings")
	}
	if s := gotSys(); s.RemoteHost != "earlier" {
		t.Errorf("sysFn lost the previous chain: %+v", s)
	}
}

// closedPort returns a port nothing listens on: bound so the number is real,
// then released so the dial is refused instead of queued behind a backlog.
func closedPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// Every named target failing is a silent substitution once the alt screen is
// up, so the run refuses instead of showing local engines only.
func TestAttachEnginesRefusesWhenNoTargetAttached(t *testing.T) {
	srv := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})
	f := &cliFlags{adds: []string{srv}}
	targets := []remote.Target{
		{User: "nobody", Host: "127.0.0.1", Port: closedPort(t)},
		{User: "nobody", Host: "127.0.0.1", Port: closedPort(t)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var (
		providers []provider.Provider
		sysFn     func() core.SysSample
		err       error
	)
	stderr := captureStderr(t, func() {
		providers, sysFn, err = attachEngines(ctx, f, targets)
	})

	if err == nil {
		t.Fatal("run continued with a dashboard of local engines only")
	}
	if !strings.Contains(err.Error(), "no ssh target could be attached") {
		t.Errorf("err = %v, want it to name the all-targets-failed case", err)
	}
	if !strings.Contains(err.Error(), "last error") {
		t.Errorf("err = %v, want the last connection failure carried", err)
	}
	if providers != nil || sysFn != nil {
		t.Error("refused run handed back providers a caller would start on")
	}
	if !strings.Contains(stderr, "toktop:") {
		t.Errorf("stderr = %q, want the per-target reason printed before the exit", stderr)
	}
}

// With no ssh target named, --add endpoints are the whole remote surface and
// the run starts with no host-vitals chain of its own.
func TestAttachEnginesAddsWithoutTargets(t *testing.T) {
	srv := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})
	f := &cliFlags{adds: []string{srv}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var (
		providers []provider.Provider
		sysFn     func() core.SysSample
		err       error
	)
	captureStderr(t, func() {
		providers, sysFn, err = attachEngines(ctx, f, nil)
	})

	if err != nil {
		t.Fatalf("attachEngines: %v", err)
	}
	if sysFn != nil {
		t.Error("sysFn set with no ssh target; local vitals would be replaced by a nil chain")
	}
	found := false
	for _, p := range providers {
		if p.Addr == srv {
			found = true
			if p.Kind != core.KindSGLang {
				t.Errorf("kind = %q, want %q", p.Kind, core.KindSGLang)
			}
		}
	}
	if !found {
		t.Errorf("the --add endpoint is missing from %+v", labels(providers))
	}
}

// Every other attach line describes something that went wrong, so an engine
// that answers for the whole run writes none of them. The run's local set is
// recorded once at attach, or the audit log cannot tell a run measuring one
// engine from a run measuring none.
func TestAttachEnginesRecordsLocalEngines(t *testing.T) {
	srv := attachEngine(t, map[string]string{"/metrics": "sglang:gen_throughput 1\n"})
	var lines bytes.Buffer
	prev := attachLog
	attachLog = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	t.Cleanup(func() { attachLog = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	captureStderr(t, func() {
		attachEngines(ctx, &cliFlags{adds: []string{srv}}, nil)
	})

	got := lines.String()
	if !strings.Contains(got, "local engines attached") {
		t.Fatalf("attach wrote no record of the engines it measures:\n%s", got)
	}
	if !strings.Contains(got, srv) {
		t.Errorf("the record does not name the attached endpoint:\n%s", got)
	}
}
