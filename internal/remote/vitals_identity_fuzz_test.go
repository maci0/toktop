package remote

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// FuzzVitalsIdentityFields covers what FuzzParseVitals cannot reach through its
// numeric asserts: the three host-identity fields of parseVitals and the
// section splitter that feeds them. Those values are whatever a peer's
// /proc/cpuinfo, /etc/os-release and uname printed, on a host the operator
// chose to watch, and every renderer and the --json report draw them, so the
// guarantees vitalsField exists to provide have to hold under a dump crafted
// to break them: one line (a peer must not forge a row the host strip did not
// write), valid UTF-8, nothing a terminal acts on, and at most
// core.ModelNameMax grapheme clusters, cut whole so a cut accented letter or
// emoji never renders as a replacement character.
//
// The section half pins what splitSections promises the fields that follow it:
// reading by name rather than by position, keeping the later block when a name
// appears twice, dropping text before the first marker and any unnamed block,
// and answering the same dump the same way twice. Both halves are asserted
// against expectations recomputed from the dump's own text rather than against
// the helpers that produce them, so a helper that drifted answers the
// comparison with the same wrong string and still fails: a splitter that mixed
// two sections, or a firstLine that stopped scanning per line, would leave
// every field one line, valid, inside the cap and empty of escapes, so only
// the field-by-field check against the raw text catches it.
func FuzzVitalsIdentityFields(f *testing.F) {
	for _, seed := range []string{
		vitalsDump,
		vitalsDumpFrom("1.0 1.0 1.0", "", "183729.42", "AMD Ryzen 9 7950X", `"Debian GNU/Linux 12"`, "6.1.0-18-amd64", ""),
		// Two blocks under one name: the later one wins.
		vitalsDumpFrom("1 1 1", "", "1", "first cpu", `"first"`, "1.0", "") +
			"%toktop%cpu\nsecond cpu model\n%toktop%os\n\"second\"\n%toktop%kernel\n2.0\n",
		// Text before the first marker, and an unnamed marker between named
		// ones: both are dropped, so the fields after them keep their own.
		"stray preamble\n%toktop%cpu\n" + strings.Repeat("p", 5000) + "\n%toktop%\nunattributed\n%toktop%kernel\n6.1\n",
		// A marker the script never writes, and a data line that merely starts
		// with the mark: the latter is data, so it is the cpu model's line.
		"%toktop%nosuch\nvalue\n%toktop%cpu\n%toktop%not a marker name\n",
		// The identity fields carrying what a peer would send to make them
		// forge output or blow the field up.
		vitalsDumpFrom("", "", "", "cpu\x1b]0;pwned\x07\x1b[2J\nsecond cpu line", "", "", ""),
		vitalsDumpFrom("", "", "", strings.Repeat("é", 5000), "", "", ""),
		vitalsDumpFrom("", "", "", strings.Repeat("🚀", 5000), "", "", ""),
		vitalsDumpFrom("", "", "", "Intel\xAECore i9", "", "", ""),
		vitalsDumpFrom("", "", "", "clаude"+strings.Repeat("", 400), "", "", ""),
		// PRETTY_NAME as /etc/os-release spells it, and the values that are
		// not Go string literals: trimQuotes strips one layer from the first
		// and leaves the rest verbatim.
		vitalsDumpFrom("", "", "", "", `"quoted os"`, "", ""),
		vitalsDumpFrom("", "", "", "", `"""`, "", ""),
		vitalsDumpFrom("", "", "", "", `"a\"b"`, "", ""),
		vitalsDumpFrom("", "", "", "", "plain os name", "", ""),
		vitalsDumpFrom("", "", "", "", "\"unterminated", "", ""),
		vitalsDumpFrom("", "", "", "", "\"\xff\xfe\"", "", ""),
		// The kernel string with a line break and a bidi override in it.
		vitalsIdentitySeed("kernel", "6.1.0\u202egnp.exe\r\nsecond"),
		vitalsIdentitySeed("kernel", "6.1.0\ttab"),
		vitalsIdentitySeed("os", `"\ud800"`),
		vitalsIdentitySeed("os", `"\ud800\ud800"`),
		vitalsIdentitySeed("cpu", "\x00\x01\x1f"),
		// A dump cut short before the last marker.
		vitalsDumpFrom("1 1 1", "MemTotal: 100 kB", "10", "c", `"o"`, "1.0"),
		"",
		"%toktop%",
		"%toktop%cpu",
		"%toktop%cpu\n",
		"\n\n\n",
	} {
		f.Add([]byte(seed))
	}

	const identityCap = core.ModelNameMax
	f.Fuzz(func(t *testing.T, data []byte) {
		dump := string(data)

		var s core.SysSample
		parseVitals(dump, &s)
		for name, v := range map[string]string{
			"CPUModel": s.CPUModel, "OsName": s.OsName, "Kernel": s.Kernel,
		} {
			assertVitalsIdentity(t, dump, name, v, identityCap)
		}

		// A second parse of the same dump is the same reading: the splitters
		// and the field fold carry no state between calls, so a poll that
		// re-reads a dump the UI kept cannot report a host identity that
		// changes without the peer changing anything.
		var again core.SysSample
		parseVitals(dump, &again)
		if again.CPUModel != s.CPUModel || again.OsName != s.OsName || again.Kernel != s.Kernel {
			t.Fatalf("identity fields are not deterministic over %q: %q/%q/%q then %q/%q/%q",
				dump, s.CPUModel, s.OsName, s.Kernel, again.CPUModel, again.OsName, again.Kernel)
		}

		// The split itself, asserted rather than inferred from the fields:
		// every key is a mark followed by one name, and every block a later
		// mark does not open is a key's own text.
		sections := splitSections(dump)
		for name, body := range sections {
			if name == "" {
				t.Fatalf("splitSections kept an unnamed block from %q", dump)
			}
			if strings.ContainsAny(name, " \t") {
				t.Fatalf("splitSections key %q is not a single name, from %q", name, dump)
			}
			for line := range strings.SplitSeq(body, "\n") {
				trimmed := strings.TrimSpace(line)
				if mark, ok := strings.CutPrefix(trimmed, sectionMark); ok &&
					mark != "" && !strings.ContainsAny(mark, " \t") {
					t.Fatalf("block %q still holds the marker line %q, from %q", name, trimmed, dump)
				}
			}
		}

		// A field read from one section carries that section's first
		// non-blank line and nothing else, which is what firstLine and the
		// splitter promise between them. The expectation is recomputed here
		// rather than by calling firstLine again, so a firstLine that stopped
		// scanning per line and handed back the whole block would answer this
		// with the same wrong string and pass: the leading blank lines and the
		// trailing data lines of a multi-line section are the whole difference,
		// and asserting against the helper under test cannot see them.
		for _, c := range []struct {
			section string
			got     string
			unquote bool
		}{
			{secCPU, s.CPUModel, false},
			{secOS, s.OsName, true},
			{secKernel, s.Kernel, false},
		} {
			body, present := sections[c.section]
			want := ""
			if present {
				for _, line := range strings.Split(body, "\n") {
					if t := strings.TrimSpace(line); t != "" {
						want = t
						break
					}
				}
				if c.unquote {
					want = unquoteOnce(want)
				}
				if want != "" {
					want = vitalsField(want)
				}
			}
			if c.got != want {
				t.Fatalf("%s section: field = %q, want the first non-blank line %q, from %q",
					c.section, c.got, want, dump)
			}
		}

		// A name written twice keeps the later block: the script writes each
		// section once, so a repeat is a peer sending two, and the later is
		// the one its last poll printed. This is asserted over the dump's own
		// text rather than over splitSections' result, because a splitter that
		// kept the first block instead would hand the same wrong body to the
		// expectation above and pass it.
		assertLaterBlockWins(t, dump, sections)
	})
}

// assertLaterBlockWins scans a dump for the last mark line opening each name
// and checks the split kept exactly that block. The scan is written against
// the raw text, so it fails when splitSections keeps a different one.
func assertLaterBlockWins(t *testing.T, dump string, sections map[string]string) {
	t.Helper()
	last := map[string]string{}
	var name string
	var body strings.Builder
	flush := func() {
		if name != "" {
			last[name] = body.String() // a later block replaces an earlier one
		}
		body.Reset()
	}
	for line := range strings.SplitSeq(dump, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if mark, ok := strings.CutPrefix(strings.TrimSpace(trimmed), sectionMark); ok &&
			mark != "" && !strings.ContainsAny(mark, " \t") {
			flush()
			name = mark
			continue
		}
		body.WriteString(trimmed)
		body.WriteByte('\n')
	}
	flush()
	for name, want := range last {
		got, present := sections[name]
		if !present {
			t.Fatalf("section %q is missing from a dump that names it: %q", name, dump)
		}
		if got != want {
			t.Fatalf("section %q = %q, want the last block %q, from %q", name, got, want, dump)
		}
	}
}

// unquoteOnce is the quote rule parseVitals applies to the os section, spelled
// out here so the expectation above does not come from the helper it checks.
func unquoteOnce(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// assertVitalsIdentity fails unless v is the shape vitalsField promises for a
// peer-supplied host-identity string.
func assertVitalsIdentity(t *testing.T, dump, name, v string, cap int) {
	t.Helper()
	if !utf8.ValidString(v) {
		t.Fatalf("%s = %q is not valid UTF-8, from %q", name, v, dump)
	}
	if strings.ContainsAny(v, "\n\r\v\f") {
		t.Fatalf("%s = %q kept a line break, from %q", name, v, dump)
	}
	if n := uniseg.GraphemeClusterCount(v); n > cap {
		t.Fatalf("%s = %d clusters, bound is %d, from %q", name, n, cap, dump)
	}
	// Whatever survives must be what a second pass over the field itself
	// yields, so a renderer that re-folds it cannot change it: the fold is
	// the only thing standing between a peer's escape sequence and a
	// terminal, and a field that refolds to something else has not been
	// folded at all.
	if again := core.SanitizeText(v); again != v {
		t.Fatalf("%s = %q still carries something a terminal acts on: %q, from %q", name, v, again, dump)
	}
}

// vitalsIdentitySeed builds a full dump carrying one identity section, so a
// seed can name the field it is exercising without spelling the other six out.
func vitalsIdentitySeed(section, value string) string {
	return vitalsDumpFrom("", "", "", "", "", "", "") + sectionMark + section + "\n" + value + "\n"
}
