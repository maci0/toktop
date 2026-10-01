package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
)

// replayFrames runs a collector for polls passes on a stepped clock, with the
// poll loop paced by a driver rather than by wall-clock time, and returns the
// frames serialized the way --json writes them. Everything the run reads is
// fixed here: the provider answers from its own call count, the process table
// is a stub, and the clock moves only when the driver moves it. Two calls must
// produce the same bytes, which is what "one seed replays the run" means for
// the poll loop.
func replayFrames(t *testing.T, polls int) []string {
	t.Helper()
	var calls atomic.Int64
	engine := provider.Provider{
		Label: "engine", Addr: "fake://engine", Kind: core.KindOllama,
		Poll: func(context.Context) (*provider.Metrics, error) {
			i := calls.Add(1)
			return &provider.Metrics{OutTotal: float64(100 * i * i), InTotal: float64(200 * i)}, nil
		},
	}
	pace := core.NewVirtualPacer()
	origin := time.Unix(1_700_000_000, 0).UTC()
	now := &atomic.Pointer[time.Time]{}
	now.Store(&origin)

	c := New([]provider.Provider{engine}, time.Second)
	c.SetSysFn(nil)
	// The real sampler reads the host's process table, which differs per run
	// and per machine; a stubbed table keeps the frames' process rows a
	// function of the run rather than of the machine it ran on.
	c.procFn = func() []procs.Info {
		return []procs.Info{{PID: 42, Name: "ollama", PortHint: 11434, Engine: "ollama", DefPort: 11434}}
	}
	c.SetNow(func() time.Time { return *now.Load() })
	t.Cleanup(func() { c.SetNow(nil) })
	c.SetPacer(pace)

	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan core.Snapshot, polls+1)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, frames) }()

	// The warm pass is not a tick: the loop makes one before it waits.
	out := make([]string, 0, polls+1)
	take := func() {
		t.Helper()
		select {
		case snap := <-frames:
			b, err := json.Marshal(snap)
			if err != nil {
				t.Fatalf("marshal frame: %v", err)
			}
			out = append(out, string(b))
		case <-time.After(10 * time.Second):
			t.Fatal("collector emitted no frame")
		}
	}
	take()
	at := origin
	for range polls {
		at = at.Add(time.Second)
		now.Store(&at)
		pace.Fire(at)
		take()
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

func TestRunReplaysFrameForFrameFromOneStep(t *testing.T) {
	const polls = 5
	first := replayFrames(t, polls)
	second := replayFrames(t, polls)
	if len(first) != polls+1 {
		t.Fatalf("frames = %d, want %d", len(first), polls+1)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("frame %d differs between two runs of one step sequence:\n first: %s\nsecond: %s", i, first[i], second[i])
		}
	}
	// A run that emitted identical frames because nothing was polled would
	// pass the same diff, so the frames have to carry the run's own history.
	if !strings.Contains(first[len(first)-1], "\"OutTokPS\"") {
		t.Fatalf("last frame carries no provider series: %s", first[len(first)-1])
	}
}

// The probe wave holds two more gates than the wave gap: the Retry-After a
// 503 arms, and the ProbeBackendGap floor the --probe cadence applies. Both
// decide whether a wave launches a generation, so both are answers a replay
// has to reproduce, and both belong on the same timeline as the wave gap
// above. A gate read on the wall clock holds a backend for a length of real
// time the driver never stepped, so the same seed billed a different number
// of generations on every run, and every retry into a gateway that asked for
// backoff is billed. The engine here is a 503 naming a 30s Retry-After: the
// deadline each wave arms is an instant on the run's own clock, and a wave
// inside the backoff neither launches nor extends it.
func TestProbeWaveGatesReplayFromOneStepSequence(t *testing.T) {
	run := func() []string {
		backend := &probeBackend{retryAfter: 30 * time.Second}
		backend.broken.Store(true)
		srv := httptest.NewServer(backend)
		defer srv.Close()

		origin := time.Unix(1_700_000_000, 0).UTC()
		now := origin
		c := New([]provider.Provider{(&fakeProvider{label: "engine", addr: srv.URL}).asProvider()}, time.Second)
		c.lastModel[srv.URL] = "m"
		c.SetNow(func() time.Time { return now })
		defer c.SetNow(nil)

		// probeWaveGap is a package var the audit tests shrink; the backoff
		// gate is the subject here, and a wave the wave gap refuses would
		// never reach it.
		old := probeWaveGap
		probeWaveGap = 0
		defer func() { probeWaveGap = old }()

		var out []string
		wave := func(label string) {
			c.ProbeAll()
			waitFor(t, func() bool {
				c.probeMu.Lock()
				defer c.probeMu.Unlock()
				return len(c.probeInflight) == 0
			}, "probe never cleared")
			c.probeMu.Lock()
			until, held := c.probeBackoff[srv.URL]
			c.probeMu.Unlock()
			if !held {
				out = append(out, label+": no backoff armed")
				return
			}
			out = append(out, fmt.Sprintf("%s: backoff until +%s", label, until.Sub(origin)))
		}
		wave("wave 0 at +0s")
		now = now.Add(10 * time.Second)
		wave("wave 1 at +10s")
		now = now.Add(25 * time.Second)
		wave("wave 2 at +35s")
		return out
	}

	first, second := run(), run()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("probe wave differs between two runs of one step sequence:\nfirst: %s\nsecond: %s", first[i], second[i])
		}
	}
	// The deadline is an instant on the timeline the driver stepped: 30s past
	// the wave that armed it, unchanged by a wave the backoff held, and 30s
	// past the step that released it.
	for i, want := range []string{
		"wave 0 at +0s: backoff until +30s",
		"wave 1 at +10s: backoff until +30s",
		"wave 2 at +35s: backoff until +1m5s",
	} {
		if first[i] != want {
			t.Errorf("step %d answered %q, want %q", i, first[i], want)
		}
	}
}

func TestSetPacerNilRestoresWallClock(t *testing.T) {
	c := New(nil, time.Second)
	c.SetPacer(core.NewVirtualPacer())
	if got := c.pacer(); got == core.WallPacer {
		t.Fatal("pacer = wall clock after a virtual one was set")
	}
	c.SetPacer(nil)
	if got := c.pacer(); got != core.WallPacer {
		t.Fatalf("pacer = %v after SetPacer(nil), want the wall clock", got)
	}
}
