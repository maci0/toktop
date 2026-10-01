package demo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Run drives frames from one goroutine while the UI fires probe waves and the
// ingest endpoint posts events from two others, and the seeded timeline is
// read by all three. -race over this is the check that the one lock around
// them does.
func TestConcurrentFramesProbesAndEvents(t *testing.T) {
	s := NewSource(time.Millisecond, 7)
	// The run's cadence is driven, not the wall clock's. The writers below
	// have to overlap a frame publication to race the state Run reads, and
	// how many frames land in that window was a function of how loaded the
	// machine was: a wall-clock millisecond let the writers finish first on
	// a busy box, and the run published nothing while they ran. A driver
	// firing the pacer puts a step on the timeline whatever the load.
	pace := core.NewVirtualPacer()
	s.SetPacer(pace)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan core.Snapshot, 4)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, ch) }()

	// The drainer runs from the start, and the writers below do not begin
	// until the run has published a frame. Started after the writers, it
	// left Run parked on a full channel with the ctx that unparks it
	// already cancelled: the writers finished in microseconds, filled the
	// four-slot buffer, and Run waited out the whole race on ctx.Done()
	// instead of reading the state they were mutating. The test passed, and
	// the -race overlap it exists for never happened.
	var frames atomic.Int64
	firstFrame := make(chan struct{})
	go func() {
		for range ch {
			if frames.Add(1) == 1 {
				close(firstFrame)
			}
		}
	}()
	select {
	case <-firstFrame:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("Run published no frame; the calls below would race an idle loop")
	}

	var wg sync.WaitGroup
	// Steps keep landing until every writer is home, so the loop reads the
	// state they are mutating rather than idling behind a tick that never
	// came. The first frame above needed no tick; every one after it does.
	writing := make(chan struct{})
	stepping := make(chan struct{})
	go func() {
		defer close(stepping)
		for {
			select {
			case <-writing:
				return
			case <-time.After(time.Millisecond):
				pace.Fire(s.Now())
			}
		}
	}()
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				switch w % 3 {
				case 0:
					s.RecordAgent(core.AgentEvent{ID: fmt.Sprintf("id-%d-%d", w, i), At: time.Now(), Agent: "a", OutputTokens: 1})
				case 1:
					s.ProbeAll()
				case 2:
					_ = s.Now()
				}
			}
		})
	}
	wg.Wait()
	close(writing)
	<-stepping
	cancel()
	go func() {
		for range ch {
		}
	}()
	<-done
	if frames.Load() < 2 {
		t.Fatal("the run published no frame while the writers were calling in: they shared no state with the run they were racing")
	}
}
