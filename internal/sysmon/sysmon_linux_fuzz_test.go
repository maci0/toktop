//go:build linux

package sysmon

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseCPUModel throws arbitrary bytes at the /proc/cpuinfo brand-string
// reader. Its result is the CPU model the system panel renders, so it has to
// come back as one trimmed, measurable line: firmware writes the file, in
// whatever encoding its build used, and the panel measures the result as text.
// The key spellings matter as much as the values, since the fold that reads
// them is the /proc fold and a rune whose lowercase is ASCII must not reach a
// branch the kernel never wrote that key for.
func FuzzParseCPUModel(f *testing.F) {
	for _, seed := range []string{
		"model name\t: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz\n",
		"Hardware\t: BCM2835\nprocessor\t: 0\n",
		"processor\t: ARMv7 Processor rev 4 (v7l)\n",
		"cpu model\t: Loongson-3A5000\nmodel name\t: MIPS Loongson-3A5000\n",
		"model name:\nhardware:   \nprocessor: 0\n",
		"model name\t:\tApple M1\n",
		"model \u212Aname\t: Spoofed\n",
		"MODEL NAME : Folded\nmodel name\t: Not Folded\n",
		"model name\t: Intel\xe9 Core i7\n",
		"model name\t: \xff\xfe\x00raw\n",
		"model name\t: a: b: c\n",
		"processor\t: 9223372036854775808\n",
		"processor\t: -1\n",
		"processor\t: +7\n",
		"\n\n: :\n:::\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := parseCPUModel(b)
		if !utf8.ValidString(got) {
			t.Fatalf("parseCPUModel(%q) = %q, want valid UTF-8", b, got)
		}
		if strings.ContainsAny(core.SanitizeText(got), "\n\r") {
			t.Fatalf("parseCPUModel(%q) = %q, and the panel renders it spanning lines", b, got)
		}
		if got != strings.TrimSpace(got) {
			t.Fatalf("parseCPUModel(%q) = %q, want it trimmed", b, got)
		}
		if again := parseCPUModel(b); again != got {
			t.Fatalf("parseCPUModel is not deterministic: %q then %q", got, again)
		}
		// A brand the parser accepted has to survive being read back as the
		// brand key, or the string it hands the panel depends on which key the
		// firmware happened to spell first.
		if got != "" {
			if back := parseCPUModel([]byte("model name : " + got + "\n")); back != got {
				t.Fatalf("parseCPUModel dropped a brand it accepted: %q -> %q", got, back)
			}
		}

		// A key that differs from the one the kernel writes only in a rune
		// whose lowercase is ASCII reads as that key under strings.ToLower, and
		// the fold must not match it. Derived from the same input so the
		// misspelling rides whatever bytes the fuzzer found.
		if !strings.ContainsAny(string(b), "\n:") {
			if spoofed := parseCPUModel([]byte("model \u212Aname : " + string(b) + "\n")); spoofed != "" {
				t.Fatalf("a Kelvin-signed key read as the brand: %q", spoofed)
			}
		}
		// "processor : 0" is a core index, not a brand, and the ARM boards that
		// omit "model name" are the reason the key is read at all.
		if _, err := strconv.Atoi(string(b)); err == nil {
			if idx := parseCPUModel([]byte("processor\t: " + string(b) + "\n")); idx != "" {
				t.Fatalf("core index %q read as a brand: %q", b, idx)
			}
		}
	})
}

// FuzzParseNvidiaVersion throws arbitrary text at the /proc/driver/nvidia/
// version reader, whose two fields are the driver's own identity row. Both
// lines are read independently, so a CUDA row ahead of the driver row must
// still land, and the last of either wins so a file carrying a second reading
// reports the newer one. Neither field may come back as anything but the
// whitespace-delimited token the file spelled.
func FuzzParseNvidiaVersion(f *testing.F) {
	for _, seed := range []string{
		"NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.14  Tue Mar 19\nDriver Version: 550.54.14\nCUDA Version: 12.4\n",
		"CUDA Version: 12.4\nDriver Version: 535.104.05\n",
		"Driver Version:\nCUDA Version:   \n",
		"Driver Version: 550.54.14 extra\n",
		"Driver Version: 550.54.14\nDriver Version: 535.104.05\n",
		"driver version: 550.54.14\n",
		"Driver Version: \u00a0550.54.14\n",
		"no version data here\n",
		"",
		"\x00\x00",
	} {
		f.Add([]byte(seed))
	}
	// The bytes are the file's, and the driver wrote it, so they cross
	// kernelText on the way in exactly as readHostStatic hands them over.
	f.Fuzz(func(t *testing.T, b []byte) {
		text := kernelText(b)
		drv, cuda := parseNvidiaVersion(text)
		for name, v := range map[string]string{"driver": drv, "cuda": cuda} {
			if v == "" {
				continue
			}
			if !utf8.ValidString(v) {
				t.Fatalf("%s = %q from %q, want valid UTF-8", name, v, text)
			}
			if fields := strings.Fields(v); len(fields) != 1 || fields[0] != v {
				t.Fatalf("%s = %q from %q, want one whitespace-delimited token", name, v, text)
			}
		}
		if again, againCUDA := parseNvidiaVersion(text); again != drv || againCUDA != cuda {
			t.Fatalf("parseNvidiaVersion is not deterministic: %q/%q then %q/%q", drv, cuda, again, againCUDA)
		}

		// A row spelled with the fuzzer's own token must come back as that
		// token, whichever field it names, and the CUDA row must not depend on
		// a driver row having been read first. A multi-line input is skipped:
		// each of its rows is a reading of its own and the last wins, which
		// says nothing about the row under test.
		if strings.Contains(text, "\n") {
			return
		}
		for _, prefix := range []string{"Driver Version: ", "CUDA Version: "} {
			row := prefix + text + "\n"
			rowDrv, rowCUDA := parseNvidiaVersion(row)
			want := strings.Fields(text)
			got := rowDrv
			if prefix == "CUDA Version: " {
				got = rowCUDA
			}
			if len(want) == 0 {
				if got != "" {
					t.Fatalf("row %q produced %q from an empty tail", row, got)
				}
				continue
			}
			if got != want[0] {
				t.Fatalf("row %q produced %q, want %q", row, got, want[0])
			}
		}
	})
}

// FuzzParseOSRelease throws arbitrary bytes at the /etc/os-release reader. The
// value is unquoted with strconv.Unquote, so a distribution that quotes its
// name and one that does not both answer, and a quote the build never closed
// falls back to the raw text rather than to nothing.
func FuzzParseOSRelease(f *testing.F) {
	for _, seed := range []string{
		"PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\n",
		"PRETTY_NAME=Alpine Linux v3.20\n",
		"PRETTY_NAME=\"\"\"\n",
		"PRETTY_NAME=\"unterminated\n",
		"PRETTY_NAME=\"tab\\there\"\n",
		"PRETTY_NAME=\n",
		"PRETTY_NAME = spaced\n",
		"PRETTY_NAME=First\nPRETTY_NAME=Second\n",
		"PRETTY_NAME=\"caf\\u00e9\"\n",
		"PRETTY_NAME=\"caf\xe9\"\n",
		"NAME=other\nID=debian\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := parseOSRelease(b)
		if !utf8.ValidString(got) {
			t.Fatalf("parseOSRelease(%q) = %q, want valid UTF-8", b, got)
		}
		if again := parseOSRelease(b); again != got {
			t.Fatalf("parseOSRelease is not deterministic: %q then %q", got, again)
		}
		// A quoted value the reader unquotes must read back the same, so a name
		// cannot change with the build's escaping. The token is the file's text
		// as the reader sees it, after kernelText. Escape-bearing tokens are
		// skipped: unquoting turns `\t` into a tab, and re-quoting that tab is
		// not the token under test.
		tok := kernelText(b)
		if tok == "" || tok != strings.TrimSpace(tok) || strings.ContainsAny(tok, "\"\\\n\r") {
			return
		}
		if quoted := parseOSRelease([]byte("PRETTY_NAME=\"" + tok + "\"\n")); quoted != tok {
			t.Fatalf("quoted PRETTY_NAME=%q read as %q", tok, quoted)
		}
	})
}
