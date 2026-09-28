package collector

import (
	"context"
	"encoding/json"
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
