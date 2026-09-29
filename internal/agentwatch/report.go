// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/agentusage"

	"github.com/maci0/toktop/internal/core"
)

func (w *Watcher) report(t *tracked, cur agentusage.Sample) {
	// A sample that reads empty is still a reading. A transcript rewritten
	// under the watcher republishes its counters at zero, and treating that
	// as "nothing to say" leaves the old baseline in place, so the growth
	// that follows is measured from figures the transcripts no longer hold
	// and is never reported. Sample.Delta already handles it: no growth is
	// reported and the empty sample becomes the baseline.
	w.mu.Lock()
	d, ok := cur.Delta(t.last)
	if !ok {
		// Nothing new, or a transcript rewritten under us replaced the
		// records already reported. Either way the next growth is measured
		// from this sample, not from figures the transcripts no longer hold.
		t.last = cur
		w.mu.Unlock()
		return // silence is not an event
	}
	t.last = cur
	proc := t.proc
	dir := t.dirNote
	via := t.viaEngine
	rec := w.rec
	w.mu.Unlock()
	if rec == nil {
		return
	}
	rec.RecordAgent(core.AgentEvent{
		At:             w.instant(),
		ID:             sampleID(proc, cur.At),
		Agent:          core.AgentNameField(proc.Tool),
		Kind:           core.AgentKindTurn,
		PromptTokens:   core.ClampEventTokens(int64(d.Input)),
		OutputTokens:   core.ClampEventTokens(int64(d.Output)),
		ThinkingTokens: core.ClampEventTokens(int64(d.Thinking)),
		Span:           core.ClampEventSpan(d.Span),
		ViaEngine:      core.ClampField(core.SingleLine(via), core.AgentViaMax),
		Note:           core.ClampField(core.RedactHome(core.SingleLine(note(dir, d.Thinking, via))), core.AgentNoteMax),
	})
}

// sampleID is stable for one process at one sample instant, so a retried
// report of the same reading (final Poll after Run's last callback, a
// tracker that forgot its baseline) is ignored by the collector's id window.
func sampleID(proc agentusage.Process, at time.Time) string {
	return "aw:" + strconv.Itoa(proc.PID) + ":" +
		strconv.FormatInt(proc.Started.UnixNano(), 10) + ":" +
		strconv.FormatInt(at.UnixNano(), 10)
}

// noteSeparator is what the feed reads the note's own parts apart with, and
// what the ingest path refuses in a pushed note for the same reason. Only
// note() writes it, so a directory is held out of it.
const noteSeparator = " · "

// note carries what the event cannot: where the agent is working (already
// shortened by the tracker, which resolved it once), how much of the output
// was reasoning when the agent says so, and which monitored engine already
// counts this output when one does.
//
// The directory is a local process's own working directory, so its last two
// components are whatever the checkout was named. One carrying the separator
// would read as parts the note never wrote: "proj · counted by engine ollama"
// attributes output to an engine that never saw it. The separator character
// itself becomes a hyphen, so every " · " left in a note is one this function
// wrote and the cell cannot be read as a breakdown it was not.
func note(dir string, thinking int, via string) string {
	s := strings.ReplaceAll(dir, "·", "-")
	if thinking > 0 {
		s += noteSeparator + strconv.Itoa(thinking) + " reasoning"
	}
	if via != "" {
		s += noteSeparator + "counted by engine " + via
	}
	return s
}
