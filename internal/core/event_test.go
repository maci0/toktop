package core

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHasAgentID(t *testing.T) {
	events := []AgentEvent{
		{ID: "a", Agent: "coder"},
		{ID: "b", Agent: "coder"},
		{Agent: "no-id"},
	}
	if !HasAgentID(events, "a") || !HasAgentID(events, "b") {
		t.Fatal("present ids must match")
	}
	if HasAgentID(events, "c") {
		t.Fatal("unknown id matched")
	}
	if HasAgentID(events, "") {
		t.Fatal("empty id must never match")
	}
	if HasAgentID(nil, "a") {
		t.Fatal("empty feed matched")
	}
}

// The ingest id-dedup window is the retained event ring. README's Agent feed
// table must name AgentHistoryLen so a ring bump cannot ship with a stale cap.
func TestREADMEDocumentsAgentHistoryLen(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("last %d events", AgentHistoryLen)
	if !strings.Contains(string(b), want) {
		t.Fatalf("README.md ingest id row must say %q (matches AgentHistoryLen)", want)
	}
}

func TestAgentCmp(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	t1 := time.Unix(2_000, 0)
	cases := []struct {
		name string
		a, b AgentEvent
		want int
	}{
		{"time before", AgentEvent{At: t0}, AgentEvent{At: t1}, -1},
		{"time after", AgentEvent{At: t1}, AgentEvent{At: t0}, 1},
		{"agent name", AgentEvent{At: t0, Agent: "a"}, AgentEvent{At: t0, Agent: "b"}, -1},
		{"id", AgentEvent{At: t0, Agent: "a", ID: "1"}, AgentEvent{At: t0, Agent: "a", ID: "2"}, -1},
		{"note", AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, AgentEvent{At: t0, Agent: "a", ID: "1", Note: "beta"}, -1},
		{"equal", AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, 0},
	}
	for _, tc := range cases {
		c := AgentCmp(tc.a, tc.b)
		switch {
		case tc.want < 0 && c >= 0:
			t.Errorf("%s: AgentCmp = %d, want < 0", tc.name, c)
		case tc.want > 0 && c <= 0:
			t.Errorf("%s: AgentCmp = %d, want > 0", tc.name, c)
		case tc.want == 0 && c != 0:
			t.Errorf("%s: AgentCmp = %d, want 0", tc.name, c)
		}
	}
}

func TestProbeCmp(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	t1 := time.Unix(2_000, 0)
	cases := []struct {
		name string
		a, b ProbeSample
		want int
	}{
		{"time before", ProbeSample{At: t0}, ProbeSample{At: t1}, -1},
		{"time after", ProbeSample{At: t1}, ProbeSample{At: t0}, 1},
		{"addr", ProbeSample{At: t0, Addr: "127.0.0.1:8000"}, ProbeSample{At: t0, Addr: "127.0.0.1:9000"}, -1},
		{"model", ProbeSample{At: t0, Addr: "127.0.0.1:8000", Model: "a"}, ProbeSample{At: t0, Addr: "127.0.0.1:8000", Model: "b"}, -1},
		{"equal", ProbeSample{At: t0, Addr: "127.0.0.1:8000", Model: "a"}, ProbeSample{At: t0, Addr: "127.0.0.1:8000", Model: "a"}, 0},
	}
	for _, tc := range cases {
		c := ProbeCmp(tc.a, tc.b)
		switch {
		case tc.want < 0 && c >= 0:
			t.Errorf("%s: ProbeCmp = %d, want < 0", tc.name, c)
		case tc.want > 0 && c <= 0:
			t.Errorf("%s: ProbeCmp = %d, want > 0", tc.name, c)
		case tc.want == 0 && c != 0:
			t.Errorf("%s: ProbeCmp = %d, want 0", tc.name, c)
		}
	}
}

func TestInsertSorted(t *testing.T) {
	intCmp := cmp.Compare[int]
	var s []int
	s = InsertSorted(append(s, 5), intCmp)
	s = InsertSorted(append(s, 2), intCmp)
	s = InsertSorted(append(s, 8), intCmp)
	s = InsertSorted(append(s, 1), intCmp)
	s = InsertSorted(append(s, 4), intCmp)
	want := []int{1, 2, 4, 5, 8}
	for i, v := range s {
		if v != want[i] {
			t.Fatalf("InsertSorted got %v, want %v", s, want)
		}
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("InsertSorted on empty slice must panic")
		}
	}()
	InsertSorted([]int{}, intCmp)
}
