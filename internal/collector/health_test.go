package collector

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The fold memo answers a repeated poll error from memory, because a downed
// engine repeats one error verbatim and folding it again costs a pass over
// every byte the engine chose to send. It is keyed on a digest rather than the
// text, so the memo has to stay correct for an error too large to be worth
// holding, and the retained state must not grow with what the peer sent.

func TestFoldErrMemoAnswersRepeat(t *testing.T) {
	c := New(nil, 0)
	err := errors.New("dial tcp 127.0.0.1:11434: connect: connection refused")

	first := c.foldErr("127.0.0.1:11434", err)
	second := c.foldErr("127.0.0.1:11434", err)
	if first != second {
		t.Fatalf("repeat of the same error folded twice: %q then %q", first, second)
	}
	if first == "" {
		t.Fatal("fold of a repeated error is empty")
	}
}

func TestFoldErrMemoIsPerError(t *testing.T) {
	c := New(nil, 0)
	a := c.foldErr("k", errors.New("connection refused"))
	b := c.foldErr("k", errors.New("no such host"))
	if a == b {
		t.Fatalf("two distinct errors shared one fold: %q", a)
	}
}

// An engine answering with a decoder's whole offending literal puts megabytes
// into one error. The published text is bounded, and the memo must be too, or
// one peer sizes the collector's memory for the life of the process.
//
// The bound is asserted over every string the memo entry holds rather than
// over a named field, so it holds whichever field carries the memo key: an
// entry retaining the error text itself fails here no matter what it is called.
func TestFoldErrRetainsBoundedState(t *testing.T) {
	c := New(nil, 0)
	huge := errors.New(strings.Repeat("9", 8<<20))

	text := c.foldErr("127.0.0.1:8000", huge)
	if len(text) > 4096 {
		t.Fatalf("published error text is %d bytes, not bounded: %.60q", len(text), text)
	}
	f, ok := c.errFold["127.0.0.1:8000"]
	if !ok {
		t.Fatal("fold was not memoized")
	}
	v := reflect.ValueOf(f)
	for i := range v.NumField() {
		fv := v.Field(i)
		if fv.Kind() != reflect.String {
			continue
		}
		if n := fv.Len(); n > 4096 {
			t.Fatalf("memo holds %d bytes in %s, not bounded: %.60q",
				n, v.Type().Field(i).Name, fv.String())
		}
	}
	// A repeat of the oversized error is still answered from the memo.
	if again := c.foldErr("127.0.0.1:8000", huge); again != text {
		t.Fatalf("oversized error did not hit the memo: %q then %q", text, again)
	}
}
