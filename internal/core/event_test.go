package core

import (
	"cmp"
	"slices"
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
	nfcEvent := []AgentEvent{{ID: "caf\u00e9", Agent: "coder"}}
	if !HasAgentID(nfcEvent, "cafe\u0301") {
		t.Fatal("NFD id must match NFC event id")
	}
	nfdEvent := []AgentEvent{{ID: "cafe\u0301", Agent: "coder"}}
	if !HasAgentID(nfdEvent, "caf\u00e9") {
		t.Fatal("NFC id must match NFD event id")
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

func TestAppendSorted(t *testing.T) {
	intCmp := cmp.Compare[int]
	var s []int
	for _, v := range []int{5, 2, 8, 1, 4} {
		s = AppendSorted(s, v, 8, intCmp)
	}
	want := []int{1, 2, 4, 5, 8}
	if !slices.Equal(s, want) {
		t.Fatalf("AppendSorted got %v, want %v", s, want)
	}
}

func TestAppendSortedTrimsToTheWindow(t *testing.T) {
	intCmp := cmp.Compare[int]
	var s []int
	for _, v := range []int{5, 2, 8, 1, 4} {
		s = AppendSorted(s, v, 3, intCmp)
	}
	if !slices.Equal(s, []int{4, 5, 8}) {
		t.Fatalf("AppendSorted got %v, want the newest three [4 5 8]", s)
	}
}

// Stability is the point of the binary search: the element lands after every
// one equal to it, so two equal timestamps order by arrival rather than
// swapping on each insert. Distinct operands cannot tell a stable insert from
// an unstable one, so this inserts duplicates.
func TestAppendSortedKeepsEqualElementsInArrivalOrder(t *testing.T) {
	intCmp := cmp.Compare[int]
	s := []int{}
	for _, v := range []int{5, 2, 5, 2, 5, 2, 1, 5, 1} {
		s = AppendSorted(s, v, 9, intCmp)
	}
	if !slices.Equal(s, []int{1, 1, 2, 2, 2, 5, 5, 5, 5}) {
		t.Fatalf("AppendSorted got %v, want []int{1 1 2 2 2 5 5 5 5}", s)
	}
}
