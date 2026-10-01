//go:build sqlite

package agentusage

import (
	"os"
	"path/filepath"
	"testing"
)

// crushDBPath is walked on the poll path for every watched directory, up to
// crushMaxWalkUp levels each, and the answer on a machine that never ran
// crush is "no store". This pins that answer's cost so the Lstat prefilter
// that answers it without opening descriptors cannot be undone silently.
func BenchmarkCrushDBPathAbsent(b *testing.B) {
	dir := b.TempDir()
	deep := dir
	for range 10 {
		deep = filepath.Join(deep, "sub")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if p := crushDBPath(deep); p != "" {
			b.Fatalf("a store appeared above a temporary tree: %s", p)
		}
	}
}
