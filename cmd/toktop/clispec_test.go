package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cliSpecEnv is a config-free environment. schema must not need a home, a
// credential, or a network, and the snapshot test must not inherit a
// TOKTOP_* value that changes the run.
func cliSpecEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"NO_COLOR=1",
	}
}

func buildToktop(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "toktop")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runToktop(t *testing.T, bin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = cliSpecEnv(t)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%v: %v\nstderr: %s", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

func TestSchemaDocument(t *testing.T) {
	bin := buildToktop(t)
	stdout, stderr, code := runToktop(t, bin, "schema")
	if code != 0 {
		t.Fatalf("schema exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("schema stderr = %q, want silence", stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("schema stdout is not JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{"clispec", "name", "version", "output", "global_args", "commands", "errors"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("schema missing %q", key)
		}
	}
	if doc["clispec"] != "0.3" || doc["name"] != "toktop" {
		t.Fatalf("identity = %v %v, want clispec 0.3 name toktop", doc["clispec"], doc["name"])
	}
	if _, bad := doc["command_layout"]; bad {
		t.Error("schema still carries command_layout")
	}
	verOut, _, verCode := runToktop(t, bin, "version")
	if verCode != 0 {
		t.Fatalf("version exit = %d", verCode)
	}
	fields := strings.Fields(verOut)
	if len(fields) < 2 || doc["version"] != fields[1] {
		t.Fatalf("schema version = %v, version command = %q", doc["version"], verOut)
	}
	output, _ := doc["output"].(map[string]any)
	if output["tty"] != "text" || output["piped"] != "json" {
		t.Fatalf("output = %v, want tty text and piped json", doc["output"])
	}
	raw, _ := json.Marshal(doc["global_args"])
	if !strings.Contains(string(raw), `"--output"`) || !strings.Contains(string(raw), `"auto"`) {
		t.Fatalf("global_args missing --output auto: %s", raw)
	}
	cmds, _ := doc["commands"].([]any)
	if len(cmds) == 0 {
		t.Fatal("schema commands is empty")
	}
	sawTTY := false
	for _, c := range cmds {
		cmd, _ := c.(map[string]any)
		for _, key := range []string{"name", "description", "effects"} {
			if cmd[key] == "" || cmd[key] == nil {
				t.Errorf("command %v missing %s", cmd["name"], key)
			}
		}
		if cmd["requires_tty"] == true {
			sawTTY = true
		}
		if _, nested := cmd["subcommands"]; nested {
			t.Errorf("%v still has subcommands", cmd["name"])
		}
	}
	if !sawTTY {
		t.Error("no command declares requires_tty")
	}
	errs, _ := doc["errors"].([]any)
	codes := map[string]float64{}
	for _, e := range errs {
		er, _ := e.(map[string]any)
		kind, _ := er["kind"].(string)
		codes[kind] = er["exit_code"].(float64)
	}
	if codes["usage"] != 2 || codes["failure"] != 1 || codes["tty_required"] != 4 {
		t.Fatalf("error codes = %v", codes)
	}

	filtered, _, fcode := runToktop(t, bin, "schema", "snapshot")
	if fcode != 0 {
		t.Fatalf("schema snapshot exit = %d", fcode)
	}
	var one map[string]any
	if err := json.Unmarshal([]byte(filtered), &one); err != nil {
		t.Fatalf("filtered schema: %v", err)
	}
	got, _ := one["commands"].([]any)
	if len(got) != 1 {
		t.Fatalf("schema snapshot commands = %d, want 1", len(got))
	}
	name, _ := got[0].(map[string]any)["name"].(string)
	if name != "snapshot" {
		t.Fatalf("filtered command = %q", name)
	}

	// -o text does not change the success bytes: schema is JSON in every mode.
	textOut, _, textCode := runToktop(t, bin, "-o", "text", "schema")
	if textCode != 0 {
		t.Fatalf("-o text schema exit = %d", textCode)
	}
	if !json.Valid([]byte(textOut)) {
		t.Fatalf("-o text schema is not JSON:\n%s", textOut)
	}
}

func TestSchemaUnknownPath(t *testing.T) {
	bin := buildToktop(t)
	stdout, stderr, code := runToktop(t, bin, "-o", "json", "schema", "no-such")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitUsage, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	var doc struct {
		Error struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastLine(stderr)), &doc); err != nil {
		t.Fatalf("envelope: %v\n%s", err, stderr)
	}
	if doc.Error.Kind != "usage" || !strings.Contains(doc.Error.Message, "no-such") {
		t.Fatalf("error = %+v", doc.Error)
	}
}

func TestUsageJSONEnvelope(t *testing.T) {
	bin := buildToktop(t)
	stdout, stderr, code := runToktop(t, bin, "-o", "json", "--interval", "0")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitUsage, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	var doc struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastLine(stderr)), &doc); err != nil {
		t.Fatalf("last stderr line is not the envelope: %v\n%s", err, stderr)
	}
	if doc.Error.Kind != "usage" {
		t.Fatalf("kind = %q", doc.Error.Kind)
	}

	_, textErr, textCode := runToktop(t, bin, "-o", "text", "--interval", "0")
	if textCode != exitUsage {
		t.Fatalf("-o text exit = %d, want %d\n%s", textCode, exitUsage, textErr)
	}
	if strings.HasPrefix(strings.TrimSpace(lastLine(textErr)), "{") {
		t.Fatalf("-o text usage error is JSON:\n%s", textErr)
	}
	if !strings.Contains(textErr, "--interval") {
		t.Fatalf("-o text stderr = %q, want the flag named", textErr)
	}
}

func TestSnapshotJSONAndText(t *testing.T) {
	bin := buildToktop(t)
	args := []string{"snapshot", "--demo", "--no-ingest", "--frames", "1", "--interval", "50ms"}
	stdout, stderr, code := runToktop(t, bin, append(args, "-o", "json")...)
	if code != 0 {
		t.Fatalf("snapshot -o json exit = %d\nstderr: %s", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("snapshot JSON: %v\n%s", err, stdout)
	}
	if _, wrapped := doc["items"]; wrapped {
		t.Fatalf("snapshot is wrapped in items: %s", stdout)
	}
	engines, _ := doc["engines"].([]any)
	if len(engines) == 0 {
		t.Fatalf("snapshot engines empty: %s", stdout)
	}
	if _, ok := doc["out_tok_per_s"]; !ok {
		t.Fatalf("snapshot missing out_tok_per_s: %s", stdout)
	}

	// auto when piped is JSON, the same document shape.
	autoOut, _, autoCode := runToktop(t, bin, args...)
	if autoCode != 0 {
		t.Fatalf("piped snapshot exit = %d", autoCode)
	}
	if !json.Valid([]byte(autoOut)) {
		t.Fatalf("piped snapshot is not JSON:\n%s", autoOut)
	}

	textOut, _, textCode := runToktop(t, bin, append(args, "-o", "text")...)
	if textCode != 0 {
		t.Fatalf("snapshot -o text exit = %d", textCode)
	}
	if json.Valid([]byte(textOut)) || !strings.Contains(strings.ToLower(textOut), "toktop") {
		t.Fatalf("-o text snapshot = %q, want the frame", textOut)
	}
}
