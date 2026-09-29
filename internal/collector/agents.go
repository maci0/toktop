package collector

import (
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// The agent event feed: what RecordAgent retains, and the clock-offset ledger
// that puts a sender's stamps on this machine's timeline. The id ledger that
// makes a retried POST count once is core.AgentIDLedger, shared with the demo
// source so both answer a replay the same way.

// agentSkewEntry is one agent's clock offset: the smallest lead its timestamps
// have sat ahead of arrival. Every stamp that agent sends carries the same
// offset, so one reading is enough to put the whole agent on this machine's
// timeline, spacing intact. An agent on this timeline records zero and is
// stored exactly as it arrived.
type agentSkewEntry struct {
	agent string
	skew  time.Duration
	at    time.Time
}

// maxAgentSkews bounds the clock-offset ledger the way core.AgentIDLedgerMax bounds
// the id ledger, and on the same reasoning: an agent that stops reporting ages
// out of the horizon, so the count cap only ever bites for a fleet still
// sending.
const maxAgentSkews = core.AgentIDLedgerMax

// forgetAgedAgentSkews drops the offset ledger entries the window has moved
// past, then the oldest ones if the count cap is still exceeded. An offset
// that is dropped and read again off a fresh event costs nothing: the
// difference between the two readings is the sender's clock drift since, not
// a different clock.
//
// A row is removed from the index only when it is the row still in force for
// that agent, named by agentSkewLive: a sender whose clock error is read the
// same way twice produces two rows of equal skew, and dropping the older one
// must not take the newer one's offset with it.
func (c *Collector) forgetAgedAgentSkews(cutoff time.Time) {
	for len(c.agentSkewOrder) > 0 {
		front := c.agentSkewOrder[0]
		if len(c.agentSkewOrder) <= maxAgentSkews &&
			c.agentSkews[front.agent] == front.skew &&
			front.at.After(cutoff) {
			return
		}
		c.agentSkewOrder = c.agentSkewOrder[1:]
		if live, ok := c.agentSkewLive[front.agent]; ok && live == front {
			delete(c.agentSkews, front.agent)
			delete(c.agentSkewLive, front.agent)
		}
	}
}

// refuseForWindow latches a run of events the retained window turned away, so
// emit can say once that a sender is being dropped rather than idle. Without
// it the only trace is stored < accepted on the sender's own POST, which reads
// as a replay: a sender whose clock lags sees exactly that, forever, and the
// agent list stays empty with nothing on it to say why.
//
// The collector clock, like every other latch here: the sample's At, the
// probe-wave gate and the skew correction all follow it, so a latch stamped
// from time.Now ages against a timeline none of them shares.
//
// Call with c.mu held.
func (c *Collector) refuseForWindow(now time.Time, agent string) {
	if c.windowLost.IsZero() {
		c.windowLost = now
	}
	c.windowRefused++
	c.windowAgent = agent
}

// storedForWindow records that an event was retained while a run of refusals
// was running, which is the end of that run. agent is the one that got in, so
// the recovery line names the sender that came back rather than the one that
// was last turned away. Call with c.mu held.
func (c *Collector) storedForWindow(agent string) {
	if c.windowLost.IsZero() {
		return
	}
	if c.windowRecovered == "" {
		c.windowRecovered = agent
	}
}

// windowRun is what one drained run of window refusals has to report: the
// events refused so far, the agent they were refused for, and the run's end.
// A run is either opening or closing at the moment it is drained, never both.
type windowRun struct {
	refused   int
	agent     string
	back      string
	lostFor   time.Duration
	recovered bool
}

// empty reports whether the run has nothing to say, which is every interval
// that neither refused nor stored anything.
func (r windowRun) empty() bool { return r.refused == 0 && !r.recovered }

// drainWindowRefusals reports one run of window refusals: its opening line the
// first time it is seen, its recovery line once an event lands again. The run
// itself is latched until that event, so an interval that refuses nothing new
// writes nothing. Reading it here rather than in RecordAgent keeps the logging
// off the ingest request path and behind the same lock the poll loop needs.
// Call with c.mu held.
// The recovery line closes the run, so it clears the refused count with it:
// the next run starts at zero, not at whatever the run just closed happened to
// leave standing.
func (c *Collector) drainWindowRefusals() windowRun {
	if c.windowLost.IsZero() {
		return windowRun{}
	}
	if !c.windowLogged {
		c.windowLogged = true
		refused := c.windowRefused
		c.windowRefused = 0
		return windowRun{refused: refused, agent: c.windowAgent}
	}
	if c.windowRecovered == "" {
		return windowRun{}
	}
	run := windowRun{
		back:      c.windowRecovered,
		lostFor:   core.Age(c.instant(), c.windowLost).Round(time.Second),
		recovered: true,
	}
	c.windowLost, c.windowAgent, c.windowRefused = time.Time{}, "", 0
	c.windowRecovered, c.windowLogged = "", false
	return run
}

// logWindowRefusals writes the pair of lines one run of window refusals
// produces. A sender whose events all sort behind the retained window is
// dropped with no line of its own, and its sender-side answer (stored under
// accepted) is the one an ordinary replay also gets, so the agent list goes
// empty with nothing on it that says why. The first line names the run; the
// second names its end, when an event was retained again.
func logWindowRefusals(run windowRun) {
	if run.empty() {
		return
	}
	lg := audit()
	if run.refused > 0 {
		attrs := []any{
			"agent", logcfg.Field(run.agent, core.AgentNameMax),
			"refused", run.refused,
			"reason", "older than the retained agent window",
		}
		if run.lostFor > 0 {
			attrs = append(attrs, "down_for", run.lostFor)
		}
		lg.Warn("toktop: agent events refused", attrs...)
	}
	if run.recovered {
		lg.Info("toktop: agent events stored again", "agent", logcfg.Field(run.back, core.AgentNameMax))
	}
}

// RecordAgent stores an agent event (called from the ingest server) and
// reports whether it was retained.
// Events come from many senders whose clocks disagree (the ingest endpoint
// can face a LAN), so arrival order is not time order; every consumer reads
// Agents newest-last (see core.Snapshot), so keep them sorted by timestamp
// the way the probe ring is. A non-empty ID already recorded within
// core.AgentIDHorizon is ignored, so a retried POST of the same event does not
// double-count, however long after the first send it arrives. An event that
// sorts behind the whole retained window is refused too, so the answer stays
// what a sender is told: the feed took this, or it did not. Only the window
// refusal is latched for the audit log: a duplicate is the sender's own replay
// and says nothing about the run's health.
func (c *Collector) RecordAgent(ev core.AgentEvent) bool {
	now := c.instant()
	if ev.At.IsZero() {
		ev.At = now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// An event's timestamp is its sender's clock reading. A sender whose clock
	// runs ahead of this one (a host with a dead RTC, a VM, a laptop off the
	// network since boot) would otherwise stamp events in this machine's
	// future, where they never age out: the agent stays in the list and its
	// tokens in the header totals however long it has been quiet, because
	// nothing is older than the window's cutoff. The offset is one clock
	// reading, not elapsed time, so subtracting it leaves the spacing the
	// sender measured and the rate the summary derives from it.
	//
	// The ledger holds the smallest lead seen for the agent, and a later
	// event that leads by less lowers it. Latching the first reading instead
	// made one outlier decide the timeline for the whole horizon, in two ways
	// a real sender produces: a clock that was 90s fast and was then corrected
	// (NTP, a resumed laptop) keeps every later event 90s stale, and two hosts
	// both running claude post under one name, so the fast one drags the
	// other's events out of the window too. Both show as an agent that has
	// gone quiet while it is still working, and the tokens are outside
	// AgentRateWindow so no total counts them. A minimum is the right estimate
	// here: a lead below the sender's true offset is a clock reading under
	//stating it (jitter, a late POST), and subtracting too little leaves the
	// stamp ahead rather than behind. The offset is bounded by the ledger
	// horizon, so a reading that understates the sender's clock is corrected
	// by a fresh one rather than for as long as the agent keeps sending.
	key := core.CanonicalAgent(ev.Agent)
	// Swept before the lookup, like the id ledger: a reading taken from an
	// offset the horizon has already retired caps the new estimate at a clock
	// error that was measured up to core.AgentIDHorizon ago, and the next event
	// after a quiet spell is the only reading there is.
	c.forgetAgedAgentSkews(now.Add(-core.AgentIDHorizon))
	lead := max(ev.At.Sub(now), 0)
	offset, seen := c.agentSkews[key]
	if !seen || lead < offset {
		offset = lead
	}
	ev.At = ev.At.Add(-offset)
	// The id index answers the dedup check in one map probe. Scanning the
	// window instead cost a full slice walk plus an NFC normalization per
	// retained event, under the mutex emit needs, for every ingested line.
	id := ""
	if ev.ID != "" {
		id = norm.NFC.String(ev.ID)
		// Ageing out runs inside Seen, first: an id still in the index but past
		// the horizon is not a duplicate, and the sweep is O(evicted) only when
		// something actually fell out of the window.
		if c.agentIDs.Seen(id, now) {
			return false
		}
	}
	// Sorted insert and the window trim are one call, and the trim reports
	// itself: AppendRetained drops whatever falls outside the newest
	// AgentHistoryLen as it inserts, so a caller cannot keep one event too
	// many or one too few, and an event that would be trimmed on arrival is
	// refused instead of silently discarded.
	agents, kept := core.AppendRetained(c.agents, ev, core.AgentHistoryLen, core.AgentCmp)
	if !kept {
		// The event sorts behind every entry the feed retains, so the window
		// would trim it the moment it landed: nothing downstream would read it,
		// and its tokens would be missing from every total. Answering stored
		// would report it on the 202 body and the audit line anyway, where a
		// gap below accepted is documented as a replay. A sender whose clock
		// runs behind sees the same honest "kept nothing" it already gets for
		// a duplicate. Its id is not ledgered: no entry was retained, so there
		// is no duplicate for the ledger to suppress.
		c.refuseForWindow(now, key)
		return false
	}
	c.agents = agents
	c.storedForWindow(key)
	// Only a retained event is a reading of the sender's clock, and a minimum
	// estimator never moves the offset upward, so nothing can pin it below
	// the sender's true offset. A replayed POST is normally stopped earlier by
	// the id ledger, and one that slips through carries the original send's
	// stamp, so its lead is at least the offset already in force and this
	// leaves the correction alone.
	if cur, ok := c.agentSkews[key]; !ok || lead < cur {
		c.agentSkews[key] = lead
		// A fresh order entry, not a rewrite of the old one: forgetAgedAgentSkews
		// matches on agent and skew, so the superseded row ages out on its own
		// without deleting the offset now in force, the same way a reused id
		// leaves its older twin behind.
		c.agentSkewOrder = append(c.agentSkewOrder, agentSkewEntry{agent: key, skew: lead, at: now})
		c.agentSkewLive[key] = agentSkewEntry{agent: key, skew: lead, at: now}
	}
	// Swept above, on every call rather than only when a new minimum arrived:
	// an agent that stops reporting has no later reading to trigger a sweep
	// placed after the write, and its offset would stay in force for the life
	// of the process.
	if id != "" {
		c.agentIDs.Add(id, now)
	}
	return true
}
