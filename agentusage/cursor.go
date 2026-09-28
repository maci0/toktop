// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "path/filepath"

// cursorRoots is the agent-transcripts directory for one working directory.
// ~/.cursor/projects names that directory with pathSlug, and the transcripts
// live under agent-transcripts/ inside it. A line with no token usage
// contributes nothing; the directory is what says which project it is.
func cursorRoots(dir string) []string {
	base := home(".cursor", "projects")
	if base == "" {
		return nil
	}
	var out []string
	for _, spelling := range dirSpellings(dir) {
		slug := pathSlug(spelling)
		if slug == "" {
			continue
		}
		out = append(out, filepath.Join(base, slug, "agent-transcripts"))
	}
	return out
}
