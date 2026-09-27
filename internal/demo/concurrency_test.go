package demo

import (
	"context"
	"fmt"
	"sync"
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
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan core.Snapshot, 4)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, ch) }()

	var wg sync.WaitGroup
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
	cancel()
	go func() {
		for range ch {
		}
	}()
	<-done
}
