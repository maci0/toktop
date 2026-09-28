// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

func TestTruncateClusters(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"zero cap", "abc", 0, ""},
		{"negative cap", "abc", -1, ""},
		{"within cap", "hello", 10, "hello"},
		{"exact cap", "hello", 5, "hello"},
		{"ascii cut", "hello", 3, "hel"},
		// A flag is two regional indicators forming one character; cutting
		// between them would render half a flag.
		{"flags kept whole", "\U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7", 2, "\U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7"},
		{"cut between flags, never inside one", "\U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7", 1, "\U0001F1E9\U0001F1EA"},
		// An emoji ZWJ sequence is several code points joined by U+200D.
		{"zwj sequence kept whole", "👩‍💻👩‍💻👩‍💻", 2, "👩‍💻👩‍💻"},
		// Combining marks stay attached to their base letter.
		{"combining mark stays attached", "cafe\u0301", 4, "cafe\u0301"},
		{"combining mark not split off", "a\u0301bc", 1, "a\u0301"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateClusters(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("TruncateClusters(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if tc.n > 0 {
				if clusters := uniseg.GraphemeClusterCount(got); clusters > tc.n {
					t.Errorf("result %q holds %d clusters, cap is %d", got, clusters, tc.n)
				}
			}
			if !utf8.ValidString(got) {
				t.Errorf("result %q is not valid UTF-8", got)
			}
		})
	}
}

// Truncation must never manufacture a partial sequence: whatever comes back
// must be exactly a whole-cluster prefix of the input, pinning the guarantee
// against a future segmentation or implementation change.
func TestTruncateClustersIsWholeClusterPrefix(t *testing.T) {
	hostile := "\U0001F1E9\U0001F1EA" + strings.Repeat("e\u0301", 8) + "👩‍💻" + strings.Repeat("x", 16)
	total := uniseg.GraphemeClusterCount(hostile)
	var clusters []string
	state := -1
	for s := hostile; s != ""; {
		var c string
		c, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
		clusters = append(clusters, c)
	}
	for n := 0; n <= total; n++ {
		want := strings.Join(clusters[:n], "")
		if got := TruncateClusters(hostile, n); got != want {
			t.Fatalf("n=%d: TruncateClusters = %q, want the first %d whole clusters %q", n, got, n, want)
		}
	}
}

// TailClusters is TruncateClusters from the other end, and the guarantee is
// the same one: whatever comes back is a whole-cluster suffix, never a byte
// slice that lands inside a rune or between a base letter and its mark.
func TestTailClustersIsWholeClusterSuffix(t *testing.T) {
	hostile := "\U0001F1E9\U0001F1EA" + strings.Repeat("e\u0301", 8) + "👩‍💻" + strings.Repeat("x", 16)
	total := uniseg.GraphemeClusterCount(hostile)
	var clusters []string
	state := -1
	for s := hostile; s != ""; {
		var c string
		c, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
		clusters = append(clusters, c)
	}
	for n := 0; n <= total; n++ {
		want := strings.Join(clusters[total-n:], "")
		if got := TailClusters(hostile, n); got != want {
			t.Fatalf("n=%d: TailClusters = %q, want the last %d whole clusters %q", n, got, n, want)
		}
	}
}

func TestClampFieldComposesToNFCAndCapsClusters(t *testing.T) {
	if got := ClampField("cafe\u0301", 64); got != "caf\u00e9" {
		t.Errorf("ClampField(NFD café) = %q, want NFC", got)
	}
	if got := ClampField("caf\u00e9", 64); got != "caf\u00e9" {
		t.Errorf("ClampField(NFC café) = %q, want unchanged", got)
	}
	flags := strings.Repeat("\U0001F1E9\U0001F1EA", 80)
	got := ClampField(flags, 64)
	if n := uniseg.GraphemeClusterCount(got); n != 64 {
		t.Errorf("ClampField(80 flags, 64) kept %d clusters", n)
	}
	if got != flags[:len(got)] || !utf8.ValidString(got) {
		t.Errorf("ClampField split a flag or left invalid UTF-8: %q", got)
	}
	if !norm.NFC.IsNormalString(got) {
		t.Errorf("ClampField result is not NFC: %q", got)
	}
	if ClampField("abc", 0) != "" || ClampField("abc", -1) != "" {
		t.Error("ClampField with n <= 0 must be empty")
	}
}

// An engine-supplied model id reaches the dashboard, the probe body and the
// --json report, so ModelName is the single place its size and character set
// are settled.
func TestModelNameBoundsEngineSuppliedID(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "llama-3.1-8b-instruct.Q4_K_M", "llama-3.1-8b-instruct.Q4_K_M"},
		{"surrounding space", "  qwen2:7b  ", "qwen2:7b"},
		{"sgr recolor", "\x1b[31mllama3\x1b[0m", "llama3"},
		{"osc52 clipboard", "\x1b]52;c;YU9UQw==\x07llama3", "llama3"},
		{"control characters", "ll\x00a\x07m3", "llam3"},
		{"empty", "", ""},
		{"only escapes", "\x1b[31m\x1b[0m", ""},
	}
	for _, c := range cases {
		if got := ModelName(c.in); got != c.want {
			t.Errorf("ModelName(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestModelNameCapsLongID(t *testing.T) {
	got := ModelName("m" + strings.Repeat("x", ModelNameMax*4))
	if n := uniseg.GraphemeClusterCount(got); n != ModelNameMax {
		t.Errorf("ModelName kept %d clusters, want %d", n, ModelNameMax)
	}
	// A flag cut in half renders as a broken glyph; the cap must land between
	// clusters.
	flags := ModelName(strings.Repeat("\U0001F1E9\U0001F1EA", 400))
	if n := uniseg.GraphemeClusterCount(flags); n != ModelNameMax || !utf8.ValidString(flags) {
		t.Errorf("ModelName(400 flags) = %d clusters, valid=%v", n, utf8.ValidString(flags))
	}
}

// A GPU's reported name goes through the same cell as a model id, and the two
// of its four sources are JSON. An escaped newline survives SanitizeText on
// purpose, so SingleLine in GPUName is the only thing keeping a driver-supplied
// name from breaking the system panel's row alignment.
func TestGPUNameCollapsesToOneLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "NVIDIA GeForce RTX 4090", "NVIDIA GeForce RTX 4090"},
		{"escaped newline", "A100\nspoofed", "A100 spoofed"},
		{"surrounding space", "  Apple M3 Max  ", "Apple M3 Max"},
		{"osc52 clipboard", "\x1b]52;c;YU9UQw==\x07Apple M3", "Apple M3"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		got := GPUName(c.in)
		if got != c.want {
			t.Errorf("GPUName(%s) = %q, want %q", c.name, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("GPUName(%s) = %q kept a line break", c.name, got)
		}
	}
}

func TestGPUNameCapsLongName(t *testing.T) {
	got := GPUName("g" + strings.Repeat("x", ModelNameMax*4))
	if n := uniseg.GraphemeClusterCount(got); n != ModelNameMax {
		t.Errorf("GPUName kept %d clusters, want %d", n, ModelNameMax)
	}
}
