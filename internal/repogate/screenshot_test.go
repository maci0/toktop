// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// screenshotPython locates an interpreter with the renderer's runtime pins
// installed, or skips the check. It is the scratch venv `make scripts-env`
// builds when it can and whatever python3 is on PATH after that, so the gate
// runs in CI's scripts env and stays silent on a machine that never installed
// one.
func screenshotPython(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join(moduleRoot, "dist", "scripts-env", "bin", "python"),
		filepath.Join(moduleRoot, "dist", "scripts-env", "Scripts", "python.exe"),
	}
	if p, err := exec.LookPath("python3"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		// Each candidate is one of the paths built above, never user input.
		if err := exec.Command(c, "-c", "import pyte, wcwidth, PIL").Run(); err == nil {
			return c
		}
	}
	t.Skip("no interpreter with pyte, wcwidth and pillow installed; run make scripts-env first")
	return ""
}

// runScreenshot renders one capture through scripts/screenshot.py at scale 1
// and returns the "<out>: <w>x<h> from <cols>x<rows> cells" line it wrote.
func runScreenshot(t *testing.T, py, capture, out string) string {
	t.Helper()
	script := filepath.Join(moduleRoot, "scripts", "screenshot.py")
	// The script and the paths come from this test and t.TempDir.
	got, err := exec.Command(py, script, capture, out, "1").CombinedOutput() // #nosec G204
	if err != nil {
		t.Fatalf("screenshot.py %s: %v\n%s", capture, err, got)
	}
	return string(got)
}

// A capture is a grid of terminal cells, so the renderer has to measure a row
// in cells rather than in characters. It measured len() of the decoded line,
// which counts a CJK ideograph, a Hangul syllable and a fullwidth form as one
// character where the terminal gives them two. The auto-detected column count
// came out a third short, pyte laid the row out at that width, and every cell
// past the cut was missing from the written image: a dashboard whose model ids
// are Japanese lost the right-hand third of itself out of the screenshot.
//
// Each class the terminal doubles is asserted, so a fix that special-cases CJK
// and not Hangul or Fullwidth fails, and Latin and the escape sequences are
// asserted as the baselines they are measured against.
func TestScreenshotMeasuresRowsInDisplayColumns(t *testing.T) {
	py := screenshotPython(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		row  string
		cols int
	}{
		{"latin", "abcdefghijkl", 12},
		{"cjk", "日本語 model", 12},             // 3 ideographs at 2 cells, a space, 5 Latin
		{"hangul", "에이전트", 8},                // 4 Hangul syllables, East Asian Wide like CJK
		{"fullwidth", "ＡＢＣ", 6},              // Fullwidth Latin is the third Wide class
		{"emoji", "👩‍💻 ab", 5},               // 2 cells, the ZWJ between is none, then " ab"
		{"combining", "e\u0301 ab", 4},       // "e" plus a combining acute is one cell
		{"precomposed", "\u00e9 ab", 4},      // the same grapheme precomposed is also one
		{"escapes", "\x1b[31mabc\x1b[0m", 3}, // SGR runs are not cells
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(dir, tc.name+".txt")
			if err := os.WriteFile(capture, []byte(tc.row+"\n"), 0o600); err != nil {
				t.Fatalf("write capture: %v", err)
			}
			out := filepath.Join(dir, tc.name+".png")
			line := runScreenshot(t, py, capture, out)
			want := "from " + strconv.Itoa(tc.cols) + "x1 cells"
			if !strings.Contains(line, want) {
				t.Errorf("row %q: %s, want %q", tc.row, strings.TrimSpace(line), want)
			}
			if _, err := os.Stat(out); err != nil {
				t.Errorf("image not written: %v", err)
			}
		})
	}
}

// An explicit column count is the caller's, so it has to survive the row
// measurement unchanged: the autodetect is the only path that measures, and a
// fix that measured unconditionally would crop a capture taken on a narrower
// terminal than its content needed.
func TestScreenshotHonorsAnExplicitColumnCount(t *testing.T) {
	py := screenshotPython(t)
	dir := t.TempDir()
	capture := filepath.Join(dir, "wide.txt")
	if err := os.WriteFile(capture, []byte("日本語 model\n"), 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	out := filepath.Join(dir, "wide.png")
	script := filepath.Join(moduleRoot, "scripts", "screenshot.py")
	got, err := exec.Command(py, script, capture, out, "1", "40").CombinedOutput() // #nosec G204
	if err != nil {
		t.Fatalf("screenshot.py: %v\n%s", err, got)
	}
	if !strings.Contains(string(got), "from 40x1 cells") {
		t.Errorf("explicit cols 40: %s", strings.TrimSpace(string(got)))
	}
}

// The dashboard renders whatever an engine calls its model and whatever an
// agent calls itself, so the renderer draws names in any script. It drew every
// cell through one Latin Nerd Font, whose .notdef box came out for every
// character outside Latin: three different ideographs rendered as the same
// hollow rectangle, one per cell, and the screenshot then claimed to show a
// dashboard that shows the name.
//
// The renderer keeps a per-character face table and this asserts the table
// resolves, through the module the renderer itself uses, for CJK, Hangul,
// Cyrillic, Arabic and the box-drawing glyph the primary face already covers.
// A machine with none of the named Noto faces installed skips rather than
// failing on what it cannot render at all.
func TestScreenshotFallsBackPerCharacter(t *testing.T) {
	py := screenshotPython(t)
	probe := `
import sys
sys.path.insert(0, sys.argv[1])
import screenshot as s

regular, bold = s.resolve_fonts()
faces = [s.load_fonts(regular, bold, 16)[0]]
faces += s.load_fallback_fonts(s.resolve_fallback_fonts(), 16)
if len(faces) < 2:
    print("NOFALLBACK")
else:
    for ch in sys.argv[2]:
        print("yes" if any(s._covers(f, ch) for f in faces) else "no")
`
	for _, ch := range []string{"日", "語", "에", "Б", "ا", "ن", "文", "A", "│"} {
		out, err := exec.Command(py, "-c", probe, filepath.Join(moduleRoot, "scripts"), ch).CombinedOutput() // #nosec G204
		if err != nil {
			t.Fatalf("coverage probe for %q: %v\n%s", ch, err, out)
		}
		got := string(out)
		if strings.Contains(got, "NOFALLBACK") {
			t.Skip("no fallback face installed; nothing outside the primary family can be drawn here")
		}
		if !strings.Contains(got, "yes") {
			t.Errorf("no face draws %q:\n%s", ch, got)
		}
	}
}
