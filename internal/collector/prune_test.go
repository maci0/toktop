package collector

import (
	"context"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/provider"
)

func TestPruneStaleKeysOnClaimRun(t *testing.T) {
	p := provider.Provider{Label: "a", Addr: "http://127.0.0.1:1"}
	c := New([]provider.Provider{p}, time.Second)
	c.errFold = map[string]foldedErr{}
	c.slow["stale"] = time.Now()
	c.scrape["stale"] = scrapeState{}
	c.lastModel["stale"] = "m"
	c.kvPct["stale"] = 1
	c.histIn["stale"] = &timedRing{}
	c.histOut["stale"] = &timedRing{}
	c.prev["stale"] = prevSample{}
	c.down["stale"] = downState{}
	c.errFold["stale"] = foldedErr{}
	c.probeBackoff["stale"] = time.Now().Add(time.Minute)
	c.probeLast["stale"] = time.Now()
	c.probeDown["stale"] = &probeDownState{}
	if !c.claimRun(context.Background()) {
		t.Fatal("claimRun failed")
	}
	if _, ok := c.histOut["stale"]; ok {
		t.Fatal("histOut stale kept")
	}
	if _, ok := c.prev["stale"]; ok {
		t.Fatal("prev stale kept")
	}
	if _, ok := c.down["stale"]; ok {
		t.Fatal("down stale kept")
	}
	if _, ok := c.errFold["stale"]; ok {
		t.Fatal("errFold stale kept")
	}
	if _, ok := c.probeBackoff["stale"]; ok {
		t.Fatal("probeBackoff stale kept")
	}
	if _, ok := c.probeLast["stale"]; ok {
		t.Fatal("probeLast stale kept")
	}
	if _, ok := c.probeDown["stale"]; ok {
		t.Fatal("probeDown stale kept")
	}
	c.releaseRun()
}
