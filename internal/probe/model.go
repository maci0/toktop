package probe

import (
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// capModel puts an engine-supplied model id through the one shape the tree
// gives such a name, so the name stored in a snapshot and the name the
// generation request sends are the same string. The one-line fold is the
// load-bearing half: a Request.Model is not otherwise sanitized on its way
// into the JSON body.
func capModel(name string) string {
	return core.ModelName(name)
}

// SelectModel picks the model a probe should measure from what an engine
// reports it has: the first model that occupies VRAM, since a probe of one
// the daemon has not loaded pays its load time, and otherwise the first name
// left. Embedding and rerank models are never probed: they take no prompt
// tokens and report no decode rate.
func SelectModel(models []core.ModelInfo) string {
	var fallback string
	for _, m := range models {
		name := capModel(m.Name)
		if name == "" || skipProbeModel(name) {
			continue
		}
		if m.SizeVRAM > 0 {
			return name
		}
		if fallback == "" {
			fallback = name
		}
	}
	return fallback
}

// skipProbeModel reports whether a model is an embedding or reranking model,
// which take no completion tokens and would report a bogus throughput. The
// needles are ASCII and the name is engine-supplied, so it is folded with
// core.FoldASCII: strings.ToLower also folds runes whose lowercase is ASCII,
// so a model id spelled with U+0130 or U+212A would be skipped on a spelling
// the engine never published.
func skipProbeModel(name string) bool {
	n := core.FoldASCII(name)
	return strings.Contains(n, "embed") || strings.Contains(n, "rerank")
}
