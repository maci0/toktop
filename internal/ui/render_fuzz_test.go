// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
)

// fuzzFloat reinterprets 8 bytes as an IEEE double so the fuzzer reaches
// every float64 the parsers upstream admit, on the fuzzer's first iteration:
// negatives, subnormals, and the magnitudes past MaxInt64 that a hand-picked
// seed list never reaches. Float layout is not a stable part of the language,
// so math.Float64frombits is the one supported way to read these back.
//
// The result is finite on purpose, and that is a real constraint rather than a
// convenience. The parsers that produce a snapshot's floats filter NaN and the
// infinities before a value reaches a sample (gpu.flexF/posFinite,
// sysmon.ParseLoadavg, provider.finite), so a non-finite reading is not a state
// the pipeline is in and the renderers are not required to survive one.
// Widening the inputs to include them would only re-prove that JSON cannot
// spell an infinity, which is why reportFloat and addRate bound the aggregates
// and the report fields rather than this harness going looking for a parser
// bug that lives in another package.
func fuzzFloat(b []byte) float64 {
	v := math.Float64frombits(fuzzBits(b))
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// fuzzBits reads eight bytes, little-endian, as the bit pattern a double
// carries. sliceAt already guarantees eight bytes, but the helper reads a
// fixed window so it never reads out of range.
func fuzzBits(b []byte) uint64 {
	var n uint64
	for i := range 8 {
		if i < len(b) {
			n |= uint64(b[i]) << (8 * i)
		}
	}
	return n
}

// fuzzTokens builds a token count the way an ingest sender can spell one: a
// magnitude drawn from the fuzzer and a sign drawn from it too, so the
// negative and out-of-bound counts core.ClampEventTokens exists to drop are
// reached alongside the ones it keeps. The eight bytes are read straight into
// the int64 rather than routed through a float: converting an out-of-range
// double to an integer is implementation-defined, and a harness that depends
// on which value the platform happens to produce is not reproducible.
func fuzzTokens(sign []byte, mag []byte) int64 {
	n := int64(fuzzBits(mag))
	if len(sign) > 0 && sign[0]&1 == 0 {
		n = -n
	}
	return n
}

// fuzzText cuts the fuzzed bytes down to the printable-ish prefix a peer
// could have sent and returns it whole: the point is that the renderer sees
// the escape sequences, bidi marks and ill-formed UTF-8 a hostile label
// carries, not that they are pre-cleaned here.
func fuzzText(b []byte) string { return string(b) }

// fuzzSnapshot builds one snapshot whose every free-form field is a slice of
// the fuzzed bytes and whose every number is drawn from them. The event times
// walk backwards off one origin so the feed, the per-agent rates and the
// charts all have a real ordering to walk rather than a slice of identical
// instants, and the history stamps are offset from the values by a byte each,
// so the two can be mismatched the way an engine that joined late leaves them.
func fuzzSnapshot(sliceA, sliceB []byte) core.Snapshot {
	now := time.Unix(1_700_000_000, 0).UTC()

	// Two histories: one with a stamp per sample and one without, and one
	// whose stamps run out before its values do, so sampleTime's bounds and
	// lastSampleTime's scan both get exercised.
	outHist := fuzzFloats(sliceA, 6)
	inHist := fuzzFloats(sliceB, 4)
	providers := []core.ProviderSnapshot{
		{
			Label: fuzzText(sliceA), Kind: fuzzText(sliceB), Addr: fuzzText(sliceA),
			OK: len(sliceA)%2 == 0, Err: fuzzText(sliceB), Version: fuzzText(sliceA),
			PID: int(fuzzTokens(sliceB, sliceA)) >> 20, ProcRSS: uint64(fuzzFloat(sliceB)),
			ProcCPU: fuzzFloat(sliceA),
			Models: []core.ModelInfo{
				{Name: fuzzText(sliceA), SizeVRAM: uint64(fuzzFloat(sliceB)), CtxMax: uint64(fuzzFloat(sliceA))},
				{Name: fuzzText(sliceB)},
			},
			OutTokPS: fuzzFloat(sliceA), InTokPS: fuzzFloat(sliceB),
			Running: int(fuzzTokens(sliceA, sliceB)) >> 8, Waiting: int(fuzzTokens(sliceB, sliceA)) >> 8,
			KVPct: fuzzFloat(sliceB), TTFTms: fuzzFloat(sliceA),
			OutHist: outHist, InHist: inHist,
			OutStamps: fuzzStamps(sliceB, len(outHist), now, true),
			InStamps:  fuzzStamps(sliceA, len(inHist)-1, now, false),
		},
		{
			Label: fuzzText(sliceB), Addr: fuzzText(sliceB), OK: true,
			OutTokPS: fuzzFloat(sliceB), InTokPS: fuzzFloat(sliceA), KVPct: fuzzFloat(sliceA),
			OutHist: fuzzFloats(sliceB, 3), InHist: fuzzFloats(sliceA, 7),
		},
	}

	events := make([]core.AgentEvent, 0, 4)
	for i := range 4 {
		a, b := sliceAt(sliceA, i), sliceAt(sliceB, i)
		events = append(events, core.AgentEvent{
			At:             now.Add(-time.Duration(i) * time.Second),
			ID:             fuzzText(a),
			Agent:          fuzzText(b),
			Model:          fuzzText(a),
			Kind:           fuzzText(b),
			PromptTokens:   fuzzTokens(a, b),
			OutputTokens:   fuzzTokens(b, a),
			ThinkingTokens: fuzzTokens(a, a),
			ViaEngine:      fuzzText(b),
			Note:           fuzzText(a),
			Span:           time.Duration(fuzzTokens(b, b)),
		})
	}

	probes := make([]core.ProbeSample, 0, 2)
	for i := range 2 {
		a, b := sliceAt(sliceB, i), sliceAt(sliceA, i)
		probes = append(probes, core.ProbeSample{
			At: now.Add(-time.Duration(i) * time.Minute), Addr: fuzzText(a),
			Model: fuzzText(b), OK: i%2 == 0, Err: fuzzText(a),
			TTFTms: fuzzFloat(b), TokPS: fuzzFloat(a), Tokens: int(fuzzTokens(b, a)) >> 16,
		})
	}

	sys := &core.SysSample{
		CPUModel: fuzzText(sliceA), OsName: fuzzText(sliceB), Kernel: fuzzText(sliceA),
		MemTotal: uint64(fuzzFloat(sliceA)), MemUsed: uint64(fuzzFloat(sliceB)),
		SwapTotal: uint64(fuzzFloat(sliceB)), SwapUsed: uint64(fuzzFloat(sliceA)),
		Load1: fuzzFloat(sliceA), Load5: fuzzFloat(sliceB), Load15: fuzzFloat(sliceA),
		HostUptime: time.Duration(fuzzTokens(sliceB, sliceA)),
		Drivers:    map[string]string{fuzzText(sliceA): fuzzText(sliceB), fuzzText(sliceB): fuzzText(sliceA)},
		NPUs:       []string{fuzzText(sliceA), fuzzText(sliceB), ""},
		RemoteHost: fuzzText(sliceB), RemoteErr: fuzzText(sliceA),
		Temps: []core.TempReading{
			{Label: fuzzText(sliceA), MilliC: int(fuzzFloat(sliceB))},
			{Label: fuzzText(sliceB), MilliC: int(fuzzFloat(sliceA)), IsGPU: true},
		},
		GPUs: []core.GPUDevice{
			{Vendor: fuzzText(sliceA), Index: int(fuzzTokens(sliceA, sliceB)) >> 8, Name: fuzzText(sliceB),
				MilliC: int(fuzzFloat(sliceA)), MemUsed: uint64(fuzzFloat(sliceB)),
				MemTotal: uint64(fuzzFloat(sliceA)), UtilPct: fuzzFloat(sliceB),
				PowerW: fuzzFloat(sliceA), Driver: fuzzText(sliceB)},
			{Vendor: fuzzText(sliceB), Index: -1, Name: fuzzText(sliceA)},
		},
	}

	return core.Snapshot{
		At:        now,
		Uptime:    time.Duration(fuzzTokens(sliceA, sliceB)),
		Providers: providers,
		Agents:    events,
		Probes:    probes,
		Sys:       sys,
	}
}

// fuzzFloats reads n doubles out of b, reusing its bytes when it is too short
// so a small seed still fills the slice it was asked for.
func fuzzFloats(b []byte, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = fuzzFloat(sliceAt(b, i))
	}
	return out
}

// fuzzStamps builds the sample stamps a history carries. aligned walks one
// second per sample the way a poll on cadence does; unaligned spaces them by a
// fuzzer-drawn offset so two samples share an instant and two land minutes
// apart, which is what a coalesced tick and a slow scrape produce.
func fuzzStamps(b []byte, n int, origin time.Time, aligned bool) []time.Time {
	if n <= 0 {
		return nil
	}
	out := make([]time.Time, n)
	for i := range out {
		var step time.Duration
		if aligned {
			step = time.Duration(i) * time.Second
		} else {
			step = time.Duration(fuzzFloat(sliceAt(b, i))) * time.Millisecond
		}
		out[i] = origin.Add(-step)
	}
	return out
}

// sliceAt returns the 8 bytes at index i of b, wrapping and repeating so a
// short or empty slice still names a whole float. The bytes, not a random
// draw, so the coverage a run reaches is a property of the seed corpus and
// the same input always builds the same snapshot.
func sliceAt(b []byte, i int) []byte {
	var out [8]byte
	if len(b) == 0 {
		return out[:]
	}
	for j := range out {
		out[j] = b[(i*8+j)%len(b)]
	}
	return out[:]
}

// FuzzSnapshotRender drives every renderer that turns a snapshot into
// operator-visible output -- the --plain text report, the --json report, and
// the two agent chart histograms -- with a snapshot whose every field is
// built from the fuzzed bytes.
//
// The whole surface this fuzzes is one trust boundary. Every string in a
// snapshot is text another program wrote: a model id off an engine's
// /v1/models payload, an agent name off an event posted to the unauthenticated
// ingest endpoint, an error message off whatever answers a probed localhost
// port. Every number is one a parser accepted and kept, including the values
// far outside the range of a real measurement -- so what is being asked here is
// what happens once such a reading reaches a renderer. A crash, a hang, or a
// report that can repaint the operator's terminal is the blast radius: these
// run on every keystroke of an interactive dashboard, and --plain and --json
// are the output a script reads.
//
// The assertions are the guarantees each renderer makes rather than "it did
// not panic", so a correctness bug surfaces as a failure the fuzzer can see:
// the JSON report is well-formed and decodes back to the sanitized strings it
// published, no published string still carries anything SanitizeText strips,
// no rate or measurement the report publishes is NaN or infinite, the text
// report stays valid UTF-8 with no escape byte left in it, and both reports
// are byte-identical on a second render of the same snapshot.
func FuzzSnapshotRender(f *testing.F) {
	for _, seed := range [][2][]byte{
		{[]byte("coder"), []byte("sonnet")},
		{nil, nil},
		{[]byte("\x1b]0;pwned\x07"), []byte("\x1b[2J")},
		{[]byte("\xff\xfe\x00"), []byte("café")},
		{[]byte("up\n9/9 engines"), []byte("agent\tstopped")},
		{[]byte{0x01, 0, 0, 0, 0, 0, 0xF0, 0x7F}, []byte{0, 0, 0, 0, 0, 0, 0xF0, 0xFF}},                // NaN, -Inf
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xEF, 0x7F}, []byte{0, 0, 0, 0, 0, 0, 0x10, 0x00}}, // +Inf, subnormal
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, []byte{0, 0, 0, 0, 0, 0, 0, 0}},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, sliceA, sliceB []byte) {
		snap := fuzzSnapshot(sliceA, sliceB)
		cfg := Config{
			Version:    "fuzz",
			Agents:     true,
			IngestAddr: fuzzText(sliceA),
			PollEvery:  time.Duration(fuzzTokens(sliceB, sliceA)),
		}

		// The --plain report: prose, no clipping, one line per section.
		plain := PlainTextFrame(cfg, snap)
		assertReportSafe(t, "plain", plain)
		if again := PlainTextFrame(cfg, snap); again != plain {
			t.Fatal("PlainTextFrame is not deterministic on the same snapshot")
		}

		// The --json report: the machine-readable half, which a script reads.
		out, err := JSONFrame(cfg, snap)
		if err != nil {
			t.Fatalf("JSONFrame refused a snapshot it must report: %v", err)
		}
		assertReportSafe(t, "json", out)
		if again, err2 := JSONFrame(cfg, snap); err2 != nil || again != out {
			t.Fatalf("JSONFrame is not deterministic: %v then %v", out, again)
		}

		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("JSONFrame wrote a document that does not decode: %v\n%s", err, out)
		}
		if got := doc["schema"]; got != float64(jsonReportSchema) {
			t.Fatalf("report schema = %v, want %d", got, jsonReportSchema)
		}

		// Every published measurement is a number a consumer will do
		// arithmetic on. A NaN or an infinity here is not a cosmetic slip:
		// encoding/json writes both as bare tokens that are not valid JSON
		// for a strict decoder, and a rate derived from one is meaningless.
		for _, key := range []string{"out_tok_per_s", "in_tok_per_s", "uptime_secs"} {
			if v, ok := doc[key].(float64); ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
				t.Fatalf("report %s = %v, which a consumer cannot use", key, v)
			}
		}
		assertReportNumbers(t, doc["engines"], "engines")
		assertReportNumbers(t, doc["agents"], "agents")
		assertReportNumbers(t, doc["agent_rates"], "agent_rates")
		assertReportNumbers(t, doc["probes"], "probes")
		assertReportNumbers(t, doc["system"], "system")

		// Every free-form string the report publishes went through
		// SanitizeText on the way out. One that still carries an escape, a
		// control character or ill-formed UTF-8 is the terminal-equivalent
		// of an injection: the report is read by the operator's terminal and
		// piped into scripts that echo it back.
		assertStringsSanitized(t, out)

		// The two agent chart histograms, which bucket the same events onto a
		// cadence grid. A cell that is negative, infinite or NaN paints a
		// nonsensical column on the live frame, and both are reachable from a
		// sender's own token counts and timestamps.
		for _, outSide := range [2]bool{true, false} {
			for _, cadence := range []time.Duration{time.Second, 250 * time.Millisecond} {
				grid := agentDenseHist(snap.Agents, outSide, snap.At, core.HistoryLen, cadence)
				if len(grid) != core.HistoryLen {
					t.Fatalf("out=%v cadence=%v: grid has %d columns, want %d",
						outSide, cadence, len(grid), core.HistoryLen)
				}
				for i, v := range grid {
					if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
						t.Fatalf("out=%v cadence=%v: column %d = %v", outSide, cadence, i, v)
					}
				}
			}
		}
	})
}

// assertReportSafe fails unless a rendered report is something a terminal can
// be shown and a file can hold: valid UTF-8, and no escape byte anywhere in
// it. The renderers sanitize each field before writing it, so a stray ESC
// means one field reached the output without passing through them.
func assertReportSafe(t *testing.T, which, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("%s report is not valid UTF-8 (%d bytes)", which, len(out))
	}
	if i := strings.IndexByte(out, 0x1b); i >= 0 {
		t.Fatalf("%s report carries an escape byte at offset %d", which, i)
	}
}

// assertReportNumbers walks a decoded part of the report and fails on any
// number that is not finite. A nested object or list is descended into, so the
// gpus, models and temps lists under system and engines are covered by the
// same two calls the top-level numbers get.
func assertReportNumbers(t *testing.T, v any, where string) {
	t.Helper()
	switch n := v.(type) {
	case map[string]any:
		for key, val := range n {
			assertReportNumbers(t, val, where+"."+key)
		}
	case []any:
		for i, val := range n {
			assertReportNumbers(t, val, where+"[]")
			_ = i
		}
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			t.Fatalf("%s = %v, which a consumer cannot use", where, n)
		}
	}
}

// assertStringsSanitized re-reads the report document and fails if any string
// it publishes is not a fixed point of core.SanitizeText, or is not valid
// UTF-8. It walks the decoded document rather than the raw bytes so it names
// the field, and so a value that survives as a number-shaped string is not
// mistaken for a number.
func assertStringsSanitized(t *testing.T, out string) {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("report does not decode: %v", err)
	}
	walkReport(t, doc, "report")
}

func walkReport(t *testing.T, v any, where string) {
	t.Helper()
	switch n := v.(type) {
	case map[string]any:
		for key, val := range n {
			walkReport(t, val, where+"."+key)
		}
	case []any:
		for _, val := range n {
			walkReport(t, val, where+"[]")
		}
	case string:
		if !utf8.ValidString(n) {
			t.Fatalf("%s published invalid UTF-8", where)
		}
		if got := core.SanitizeText(n); got != n {
			t.Fatalf("%s published text a reader's terminal would run: %q", where, n)
		}
	}
}
