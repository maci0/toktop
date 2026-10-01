// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import "testing"

// baseNameEq compares without building the folded name, which is only a
// faithful stand-in for baseName if it accepts and rejects exactly what baseName
// does. These are the spellings the host hands it: a Windows path with a
// backslash and an uppercase extension, an interpreter module path, a name
// whose folding changes its length under nothing but ToLower's over-folding,
// and the multi-byte runes baseName's comment calls out.
func TestBaseNameEqualAgreesWithBaseName(t *testing.T) {
	names := []string{
		"",
		"/",
		".",
		"..",
		"vllm",
		"VLLM",
		"vLLM",
		"/usr/bin/python3",
		"/usr/lib/vllm/vllm.entrypoints.openai.api_server",
		`C:\Python\Scripts\LITELLM.EXE`,
		`C:\Python\Scripts\litellm.exe`,
		"litellm.exe",
		"LITELLM.EXE",
		"ollama.EXE",
		"ollama",
		".exe",
		"exe",
		"a.exe.exe",
		"x.EXE",
		"kvllm",           // a binary spelled through U+212A KELVIN SIGN
		"Ångström.EXE",    // multi-byte runes must be left alone, not rewritten
		"litellm\xff.exe", // an invalid byte must not become U+FFFD
		"ollama",
	}
	wants := []string{"vllm", "litellm", "ollama", "exe", "kvllm", "Ångström", "x", "nope"}
	for _, n := range names {
		for _, want := range wants {
			got := baseNameEqual(n, want)
			if wantBase := baseName(n) == want; got != wantBase {
				t.Errorf("baseNameEqual(%q, %q) = %v, baseName(%q) == %q is %v",
					n, want, got, n, want, wantBase)
			}
		}
	}
}

// baseNameEq searches every argument, so the allocation-free comparison has to
// hold on the argument list as well as on one name.
func TestBaseNameEqAgreesWithBaseNameOverArgs(t *testing.T) {
	args := [][]string{
		{},
		{"bash"},
		{"python3", "-m", "vllm.entrypoints.openai.api_server", "--port", "8000"},
		{`C:\Python\Scripts\LITELLM.EXE`, "--port", "4000"},
		{"gpustack", "start"},
		{"/opt/sglang/sglang.launch_server"},
		{"/opt/ramalama/bin/ramalama"},
	}
	wants := []string{"vllm", "litellm", "sglang", "ramalama", "gpustack", "bash"}
	for _, a := range args {
		for _, want := range wants {
			got := baseNameEq(a, want)
			wantRes := slicesContainsBaseName(a, want)
			if got != wantRes {
				t.Errorf("baseNameEq(%q, %q) = %v, want %v", a, want, got, wantRes)
			}
		}
	}
}

func slicesContainsBaseName(args []string, want string) bool {
	for _, a := range args {
		if baseName(a) == want {
			return true
		}
	}
	return false
}

// baseNameEq runs for every process on the host on every /proc poll, three
// matchers deep, so it must not allocate. A name containing uppercase used to
// be copied by FoldASCII once per matcher per argument.
func TestBaseNameEqDoesNotAllocate(t *testing.T) {
	args := []string{"/usr/lib/vllm/vllm.entrypoints.openai.api_server", "--served-model-name", "Qwen/Qwen3-32B", "--port", "8000"}
	if !baseNameEq(args, "vllm.entrypoints.openai.api_server") {
		t.Fatal("expected the interpreter module path to match by base name")
	}
	if allocs := testing.AllocsPerRun(100, func() { baseNameEq(args, "litellm") }); allocs != 0 {
		t.Errorf("baseNameEq allocates %.0f objects per call, want 0", allocs)
	}
}
