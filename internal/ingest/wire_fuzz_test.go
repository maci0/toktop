package ingest

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// FuzzEventFromWire drives the wire decoder with the fields fuzzed apart
// rather than as one body of bytes. handlePost fuzzes the body a sender
// writes, but a fuzzer mutating a body has to synthesize well-formed JSON
// around a number before it reaches the decoder, so the spellings that carry
// the risk here are exactly the ones it never produces: values straddling
// 2^63, an exponent form, a trailing .0, a whole number past what a float64
// names exactly. Splitting ts, id and the four numeric fields into their own
// inputs reaches each of them on the first iteration.
//
// The oracle is a second, independent reading of the same number. big.Rat
// reads an integer a JSON number spells exactly, and parseTokenJSON's whole
// job is to agree with it: the answer has to be that integer, or an error if
// it does not fit int64. A spelling the two readers disagree on is a count
// the sender cannot predict from the value it wrote.
//
// Every field the decoder accepted is a value that reaches the retained feed
// and the --json report, so the boundary guarantees the callers rely on are
// asserted too: token counts non-negative and inside ClampEventTokens, the
// span inside MaxEventSpan, the id inside its cap, and a refusal carrying
// neither an event nor a reason a sender cannot act on.
func FuzzEventFromWire(f *testing.F) {
	wholeInts := []string{
		"0", "-0", "1", "-1", "100", "100.0", "1e2", "1E2", "1e+2", "-1e-2",
		"0.0", "-0.0", "2", "3.0", "1e0", "-1e0", "1.5", "0.1", "1.0000000000000001",
	}
	// The magnitudes where a float64 stops naming every integer it holds, and
	// where a count just outside int64 rounds onto the extreme and looks
	// in-range. Each is spelled three ways: as an integer, with a fraction
	// part, and in exponent form.
	const (
		maxI64     = "9223372036854775807"
		overMaxI64 = "9223372036854775808"
		underMin   = "-9223372036854775808"
		underMinB  = "-9223372036854775809"
		pow62      = "4611686018427387904" // 2^62
		pow62p     = "4611686018427387905"
		pow63      = "9223372036854775808" // 2^63
		pow63m     = "9223372036854775807" // 2^63 - 1
	)
	boundaries := []string{
		maxI64, overMaxI64, underMin, underMinB,
		pow62, pow62p, pow63, pow63m,
		"9.223372036854775e18", "9.223372036854776e18", "9.223372036854776808e18",
		"-9.223372036854775e18", "-9.223372036854776e18", "-9.223372036854775808e18",
		"-9.223372036854775807e18", "-9.223372036854776e18",
		"4611686018427387904.0", "4611686018427387905.0",
		"4.611686018427388e18", "4.6116860184273879e18",
		"1e308", "1e309", "-1e309", "1e-309", "1e-400",
		"9223372036854775807.0", "-9223372036854775808.0",
		"999999999999999999999999999999", "-0", "-0.0", "-1e2",
	}
	notNumbers := []string{
		"", " ", "null", "true", "false", `"5"`, `"1e3"`, "[]", "{}", "abc",
		"NaN", "Inf", "-Inf", "0x10", "1_0", "1/2", "+5", "1.2.3", "1e", "1e+",
		".5", "5.", "--5", "1 2", "1,5", "0b101", "٥", "1e١٨", "00012",
	}
	stamps := []string{
		"", " ", "null", `"2026-01-02T03:04:05Z"`, `"2026-01-02T05:04:05+02:00"`,
		`" 2026-01-02T03:04:05Z "`, `"2026-01-02T03:04:05"`, `"yesterday"`, `5`,
		`"0000-01-01T00:00:00Z"`, `"9999-12-31T23:59:59Z"`, `"2026-13-45T99:99:99Z"`,
		`"\ud800"`, `"2026-01-02T03:04:05.999999999Z"`, `"20260102T030405Z"`,
	}
	ids := []string{
		"", "turn-1", "~", "/home/dev/x", "/home/dev",
		strings.Repeat("a", 128), strings.Repeat("a", 129),
		strings.Repeat("\U0001F1E9", 129), "\u0000", "\u001b[2J", "café",
		"cafe\u0301", "/home/dev/" + strings.Repeat("a", 300),
	}

	numbers := append(append(append([]string{}, wholeInts...), boundaries...), notNumbers...)

	// Each numeric spelling on its own, and each one paired with a different
	// spelling in the other three fields: the four are read by one function
	// and each keeps its own field name in the refusal, so a seed that leaves
	// three of them at a value that always parses checks only the first.
	for _, n := range numbers {
		f.Add(stamps[0], ids[0], n, n, n, n)
	}
	for i, n := range numbers {
		for _, j := range []int{i + 1, i + 2, i + 3} {
			m := numbers[j%len(numbers)]
			f.Add(stamps[i%len(stamps)], ids[i%len(ids)], n, m, numbers[(i+1)%len(numbers)], m)
		}
	}
	// The non-numeric fields against the numbers that parse, so a stamp or an
	// id that decides the answer is reached with a body that otherwise works.
	for _, s := range stamps {
		f.Add(s, "turn-1", "10", "20", "30", "40")
	}
	for _, id := range ids {
		f.Add(`"2026-01-02T03:04:05Z"`, id, "10", "20", "30", "40")
	}
	// A refusal in one field must not stop the others being read, so the
	// boundary spellings also travel beside values that parse.
	for i, n := range numbers {
		f.Add(stamps[i%len(stamps)], ids[i%len(ids)], n, "1e2", "100.0", "0")
	}

	f.Fuzz(func(t *testing.T, ts, id, prompt, output, thinking, span string) {
		wire := agentEventWire{
			At:             json.RawMessage(ts),
			ID:             id,
			Agent:          "coder",
			Model:          "sonnet",
			Kind:           core.AgentKindTool,
			PromptTokens:   json.RawMessage(prompt),
			OutputTokens:   json.RawMessage(output),
			ThinkingTokens: json.RawMessage(thinking),
			ViaEngine:      "127.0.0.1:11434",
			Note:           "/home/dev/proj",
			SpanMs:         json.RawMessage(span),
		}
		ev, err := eventFromWire(wire)

		again, err2 := eventFromWire(wire)
		if !sameEvent(ev, again) || !sameError(err, err2) {
			t.Fatalf("eventFromWire is not deterministic for %+v: %+v/%v then %+v/%v",
				wire, ev, err, again, err2)
		}

		if err != nil {
			if (ev != core.AgentEvent{}) {
				t.Fatalf("refused wire %+v produced an event: %+v", wire, ev)
			}
			assertSenderReason(t, err.Error())
			return
		}

		for _, f := range []struct {
			field string
			n     int64
		}{
			{"prompt_tokens", ev.PromptTokens},
			{"output_tokens", ev.OutputTokens},
			{"thinking_tokens", ev.ThinkingTokens},
		} {
			if f.n < 0 {
				t.Fatalf("accepted wire %+v kept a negative %s: %d", wire, f.field, f.n)
			}
			if f.n != core.ClampEventTokens(f.n) {
				t.Fatalf("accepted wire %+v kept an out-of-bound %s: %d", wire, f.field, f.n)
			}
		}
		if ev.Span < 0 || ev.Span > core.MaxEventSpan {
			t.Fatalf("accepted wire %+v kept span %v, bound %v", wire, ev.Span, core.MaxEventSpan)
		}
		if n := uniseg.GraphemeClusterCount(ev.ID); n > core.AgentIDMax {
			t.Fatalf("accepted wire %+v kept an id of %d characters, cap %d", wire, n, core.AgentIDMax)
		}
		if ev.ID == "" && strings.TrimSpace(id) != "" {
			t.Fatalf("accepted wire %+v stored an empty id for %q", wire, id)
		}
		// A stamp the decoder treats as absent is the zero the handler stamps
		// on arrival. The claim runs one way only: a sender can write the
		// zero instant itself ("0001-01-01T00:00:00Z"), and a time.Time
		// cannot tell that from no stamp at all, so the converse does not
		// hold and is not asserted.
		if stampAbsent(ts) && !ev.At.IsZero() {
			t.Fatalf("accepted wire %+v read absent ts %q as %v", wire, ts, ev.At)
		}
		if ev.Kind != core.AgentKindTool || ev.Agent != "coder" {
			t.Fatalf("accepted wire %+v altered an untouched field: %+v", wire, ev)
		}
		if !utf8.ValidString(ev.Note) {
			t.Fatalf("accepted wire %+v produced an invalid note: %q", wire, ev.Note)
		}

		// The differential oracle on all four numbers the wire carries, and
		// the pair assertion across the decode: the count the parser read has
		// to be the count the event kept, or the one the clamp turned into
		// zero. A value the parser refused cannot have reached the event at
		// all, and eventFromWire already returned for that case above.
		for _, n := range []struct {
			field  string
			raw    string
			stored int64
		}{
			{"prompt_tokens", prompt, ev.PromptTokens},
			{"output_tokens", output, ev.OutputTokens},
			{"thinking_tokens", thinking, ev.ThinkingTokens},
		} {
			parsed, perr := parseTokenJSON([]byte(n.raw), n.field)
			if perr != nil {
				t.Fatalf("accepted wire %+v refused %s = %s: %v", wire, n.field, n.raw, perr)
			}
			assertAgreesWithRat(t, n.field, n.raw, parsed, perr)
			if want := core.ClampEventTokens(parsed); n.stored != want {
				t.Fatalf("%s = %s read as %d, stored %d, clamp gives %d", n.field, n.raw, parsed, n.stored, want)
			}
		}
		ms, serr := parseTokenJSON([]byte(span), "span_ms")
		if serr != nil {
			t.Fatalf("accepted wire %+v refused span_ms = %s: %v", wire, span, serr)
		}
		assertAgreesWithRat(t, "span_ms", span, ms, serr)
		wantSpan := time.Duration(0)
		if ms >= 0 && ms <= maxSpanMs {
			wantSpan = time.Duration(ms) * time.Millisecond
		}
		if ev.Span != wantSpan {
			t.Fatalf("span_ms = %s read as %d ms, stored %v, want %v", span, ms, ev.Span, wantSpan)
		}
	})
}

// stampAbsent reports whether parseEventTime reads a raw ts field as no
// stamp at all. The trim happens twice, once on the raw field and once on the
// unquoted string, so a sender that writes a quoted tab or a quoted newline
// leaves nothing behind either and the event is stamped on arrival.
func stampAbsent(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" {
		return true
	}
	v, err := strconv.Unquote(s)
	if err != nil {
		return false
	}
	return strings.TrimSpace(v) == ""
}

// sameEvent compares two decoded events field by field rather than with ==
// on the struct, so the two calls can be made with == apart over one thing:
// time.Time carries its *time.Location by pointer, and time.Parse allocates a
// fresh Location for a zone offset that is not a whole hour ("+00:01"), so the
// two stamps are the same instant with two different pointers. The instant and
// the zone offset are what an event carries, and both are compared here.
func sameEvent(a, b core.AgentEvent) bool {
	rest := a
	rest.At = time.Time{}
	other := b
	other.At = time.Time{}
	if rest != other {
		return false
	}
	return a.At.Equal(b.At) && a.At.Format("-07:00") == b.At.Format("-07:00")
}

// sameError compares two decode answers, treating a nil and an error as
// different. The decoder is reached twice with the same wire, so the two
// answers have to match exactly, message included.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

// assertAgreesWithRat checks a decoded number against an independent reading
// of the same spelling. A JSON number that reads as an integer has exactly
// one value, so the two readers must return the same one, or the decoder must
// refuse it.
//
// The claim is scoped to JSON numbers, which is what isJSONNumber gates on.
// big.Rat also reads spellings the wire cannot carry: a fraction ("1/2"), a
// hexadecimal float ("0x10"), and a number with no integer part before the
// point (".7e18"). A sender cannot write any of them, and the decoder
// deliberately refuses to widen the number syntax to reach them, so they
// carry no claim here.
func assertAgreesWithRat(t *testing.T, field, raw string, got int64, err error) {
	t.Helper()
	s := strings.TrimSpace(raw)
	if !isJSONNumber(s) {
		return
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || !r.IsInt() {
		return
	}
	want := r.Num()
	if want.Cmp(minInt64Big) < 0 || want.Cmp(maxInt64Big) > 0 {
		if err == nil {
			t.Fatalf("%s = %s decoded to %d, which is outside int64", field, raw, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s = %s was refused although it is the in-range integer %s", field, raw, want)
	}
	if big.NewInt(got).Cmp(want) != 0 {
		t.Fatalf("%s = %s decoded to %d, want %s", field, raw, got, want)
	}
}

// assertSenderReason checks a refusal names something the sender can act on.
// The client-facing reason is all a sender has to go on, and a Go type name
// or a field path from the decoder is neither documented nor actionable.
func assertSenderReason(t *testing.T, msg string) {
	t.Helper()
	if msg == "" {
		t.Fatal("refusal carried no reason")
	}
	// The "bad json:" prefix is the documented shape of a body refusal, so
	// what is checked here is the decoder's own vocabulary reaching a sender:
	// a Go type name, a struct field path, or a strconv message.
	for _, leaked := range []string{
		"agentEventWire", "Go value", "Go struct", "strconv", "ParseInt", "ParseFloat",
		"cannot unmarshal", "invalid syntax", "value out of range", "big.Rat", "Rat.SetString",
	} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("refusal reason leaks the decoder: %q", msg)
		}
	}
	for _, named := range []string{
		"prompt_tokens", "output_tokens", "thinking_tokens", "span_ms", "id", "RFC 3339",
	} {
		if strings.Contains(msg, named) {
			return
		}
	}
	t.Fatalf("refusal reason names nothing the sender wrote: %q", msg)
}
