package probe

import (
	"encoding/json"
	"testing"
)

// TestOpenAIFrameShapes pins the frame parser to the lines an OpenAI-compatible
// stream actually emits, and to the one it must reject: a data field carrying no
// payload is not a frame, because handing the caller an empty payload fails the
// whole probe over a line that is not malformed.
func TestOpenAIFrameShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{"sse data", "data: {\"a\":1}", `{"a":1}`, true},
		{"sse data no space", "data:{\"a\":1}", `{"a":1}`, true},
		{"sse data padded after colon", "data:   {\"a\":1}", `{"a":1}`, true},
		{"bare object", `{"a":1}`, `{"a":1}`, true},
		{"done", "data: [DONE]", "[DONE]", true},
		{"empty data", "data:", "", false},
		{"whitespace-only data", "data:   ", "", false},
		{"comment", ": keep-alive", "", false},
		{"event line", "event: message", "", false},
		{"empty line", "", "", false},
		{"text line", "hello", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := openaiFrame(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
			if ok && !json.Valid([]byte(got)) && got != "[DONE]" {
				t.Errorf("payload %q is not decodable JSON", got)
			}
		})
	}
}
