package collector

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/provider"
)

// The collector's shared state is reached from five goroutines at once in a
// real run: Run's emit loop, the sys and process pollers, the probe fan-out
// ProbeAll spawns, the ingest handlers calling RecordAgent, and SetNow.
// -race over this is the check that the locks around them actually hold.
func TestConcurrentEmitRecordProbeClock(t *testing.T) {
	provs := []provider.Provider{}
	for i := range 3 {
		label := fmt.Sprintf("p%d", i)
		provs = append(provs, provider.Provider{Label: label, Addr: "http://127.0.0.1:" + fmt.Sprint(9000+i), Kind: core.KindOllama,
			Poll: func(ctx context.Context) (*provider.Metrics, error) {
				return &provider.Metrics{OutTotal: 10, InTotal: 5, Models: []core.ModelInfo{{Name: "m"}}}, nil
			}})
	}
	c := New(provs, time.Millisecond)
	c.SetSysFn(func() core.SysSample { return core.SysSample{CPUModel: "x"} })
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan core.Snapshot, 4)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, out) }()

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				switch w % 4 {
				case 0:
					c.RecordAgent(core.AgentEvent{ID: fmt.Sprintf("id-%d-%d", w, i), At: time.Now(), Agent: "a", OutputTokens: 1})
				case 1:
					c.RecordProbe(core.ProbeSample{At: time.Now(), Addr: "x", TokPS: float64(i)})
				case 2:
					c.ProbeAll()
				case 3:
					c.SetNow(func() time.Time { return time.Now() })
				}
			}
		})
	}
	wg.Wait()
	cancel()
	go func() {
		for range out {
		}
	}()
	<-done
}
