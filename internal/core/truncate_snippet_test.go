package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func TestSnippetBoundedAndOneLine(t *testing.T) {
	got := Snippet([]byte(strings.Repeat("boom ", 400) + "\r\n\ttail"))
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("snippet kept line breaks: %q", got)
	}
	if n := uniseg.GraphemeClusterCount(got); n > SnippetCap {
		t.Errorf("snippet = %d characters, cap is %d", n, SnippetCap)
	}
	if got := Snippet(nil); got != "" {
		t.Errorf("empty body snippet = %q, want empty", got)
	}
}

// A body cut mid-character prints garbage in the error line (half a flag
// emoji, a dangling zero-width joiner); the cap must land between characters.
func TestSnippetKeepsCharactersWhole(t *testing.T) {
	body := []byte(strings.Repeat("模型", 200) + " \U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7")
	got := Snippet(body)
	if n := uniseg.GraphemeClusterCount(got); n > SnippetCap {
		t.Errorf("snippet = %d characters, cap is %d", n, SnippetCap)
	}
	if strings.HasSuffix(got, "\u200d") {
		t.Errorf("snippet ends with a dangling zero-width joiner: %q", got)
	}
	// Whatever survived must be a whole-cluster prefix of the collapsed body.
	collapsed := strings.Join(strings.Fields(string(body)), " ")
	if !strings.HasPrefix(collapsed, got) {
		t.Errorf("snippet %q is not a prefix of the body", got)
	}
	indicators := 0
	for _, r := range got {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			indicators++
		}
	}
	if indicators%2 != 0 {
		t.Errorf("snippet holds a lone regional indicator (half a flag): %q", got)
	}
}

func TestSnippetStripsTerminalInjection(t *testing.T) {
	got := Snippet([]byte("ok\x1b]52;c;QUJD\x07tail"))
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Errorf("snippet retained escape bytes: %q", got)
	}
	if !strings.Contains(got, "ok") || !strings.Contains(got, "tail") {
		t.Errorf("snippet lost visible text: %q", got)
	}
}

func TestSnippetDropsInvalidUTF8(t *testing.T) {
	got := Snippet([]byte("caf\xff\xfe"))
	if !utf8.ValidString(got) {
		t.Errorf("snippet is not valid UTF-8: %q", got)
	}
	if got != "caf" {
		t.Errorf("snippet = %q, want caf with the ill-formed bytes dropped", got)
	}
}
