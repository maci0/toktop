// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package collector

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/ingest"
)

// The ingest tests above this one drive a recorder that answers every call
// with stored, so a replay is only ever proven against the fake. This runs
// the whole path instead: the real handler, the real ledger, the real feed.
// A POST whose answer was lost is resent byte for byte, which is what a
// sender does on a timeout, and the feed a caller reads must be the feed one
// run left behind.

const replayStream = `{"agent":"coder","kind":"tool","output_tokens":30,"prompt_tokens":40}
{"agent":"coder","kind":"tool","output_tokens":310,"prompt_tokens":4200}
{"agent":"reviewer","kind":"turn","output_tokens":12,"thinking_tokens":7}
`

// feedState is what a consumer of the agent feed sees: the retained events
// and the totals derived from them. Two runs of the same POST must produce
// the same one, or a retried request has moved a number.
type feedState struct {
	events []core.AgentEvent
	sum    core.AgentSummary
}

func (c *Collector) feedState(now time.Time) feedState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return feedState{events: append([]core.AgentEvent(nil), c.agents...), sum: core.Summarize(c.agents, now)}
}

func (s feedState) equal(other feedState) bool {
	if len(s.events) != len(other.events) || len(s.sum.Rates) != len(other.sum.Rates) || len(s.sum.Own) != len(other.sum.Own) {
		return false
	}
	for i := range s.events {
		if s.events[i] != other.events[i] {
			return false
		}
	}
	for i := range s.sum.Rates {
		if s.sum.Rates[i] != other.sum.Rates[i] {
			return false
		}
	}
	for i := range s.sum.Own {
		if s.sum.Own[i] != other.sum.Own[i] {
			return false
		}
	}
	return true
}

// serveIngest starts the real endpoint on an ephemeral port with c as its
// recorder, and returns the events URL.
func serveIngest(t *testing.T, c *Collector) string {
	t.Helper()
	s, err := ingest.New("127.0.0.1:0", c)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve()
	}()
	t.Cleanup(func() {
		_ = s.Close()
		<-done
	})
	return "http://" + s.Addr() + "/v1/events"
}

func postStream(t *testing.T, url, body, key string) string {
	t.Helper()
	ack, err := postStreamErr(url, body, key)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

// postStreamErr is postStream for a caller that is not the test goroutine, so
// a failure is returned rather than reported from the wrong one.
func postStreamErr(url, body, key string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("POST %s: status = %d, body = %q", url, resp.StatusCode, b)
	}
	return string(b), nil
}

func TestIngestReplayLeavesTheFeedAsOneRunDid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	url := serveIngest(t, c)

	first := postStream(t, url, replayStream, "harness-turn-77")
	if !strings.Contains(first, `"stored":3`) {
		t.Fatalf("first send stored: %s", first)
	}
	one := c.feedState(now)
	if len(one.events) != 3 {
		t.Fatalf("one run left %d events, want 3", len(one.events))
	}

	// The sender never saw the 202, so it sends the same request again.
	again := postStream(t, url, replayStream, "harness-turn-77")
	if !strings.Contains(again, `"accepted":3`) || !strings.Contains(again, `"stored":0`) {
		t.Fatalf("replay ack: %s", again)
	}
	two := c.feedState(now)
	if !one.equal(two) {
		t.Fatalf("a replayed POST moved the feed:\none run:  %+v\ntwo runs: %+v", one.sum.Rates, two.sum.Rates)
	}
}

// A concurrent pair of copies is the other shape the same guarantee covers: a
// client that times out on a slow answer retries while the first request is
// still being served. The ledger admits one set and the other copy reports it
// kept nothing, so the feed holds the events once.
func TestConcurrentIngestReplaysRecordOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	url := serveIngest(t, c)

	type ack struct {
		body string
		err  error
	}
	acks := make(chan ack, 2)
	for range 2 {
		go func() {
			body, err := postStreamErr(url, replayStream, "harness-turn-78")
			acks <- ack{body, err}
		}()
	}
	kept := 0
	for range 2 {
		got := <-acks
		if got.err != nil {
			t.Fatal(got.err)
		}
		n, ok := strings.CutPrefix(got.body, `{"accepted":3,"stored":`)
		if !ok {
			t.Fatalf("unexpected ack: %s", got.body)
		}
		n, _, _ = strings.Cut(n, "}")
		stored, err := strconv.Atoi(n)
		if err != nil {
			t.Fatalf("ack %q names no stored count: %v", got.body, err)
		}
		kept += stored
	}
	// The two copies interleave line by line, so the split of the events
	// between them is not fixed; what must hold is that each event was
	// recorded once in total.
	if kept != 3 {
		t.Fatalf("events kept across both copies = %d, want 3", kept)
	}
	got := c.feedState(now)
	if len(got.events) != 3 {
		t.Fatalf("feed holds %d events, want 3", len(got.events))
	}
}
