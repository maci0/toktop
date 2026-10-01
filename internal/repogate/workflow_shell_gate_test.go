// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package repogate

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The gate that shellchecks the bash inside the workflows. scripts/
// workflow-run-blocks.awk is the extractor it hands those blocks to, and this
// file pins both: the gate is the only analyzer in the tree that reads a
// workflow's `run:` blocks (yamllint parses the document around them), so an
// extractor that quietly skips a block reports a clean tree over steps nobody
// analyzed, which is the exact failure this repo's other gates exist to catch.

const workflowExtractor = "workflow-run-blocks.awk"

// extractWorkflowBlocks runs the tree's extractor over one workflow body and
// returns what it wrote, keyed by file name.
func extractWorkflowBlocks(t *testing.T, workflow string) map[string]string {
	t.Helper()
	return runExtractor(t, map[string]string{"ci.yml": workflow})
}

// runExtractor writes each workflow to a temp tree under its own name, runs the
// extractor over every one, and returns every file written, keyed by name. The
// workflows go in under real names because the extractor names its output after
// the file it read: a finding has to name a file a reader can open.
func runExtractor(t *testing.T, workflows map[string]string) map[string]string {
	t.Helper()
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skipf("awk unavailable: %v", err)
	}
	script := filepath.Join(moduleRoot, "scripts", workflowExtractor)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("scripts/%s is missing: %v", workflowExtractor, err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	for name, body := range workflows {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		cmd := exec.Command(awk, "-v", "outdir="+out, "-f", script, filepath.Join(dir, name))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("awk %s: %v\n%s", name, err, b)
		}
	}
	blocks := map[string]string{}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatalf("read outdir: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(out, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		blocks[e.Name()] = string(b)
	}
	return blocks
}

// blockBody drops the two header lines the extractor writes, a shebang and the
// comment naming the step, so a case compares the bash alone.
func blockBody(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) < 2 {
		return s
	}
	return strings.Join(lines[2:], "\n")
}

// TestWorkflowBlockExtraction pins what the extractor reads out of each shape
// a workflow here is written in. Every shellcheck run downstream is only as good
// as the block set it is handed.
func TestWorkflowBlockExtraction(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		file     string
		line     int
		body     string
	}{
		{
			// The form every multi-line step in this tree uses: `run:` as a
			// key under `- name:`, body indented past the key.
			name: "keyed step with a literal block",
			workflow: "jobs:\n" +
				"  a:\n" +
				"    steps:\n" +
				"      - name: gate\n" +
				"        run: |\n" +
				"          make check\n" +
				"          make lint\n",
			file: "ci.yml",
			line: 5,
			body: "make check\nmake lint",
		},
		{
			// A step with no name is written as a bare list item, so the block
			// starts on the line after the dash. An extractor that keyed only
			// on `run:` would drop every unnamed step while still reporting
			// that it had read the file.
			name: "bare list item",
			workflow: "jobs:\n" +
				"  a:\n" +
				"    steps:\n" +
				"      - run: |\n" +
				"          make check-shell\n",
			file: "ci.yml",
			line: 4,
			body: "make check-shell",
		},
		{
			// `- run: |2` names the body's column outright instead of deriving
			// it from the first content line, which is the case an extractor
			// that assumed `key indent + 2` reads the body one column left.
			name: "indentation indicator",
			workflow: "jobs:\n" +
				"  a:\n" +
				"    steps:\n" +
				"      - run: |2\n" +
				"            echo indented\n",
			file: "ci.yml",
			line: 4,
			body: "  echo indented",
		},
		{
			// A folded block is one line to the runner. Analyzing it as the
			// several the source shows is analyzing a script that is not the
			// one the step runs.
			name:     "folded block joins its lines",
			workflow: "jobs:\n  a:\n    steps:\n      - run: >-\n          echo a\n          echo b\n",
			file:     "ci.yml",
			line:     4,
			body:     "echo a echo b",
		},
		{
			// A step one space deeper than the tree's norm must still have its
			// last line read: the block ends at the key's column, not at a
			// hard-coded one.
			name:     "step indented deeper than its neighbours",
			workflow: "jobs:\n  a:\n    steps:\n      - name: deep\n         run: |\n           echo deep\n",
			file:     "ci.yml",
			line:     5,
			body:     "echo deep",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks := extractWorkflowBlocks(t, tc.workflow)
			want := tc.file + "-" + strconv.Itoa(tc.line) + ".bash"
			got, ok := blocks[want]
			if !ok {
				t.Fatalf("no block named %s; extracted %v", want, names(blocks))
			}
			if len(blocks) != 1 {
				t.Errorf("extracted %d blocks, want 1: %v", len(blocks), names(blocks))
			}
			if b := blockBody(got); b != tc.body {
				t.Errorf("block %s body:\ngot:\n%s\nwant:\n%s", want, b, tc.body)
			}
			if !strings.Contains(got, " line "+strconv.Itoa(tc.line)+"\n") {
				t.Errorf("block carries no comment naming the step it came from:\n%s", got)
			}
		})
	}
}

// TestWorkflowBlockExtractionKeepsEveryBlockSeparate pins that a workflow's
// blocks become separate files. The reason is scoping: a
// `# shellcheck disable` written into a block has to silence that step alone,
// and a gate that concatenated them would let one step's suppression answer
// for every other step in the same workflow.
func TestWorkflowBlockExtractionKeepsEveryBlockSeparate(t *testing.T) {
	workflow := "jobs:\n" +
		"  a:\n" +
		"    steps:\n" +
		"      - name: one\n" +
		"        run: |\n" +
		"          echo one\n" +
		"      - name: two\n" +
		"        run: |\n" +
		"          echo two\n"
	blocks := extractWorkflowBlocks(t, workflow)
	if len(blocks) != 2 {
		t.Fatalf("extracted %d blocks, want 2: %v", len(blocks), names(blocks))
	}
	for name, want := range map[string]string{"ci.yml-5.bash": "echo one", "ci.yml-8.bash": "echo two"} {
		got, ok := blocks[name]
		if !ok {
			t.Errorf("no block named %s; extracted %v", name, names(blocks))
			continue
		}
		if b := blockBody(got); b != want {
			t.Errorf("block %s body = %q, want %q", name, b, want)
		}
	}
}

// TestWorkflowBlockExtractionReadsEveryRealWorkflow pins the gate against the
// workflows in this tree rather than against fixtures. Each one is a
// 300-line document of comments, nested keys and blank lines between blocks, so
// an extractor that reads every fixture above while skipping real steps is
// exactly the failure worth catching: the count of block headers in the source
// is the count of blocks the gate must hand shellcheck.
func TestWorkflowBlockExtractionReadsEveryRealWorkflow(t *testing.T) {
	dir := filepath.Join(moduleRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	workflows := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		workflows[name] = string(raw)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflows found; the fixture is not reading the real ones")
	}
	blocks := runExtractor(t, workflows)

	for name, raw := range workflows {
		want := 0
		for _, line := range strings.Split(raw, "\n") {
			if isRunBlockHeader(line) {
				want++
			}
		}
		if want == 0 {
			t.Errorf("%s holds no 'run:' block", name)
		}
	}
	if len(blocks) != 9 {
		t.Errorf("the tree's workflows hold 9 'run:' blocks, extracted %d: %v", len(blocks), names(blocks))
	}

	// Every block is bash the runner would read, so it has to parse: a block
	// that lost its first or last line is a truncated script, and shellcheck
	// clearing it proves nothing about the step.
	for name, body := range blocks {
		b := blockBody(body)
		if strings.TrimSpace(b) == "" {
			t.Errorf("%s extracted empty", name)
			continue
		}
		if err := shellcheckSyntax(t, b); err != nil {
			t.Errorf("%s does not parse as bash:\n%s\n%v", name, b, err)
		}
	}
}

// isRunBlockHeader reports whether a workflow line opens a `run:` block scalar,
// counting the keyed form and the bare list item the tree writes, and every
// chomping and folding indicator the extractor accepts.
func isRunBlockHeader(line string) bool {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "- ")
	if !strings.HasPrefix(t, "run: ") {
		return false
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "run:"))
	if t == "" {
		return false
	}
	if t[0] != '|' && t[0] != '>' {
		return false
	}
	for _, c := range t[1:] {
		if c != '-' && c != '+' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// shellcheckSyntax parses a block the way check-workflow-shell reads it. The
// Go tree's own gates cannot run shellcheck here: a test in internal/repogate
// is compiled into a binary, and asking the host for an interpreter is the
// dependency this repo's tests already skip on when it is absent.
func shellcheckSyntax(t *testing.T, script string) error {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "block.bash")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write block: %v", err)
	}
	out, err := exec.Command(shell, "-n", path).CombinedOutput()
	if err != nil {
		t.Logf("bash -n output: %s", out)
	}
	return err
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
