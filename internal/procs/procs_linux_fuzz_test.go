//go:build linux

package procs

import (
	"os"
	"strings"
	"testing"
)

// statTail is a well-formed field 3..24 run, the part procStatCPUAndRSS
// actually reads: utime (14), stime (15) and rss pages (24).
const statTail = "R 1996325 1996325 1996325 0 -1 4194304 368 105 0 0 0 0 0 0 16 -4 1 0 7385257 6447104 579 42"

// FuzzProcStatCPUAndRSS drives the /proc/PID/stat field walker over arbitrary
// bytes. Every process on the box writes its own line there and a process
// names itself, so the comm parens are process-controlled input to this
// parser, not a kernel-owned frame. What must hold whatever arrives: no
// panic, RSS bytes saturate instead of wrapping when the page count is
// absurd, a repeated parse of the same line agrees with the first, and a
// comm containing spaces does not shift the field numbering.
func FuzzProcStatCPUAndRSS(f *testing.F) {
	for _, seed := range []string{
		"1996325 (head) " + statTail,
		"1 (a b c d e) " + statTail,
		"1 () " + statTail,
		"1 ()",
		"1 () ",
		"1 (",
		")",
		"(",
		"()",
		"1 (x)) " + statTail,
		"1 (x " + statTail,
		"1 (x)\t" + statTail,
		"1 (x)  " + statTail,
		"1 (x)\n" + statTail,
		"1996325 (head) R 1 2 3 4 5 6 7 8 9 10 11 18446744073709551615 18446744073709551615 18 19 18446744073709551615 21 22",
		"1996325 (head) R 1 2 3 4 5 6 7 8 9 10 11 0 0 18 19 0 21 22",
		"1 (x) R " + strings.Repeat("9 ", 4096),
		"1 (x) -1 -2 -3 4 5 6 7 8 9 10 11 -1 -2 -3 18 19 -3 21 22",
		"1 (x) 0x10 1e3 1.5 4 5 6 7 8 9 10 11 12 13 0x10 0x10 0x10 18 19 0x10 21 22",
		"\x00\x01\xff (x) R 1",
		"1 (x) R",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, stat string) {
		ticks, rss := procStatCPUAndRSS(stat)
		againTicks, againRSS := procStatCPUAndRSS(stat)
		if ticks != againTicks || rss != againRSS {
			t.Fatalf("parse is not deterministic: (%d,%d) then (%d,%d)",
				ticks, rss, againTicks, againRSS)
		}
		// RSS comes out of a saturating multiply, so a huge page count is
		// MaxUint64 rather than a small wrapped byte count. Anything below
		// MaxUint64 that is not a multiple of the page size came from a
		// wrap or from a partial read.
		ps := uint64(os.Getpagesize())
		if rss != ^uint64(0) && ps != 0 && rss%ps != 0 {
			t.Fatalf("rss %d is not a multiple of the %d byte page size: %q", rss, ps, stat)
		}
	})
}

// FuzzProcStatCommBoundary checks the property the field walker's comment
// promises: comm is delimited by its own parens and may contain spaces, so
// every comm spelling must yield the same ticks and RSS for a fixed tail.
// A comm holding a closing paren or a stray paren is the fuzzer's to try;
// the kernel does not stop a process from choosing one.
func FuzzProcStatCommBoundary(f *testing.F) {
	f.Add("head")
	f.Add("a b c")
	f.Add("")
	f.Add("x)y")
	f.Add("(x")
	f.Add(") x S 0 0 1")
	f.Add("very long comm")
	f.Add("\xff\xfe \x00")
	f.Add("  leading and trailing  ")
	f.Fuzz(func(t *testing.T, comm string) {
		if strings.ContainsAny(comm, "\n\r\x00") {
			return // not a shape a comm can take: the line ends at the newline
		}
		// Keep the line inside the kernel's 15-byte comm field so the case
		// stays reachable, then pad with filler to the field's fixed width.
		if len(comm) > 15 {
			comm = comm[:15]
		}
		padded := comm + strings.Repeat("z", 15-len(comm))
		body := "1 (" + padded + ") " + statTail
		gotTicks, gotRSS := procStatCPUAndRSS(body)
		wantTicks, wantRSS := procStatCPUAndRSS("1 (" + strings.Repeat("z", 15) + ") " + statTail)
		if gotTicks != wantTicks || gotRSS != wantRSS {
			t.Fatalf("comm %q shifted the fields: got (%d,%d) want (%d,%d)",
				comm, gotTicks, gotRSS, wantTicks, wantRSS)
		}
	})
}
