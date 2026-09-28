// The agent event id ledger: which event ids a recorder has already stored,
// and for how long.
//
// It lives here because the two recorders of the same feed (the live
// collector and the demo source) have to answer a replayed POST identically,
// and the tier rule puts them on either side of a boundary neither may
// import across. Every core.AgentRecorder implementation holds one.

package core

import "time"

// The dedup window for event ids, and its bounds.
//
// AgentHistoryLen caps what the feed displays, which is a poor stand-in for
// how long a replay has to stay recognizable: the ring holds roughly half a
// minute of a busy fleet's events, so a sender whose POST is retried after a
// lost response and a client-side backoff of a minute finds its ids already
// evicted and every line of the replay counted a second time. The horizon
// below is the retry window a sender may reasonably hold to; the count cap
// bounds the ledger for a fleet that emits faster than that, so neither bound
// can be reached without the other holding.
const (
	AgentIDHorizon   = 15 * time.Minute
	AgentIDLedgerMax = 8 * AgentHistoryLen
)

// agentIDEntry is one id and the instant it was recorded at, held in
// insertion order so the oldest is the one that falls out of the window.
type agentIDEntry struct {
	id string
	at time.Time
}

// AgentIDLedger remembers the event ids already stored, so a retried POST
// counts once however long after the first send it arrives. It is keyed on the
// instant the event was recorded, not on the event's own timestamp: a replay
// carries the sender's clock, and a forged or stale stamp must not decide how
// long its own duplicate is ignored.
//
// The zero value is ready to use. It is not safe for concurrent use; the
// recorder holding it owns the lock its other fields are under.
type AgentIDLedger struct {
	ids   map[string]time.Time
	order []agentIDEntry
}

// Seen reports whether id is still in the ledger, dropping first whatever the
// window has moved past. An id the horizon has retired is not a duplicate.
func (l *AgentIDLedger) Seen(id string, now time.Time) bool {
	l.forget(now.Add(-AgentIDHorizon))
	_, dup := l.ids[id]
	return dup
}

// Add records id as stored at now.
func (l *AgentIDLedger) Add(id string, now time.Time) {
	if l.ids == nil {
		l.ids = make(map[string]time.Time)
	}
	l.ids[id] = now
	l.order = append(l.order, agentIDEntry{id: id, at: now})
	l.forget(now.Add(-AgentIDHorizon))
}

// Len is how many distinct ids the ledger holds.
func (l *AgentIDLedger) Len() int { return len(l.ids) }

// Tracked is how many entries the eviction order carries. Every Add runs the
// sweep, which retires an id's older record the moment the newer one lands, so
// the order holds one entry per held id: Tracked equals Len, and both are
// bounded by AgentIDLedgerMax.
func (l *AgentIDLedger) Tracked() int { return len(l.order) }

// Held reports whether id is in the ledger right now, without ageing anything
// out. It is for reading the ledger back, not for the dedup decision, which
// goes through Seen so a stale id is not read as a duplicate.
func (l *AgentIDLedger) Held(id string) bool {
	_, ok := l.ids[id]
	return ok
}

// forget drops the entries the window has moved past, then, if the count cap
// is still exceeded, the oldest ones. An entry is removed from the index only
// when it is still the occurrence that reached the front: a record superseded
// by a newer one of the same id no longer stands for it, and dropping it from
// the index would retire the record that replaced it.
func (l *AgentIDLedger) forget(cutoff time.Time) {
	for len(l.order) > 0 {
		front := l.order[0]
		if len(l.order) <= AgentIDLedgerMax && l.ids[front.id].Equal(front.at) && front.at.After(cutoff) {
			return
		}
		l.order = l.order[1:]
		if at, ok := l.ids[front.id]; ok && at.Equal(front.at) {
			delete(l.ids, front.id)
		}
	}
}
