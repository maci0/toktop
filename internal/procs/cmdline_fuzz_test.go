package procs

import (
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"
)

// FuzzClipUTF8Prefix drives the byte-clipping helper over arbitrary bytes. A
// /proc/PID/cmdline buffer is whatever the process chose to exec, so it can
// hold NULs, lone continuation bytes and truncated sequences; the clip
// exists to keep a cut from leaving a dangling lead byte, which is only
// meaningful for a well-formed line and must never make a well-formed one
// worse. What has to hold: the result is a prefix, within the byte bound,
// and valid UTF-8 whenever the input was.
func FuzzClipUTF8Prefix(f *testing.F) {
	for _, seed := range []string{
		"",
		"vllm",
		"é",
		"éé",
		"日本語のモジュールパス",
		strings.Repeat("x", 4095) + "é",
		strings.Repeat("x", 4096) + "é",
		strings.Repeat("x", 100_000),
		"\xff\xfe\x00",
		"\xc3",
		"\xed\xa0\x80",
		"\xf0\x9f\x98\x80",
		"llama-server\x00--port\x008080",
		"🚀",
		strings.Repeat("🚀", 2000),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, n := range []int{0, 1, 2, 3, 4, 4095, 4096, 8192, len(s)} {
			got := clipUTF8Prefix(s, n)
			if len(got) > n {
				t.Fatalf("clipUTF8Prefix(%q, %d) returned %d bytes, past the bound", s, n, len(got))
			}
			if !strings.HasPrefix(s, got) {
				t.Fatalf("clipUTF8Prefix(%q, %d) = %q, not a prefix of the input", s, n, got)
			}
			if utf8.ValidString(s) && !utf8.ValidString(got) {
				t.Fatalf("clipUTF8Prefix split a character of a well-formed line: %q of %q at %d", got, s, n)
			}
			// The clip keeps as much of the bound as it can: a character that
			// straddles the bound is dropped whole, but a shorter result than
			// that is bytes given away for nothing.
			if utf8.ValidString(s) {
				for j := len(got) + 1; j <= n && j < len(s); j++ {
					if utf8.ValidString(s[:j]) {
						t.Fatalf("clipUTF8Prefix(%q, %d) = %q, but %d bytes also fit", s, n, got, j)
					}
				}
			}
			// Clipping twice is the same as clipping once: the result is already
			// inside the bound, so a second pass has nothing left to do.
			if again := clipUTF8Prefix(got, n); again != got {
				t.Fatalf("clipUTF8Prefix is not idempotent on %q at %d: %q then %q", s, n, got, again)
			}
		}
	})
}

// FuzzClipArgs drives the retained command-line builder over hostile argv. A
// local listing splits the raw /proc/PID/cmdline buffer on its NUL
// separators, so the input is the buffer itself: a browser's flag blob, an
// inline prompt, invalid bytes, a 200 KB argv. What must hold: the joined
// length stays inside CmdlinePrefix, every kept element is a prefix of the
// argument it came from, the result is valid UTF-8 whenever the input was,
// and no element aliases the caller's slice.
func FuzzClipArgs(f *testing.F) {
	for _, seed := range []string{
		"",
		"\x00",
		"ollama\x00serve\x00--port\x0011434",
		"vllm\x00serve\x00--model\x00/models/meta-llama/Llama-3-8B",
		"python\x00-m\x00vllm.entrypoints.openai.api_server",
		"chromium\x00--disable-features=" + strings.Repeat("A", 40_000),
		"sh\x00-c\x00" + strings.Repeat("'", 9000),
		"\xff\xfe\x00--port\x0099999",
		"日本語\x00" + strings.Repeat("é", 3000),
		strings.Repeat("x", 200_000),
		"\x00\x00\x00",
		"tritonserver\x00--model-repository=/models",
		"ollama\x00serve\x00\x00--port\x0011434",
		"vllm\x00--port=8000\x00" + strings.Repeat("🚀", 2000),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cmdline string) {
		args := strings.Split(cmdline, "\x00")
		got := ClipArgs(args)
		if len(got) > len(args) {
			t.Fatalf("ClipArgs(%d args) returned %d, more than it was given", len(args), len(got))
		}
		total := 0
		for i, a := range got {
			if i > 0 {
				total++ // the separator a join writes between two arguments
			}
			total += len(a)
			if !strings.HasPrefix(args[i], a) {
				t.Fatalf("ClipArgs kept %q at index %d, which is not a prefix of %q", a, i, args[i])
			}
			if utf8.ValidString(args[i]) && !utf8.ValidString(a) {
				t.Fatalf("ClipArgs split a character: %q of %q", a, args[i])
			}
		}
		if total > CmdlinePrefix {
			t.Fatalf("ClipArgs retained %d bytes, bound is %d", total, CmdlinePrefix)
		}
		// Elements are cloned: a listing builds them as windows onto one
		// whole cmdline buffer, so a shared element pins that whole line, and
		// the prompt or path past the bound with it, for the sampler's life.
		for i := range got {
			if got[i] == "" {
				continue
			}
			if unsafe.StringData(got[i]) == unsafe.StringData(args[i]) {
				t.Fatalf("ClipArgs kept argument %d aliasing the caller's slice", i)
			}
		}
		if again := ClipArgs(got); len(again) != len(got) {
			t.Fatalf("ClipArgs is not idempotent: %d args then %d", len(got), len(again))
		}
		// The matchers read the clipped line, so every one of them has to
		// agree with itself and name an engine from the table.
		eng, def, ok := MatchEngine(Info{PID: 1, Name: args0(args), Args: got, PortHint: ExtractPort(got)})
		if againEng, againDef, againOK := MatchEngine(Info{PID: 1, Name: args0(args), Args: got, PortHint: ExtractPort(got)}); againEng != eng || againDef != def || againOK != ok {
			t.Fatalf("MatchEngine is not deterministic over %q: %q/%d/%v then %q/%d/%v",
				got, eng, def, ok, againEng, againDef, againOK)
		}
		if !ok {
			if eng != "" || def != 0 {
				t.Fatalf("MatchEngine(%q) = %q/%d with ok=false, want the zero match", got, eng, def)
			}
			return
		}
		known := false
		for _, m := range engineMatchers {
			if m.engine == eng {
				known = true
				if def != m.defPort {
					t.Fatalf("MatchEngine(%q) = %q with default port %d, want %d", got, eng, def, m.defPort)
				}
			}
		}
		if !known {
			t.Fatalf("MatchEngine(%q) named the unknown engine %q", got, eng)
		}
		if p := ExtractPort(got); p != 0 && (p < 1 || p > 65535) {
			t.Fatalf("ExtractPort(%q) = %d, outside the TCP port range", got, p)
		}
	})
}

func args0(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
