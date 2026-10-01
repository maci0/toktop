// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import "testing"

// foldCorpus is a fixed set of command lines with the shapes a host really
// holds: a bare comm, an interpreter with a module path, a long browser-like
// argv, and ones carrying uppercase to exercise the fold itself. Fixed input
// rather than the live process table (see BenchmarkListLinux) so the engine
// matcher's own share of a sweep is legible rather than mixed with whatever
// this host happens to be running.
var foldCorpus = []Info{
	{Name: "/usr/lib/systemd/systemd", Args: []string{"/usr/lib/systemd/systemd", "--switched-root", "--system"}},
	{Name: "/usr/bin/python3", Args: []string{"/usr/bin/python3", "-m", "sglang.launch_server", "--model-path", "/models/Qwen-32B", "--port", "30000"}},
	{Name: "/usr/lib/firefox/firefox", Args: []string{"/usr/lib/firefox/firefox", "-new-instance", "-Profile", "/home/u/.mozilla/firefox/abc.default", "--MOZ_LOG", "PlatformDecoderModule:5"}},
	{Name: "ollama", Args: []string{"ollama", "serve"}},
	{Name: "/opt/vllm/vllm.entrypoints.openai.api_server", Args: []string{"python3", "-m", "vllm.entrypoints.openai.api_server", "--served-model-name", "Qwen/Qwen3-32B"}},
	{Name: "bash", Args: []string{"bash"}},
}

// BenchmarkMatchEngineFold measures one pass of the corpus through a single
// reused fold scratch, which is what a /proc poll does once per process on the
// host. It is the ratchet on the fold buffers: with them the pass allocates
// nothing per process, and without them each line built its own joined fold
// and its own per-argument folds, which is the largest source of garbage a
// sweep produced (the fold was 61% of the bytes one whole sweep allocated).
func BenchmarkMatchEngineFold(b *testing.B) {
	b.ReportAllocs()
	scratch := &foldScratch{}
	for b.Loop() {
		for _, i := range foldCorpus {
			matchEngine(scratch, i)
		}
	}
}
