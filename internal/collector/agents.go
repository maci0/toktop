package collector

import (
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
)

// The agent event feed: what RecordAgent retains, and the id ledger that
// makes a retried POST count once.

// The dedup window for event ids, and its bounds.
//
// core.AgentHistoryLen caps what the feed displays, which is a poor stand-in
// for how long a replay has to stay recognizable: the ring holds roughly half
// a minute of a busy fleet's events, so a sender whose POST is retried after a
// lost response and a client-side backoff of a minute finds its ids already
// evicted and every line of the replay counted a second time. The horizon
// below is the retry window a sender may reasonably hold to; the count cap
// bounds the ledger for a fleet that emits faster than that, so neither bound
// can be reached without the other holding.
const (
	agentIDHorizon = 15 * time.Minute
	agentIDMax     = 8 * core.AgentHistoryLen
)

// agentIDEntry is one id and the instant it was recorded at, held in
// insertion order so the oldest is the one that falls out of the window.
type agentIDEntry struct {
	id string
	at time.Time
}

// forgetAgedAgentIDs drops the entries the window has moved past, then, if the
// count cap is still exceeded, the oldest ones. An id can appear twice in the
// order (recorded, evicted, reused), so an entry is only removed from the
// index when it is still the occurrence that reached the front: the newer
// record of the same id must survive its own older twin.
func (c *Collector) forgetAgedAgentIDs(cutoff time.Time) {
	for len(c.agentIDOrder) > 0 {
		front := c.agentIDOrder[0]
		if len(c.agentIDOrder) <= agentIDMax && c.agentIDs[front.id].Equal(front.at) && front.at.After(cutoff) {
			return
		}
		c.agentIDOrder = c.agentIDOrder[1:]
		if at, ok := c.agentIDs[front.id]; ok && at.Equal(front.at) {
			delete(c.agentIDs, front.id)
		}
	}
}

// RecordAgent stores an agent event (called from the ingest server) and
// reports whether it was retained.
// Events come from many senders whose clocks disagree (the ingest endpoint
// can face a LAN), so arrival order is not time order; every consumer reads
// Agents newest-last (see core.Snapshot), so keep them sorted by timestamp
// the way the probe ring is. A non-empty ID already recorded within
// agentIDHorizon is ignored, so a retried POST of the same event does not
// double-count, however long after the first send it arrives.
func (c *Collector) RecordAgent(ev core.AgentEvent) bool {
	now := c.instant()
	if ev.At.IsZero() {
		ev.At = now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The id index answers the dedup check in one map probe. Scanning the
	// window instead cost a full slice walk plus an NFC normalization per
	// retained event, under the mutex emit needs, for every ingested line.
	id := ""
	if ev.ID != "" {
		// Ageing out runs first: an id still in the index but past the horizon
		// is not a duplicate, and the sweep is O(evicted) only when something
		// actually fell out of the window.
		c.forgetAgedAgentIDs(now.Add(-agentIDHorizon))
		id = norm.NFC.String(ev.ID)
		if _, dup := c.agentIDs[id]; dup {
			return false
		}
	}
	// Sorted insert and the window trim are one call: AppendSorted drops
	// whatever falls outside the newest AgentHistoryLen as it inserts, so a
	// caller cannot keep one event too many or one too few.
	c.agents = core.AppendSorted(c.agents, ev, core.AgentHistoryLen, core.AgentCmp)
	if id != "" {
		// The ledger is keyed on the recording instant, not the event's own
		// timestamp: a replay carries the sender's clock, and a forged or
		// stale stamp must not decide how long its own duplicate is ignored.
		c.agentIDs[id] = now
		c.agentIDOrder = append(c.agentIDOrder, agentIDEntry{id: id, at: now})
		c.forgetAgedAgentIDs(now.Add(-agentIDHorizon))
	}
	return true
}
