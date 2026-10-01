package collector

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/probe"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
)

// One engine's entry in a snapshot, and the health state that entry is keyed
// on: the latches recording an outage or a slow poll, the error text fold
// they carry, and the audit lines each boundary crossing produces.

// downState is one engine's current outage: when it started and the reason
// the first failed poll gave.
type downState struct {
	since  time.Time
	reason string
}

// foldedErr is one poll error's text after the home fold and the length
// bound, kept alongside a digest of the exact error and the home it was folded
// against so a repeat of that error can be answered from memory.
//
// The digest is a digest rather than the error text because the error is the
// engine's to choose and is not bounded: an error carrying a decoder's whole
// offending literal runs to megabytes, and holding one per engine for the life
// of the process is memory a remote peer sized. Digesting still answers the
// repeat from memory, since a downed engine repeats one error verbatim, and the
// hash is a pass over the bytes rather than the fold the memo exists to skip.
type foldedErr struct {
	digest [sha256.Size]byte
	home   string
	text   string
}

// foldErr is the folded text of err, memoized per key, and whether the home
// lookup the fold depends on failed. The fold is a pure function of the error
// string and the home directory, and a downed engine repeats its failed poll's
// error verbatim every interval, so folding once per distinct error replaces a
// multi-megabyte re-fold per second per downed engine. The home is re-read each
// time and part of the memo key, so a process whose $HOME changes does not keep
// serving a fold made against the old one.
//
// The home failure is reported rather than logged here, because this runs under
// c.mu and the caller publishes it after the lock. Call with c.mu held.
func (c *Collector) foldErr(key string, err error) (text string, homeUnknown bool) {
	raw := err.Error()
	digest := sha256.Sum256([]byte(raw))
	home, herr := os.UserHomeDir()
	if f, ok := c.errFold[key]; ok && f.digest == digest && f.home == home {
		return f.text, false
	}
	if herr != nil {
		// RedactHome folds the home on the same lookup and gives up with the
		// message unchanged, so the text published below and on every
		// --json run keeps the account's paths. That is a consequence of the
		// environment, not a choice, and the operator is the one who can fix
		// it, so it is said once per sweep rather than left to be
		// discovered in a path that was supposed to be folded.
		homeUnknown = true
	}
	text = core.Snippet([]byte(core.RedactHome(raw)))
	if c.errFold == nil {
		c.errFold = make(map[string]foldedErr)
	}
	c.errFold[key] = foldedErr{digest: digest, home: home, text: text}
	return text, homeUnknown
}

// providerSnapshot builds one engine's snapshot entry from its poll result,
// updating the per-key baselines, history rings and health state that entry is
// keyed on. Call with c.mu held.
//
// The health transitions the poll caused are returned separately so emit can
// collect the whole sweep's transitions and log them after the lock: an engine
// going away is the dependency failure an operator needs named, and a slow
// stderr must not stall the poll loop the snapshot depends on. The list is
// empty when this engine crossed neither boundary. homeUnknown reports the same
// deferral for the fold's own audit line.
func (c *Collector) providerSnapshot(p provider.Provider, r result, now time.Time, byPort map[int]procs.Info) (ps core.ProviderSnapshot, changes []healthChange, homeUnknown bool) {
	ps = core.ProviderSnapshot{
		Label: p.Label,
		Kind:  p.Kind,
		Addr:  p.Addr,
	}
	// Per-provider state is keyed by providerKey: the endpoint when the
	// provider has one, the label otherwise, so labels repeat across
	// instances of the same engine kind without sharing baselines.
	key := providerKey(p)
	// ring() (not a bare map index): a provider whose first poll failed
	// has no history yet, and indexing the map there would deref nil.
	outR, inR := c.ring(c.histOut, key), c.ring(c.histIn, key)
	switch {
	case r.err != nil:
		// Folded for the same reason the audit log folds it: an engine error
		// can echo a model or config path under the operator's home, and
		// this text reaches the dashboard and both reports.
		//
		// Snippet bounds it too, and that is the part the home fold does not
		// do: the error is a decoder's, and encoding/json embeds the whole
		// offending literal in an UnmarshalTypeError. An engine answering
		// /api/ps with a 4 MiB number puts those 4 MiB in the error, into
		// the snapshot, and out again on every frame and on every --json
		// run, and they are re-sanitized each time. The engine's HTTP status
		// line rides the same string, so this is where both are made safe,
		// once, at the boundary every other engine-supplied string already
		// uses.
		ps.Err, homeUnknown = c.foldErr(key, r.err)
	case r.m == nil:
		ps.Err = "empty poll result"
	default:
		ps.OK = true
		if was, ok := c.down[key]; ok {
			delete(c.down, key)
			changes = append(changes, healthChange{
				p: p, kind: changeUp, reason: was.reason, since: was.since, heldFor: core.Age(now, was.since),
			})
		}
		// A poll that answered inside the budget is also the end of a slow
		// run, so the latch is cleared even when the answer arrived late in
		// the previous poll. Emptied without a line: a failed poll has its
		// own outage line, and a recovery from one already says the engine
		// came back.
		if r.took < slowPollThreshold {
			if since, ok := c.slow[key]; ok {
				delete(c.slow, key)
				changes = append(changes, healthChange{p: p, kind: changeFast, since: since, heldFor: core.Age(now, since)})
			}
		} else if _, ok := c.slow[key]; !ok {
			c.slow[key] = now
			changes = append(changes, healthChange{p: p, kind: changeSlow, since: now, took: r.took})
		}
		ps.Models = r.m.Models
		ps.Running = r.m.Running
		ps.Waiting = r.m.Waiting
		ps.TTFTms = r.m.TTFTms
		if port, loopback := loopbackPort(p.Addr); port > 0 && loopback {
			if proc, ok := byPort[port]; ok {
				ps.PID, ps.ProcRSS, ps.ProcCPU = proc.PID, proc.RSS, proc.CPUPct
			}
		}
		if r.m.Version != "" {
			ps.Version = r.m.Version
		}
		if name := probe.SelectModel(r.m.Models); name != "" {
			c.lastModel[key] = name
		} else {
			// Successful poll with nothing loaded: a stale id would
			// make the next 'p' JIT-load (or bill) a cold model, and
			// an unloaded engine has no KV cache in use.
			delete(c.lastModel, key)
			delete(c.kvPct, key)
		}
		if r.m.HasKV {
			ps.KVPct = r.m.KVPct
			c.kvPct[key] = ps.KVPct
		} else {
			ps.KVPct = c.kvPct[key]
		}
		ps.OutTokPS, ps.InTokPS = c.rates(key, r.m, now)
		outR.push(ps.OutTokPS, now)
		inR.push(ps.InTokPS, now)
	}
	ps.OutHist, ps.OutStamps = outR.copy(), outR.times()
	ps.InHist, ps.InStamps = inR.copy(), inR.times()
	if ps.Err != "" {
		if _, ok := c.down[key]; !ok {
			c.down[key] = downState{since: now, reason: ps.Err}
			changes = append(changes, healthChange{p: p, kind: changeDown, reason: ps.Err, since: now})
		}
		// The outage supersedes any slow run in progress: the engine is not
		// answering, and its next answer is measured fresh, so a stale latch
		// cannot report a slowdown that ended before the outage began.
		delete(c.slow, key)
	}
	return ps, changes, homeUnknown
}

// providerKey is the per-provider state key: the endpoint when known, else
// the display label. Endpoints are unique per instance; labels repeat across
// instances of the same engine kind.
func providerKey(p provider.Provider) string {
	if addr := p.Addr; addr != "" {
		return addr
	}
	return p.Label
}

// changeKind is which boundary an engine crossed on the last poll. A poll can
// cross two at once (an engine recovers and comes back slow), so a change
// carries its own kind rather than being inferred from the snapshot entry.
type changeKind uint8

const (
	// changeDown and changeUp are the answering boundary: a poll that
	// failed, and one that answered after failing.
	changeDown changeKind = iota
	changeUp
	// changeSlow and changeFast are the latency boundary: a poll that
	// answered but took too long, and one that answered in time again.
	changeSlow
	changeFast
)

// healthChange is one engine crossing the answering or the latency boundary.
// reason is the failure text that started an answering run, so the recovery
// line names the outage it ends. took is the duration that tripped the latency
// boundary, and is zero on the change that ends the run. heldFor is how long
// the run lasted when the change was reported.
type healthChange struct {
	p       provider.Provider
	kind    changeKind
	reason  string
	since   time.Time
	heldFor time.Duration
	took    time.Duration
}

// slowPollThreshold is the duration past which a poll that still answered is
// audited. Half of provider.PollTimeout: an engine that needs more of the
// budget than that is on its way to the timeout that turns the same engine
// into an outage line, and until that timeout the dashboard reports it
// healthy, since a slow answer and a fast one are the same green. A var so
// tests can shrink it instead of sleeping past the real one.
var slowPollThreshold = provider.PollTimeout / 2

// logChanges writes one audit line per engine in a boundary bucket. The
// transitions are already deduplicated in emit, so a fleet of engines that is
// down produces one line when it goes down and one when it returns however
// many intervals passed in between.
//
// heldKey names how long the run lasted under each boundary, and the two
// boundaries carry different extras: an answering transition names the failure
// text that started the outage, and a latency one the duration that tripped it,
// which no snapshot exposes, since the dashboard shows whether an engine
// answered but never how long the answer took.
//
// reasonKey names the field the failure text is written under, and it is empty
// for the transitions that have no failure to report. The two are one decision
// rather than two: a line that names a run that already ended is an operator's
// "what is broken right now" filter picking up a resolved outage, and every
// other transition line in this package (auditProbe) already withholds it. So
// the boundary that starts a run carries the reason and the one that ends it
// does not, and the field says which failure it was: `reason` on the line that
// reports the engine going down, `down_reason` on the line that reports it
// coming back and naming the outage that just ended.
func logChanges(changes []healthChange, level slog.Level, msg, heldKey, reasonKey string) {
	if len(changes) == 0 {
		return
	}
	lg := audit()
	for _, ch := range changes {
		attrs := []any{
			"engine", logcfg.Field(ch.p.Label, 128),
			"addr", logcfg.Field(ch.p.Addr, logcfg.FieldCap),
		}
		if reasonKey != "" && ch.reason != "" {
			attrs = append(attrs, reasonKey, logcfg.Field(ch.reason, logcfg.FieldCap))
		}
		if ch.took > 0 {
			attrs = append(attrs, "duration", ch.took.Round(time.Millisecond))
		}
		if ch.heldFor > 0 {
			attrs = append(attrs, heldKey, ch.heldFor.Round(time.Millisecond))
		}
		lg.Log(context.Background(), level, msg, attrs...)
	}
}

// logHomeUnknown audits that engine errors keep their home paths, which is
// what RedactHome's own home lookup failing does to them. Called after c.mu is
// released, like logChanges, because a stalled stderr must not stall the poll
// loop the fold is part of.
func logHomeUnknown() {
	_, err := os.UserHomeDir()
	if err == nil {
		return
	}
	audit().Warn("toktop: home directory unknown; engine errors keep their home paths",
		"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
}
