// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
)

// agy (Antigravity CLI) appends one step per line to transcript.jsonl under
// ~/.gemini/antigravity-cli. The steps this package can count are the ones
// that carry a Gemini tokens object or usageMetadata. A step that names
// neither contributes nothing: the transcript of a session that only logged
// planner text has no token record to read.
//
// The working directory is the workspace or cwd on a record in the file. A
// file that names none is not attributed, so one project's watch does not
// read every other project's transcript.

func parseAgy(line []byte) (values, string, bool) {
	return parseGeminiRecord(line)
}

func agySessionCwd(line []byte) (string, bool) {
	line = bytes.TrimPrefix(bytes.TrimSpace(line), utf8BOM)
	var rec struct {
		Workspace string `json:"workspace"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return "", false
	}
	if rec.Cwd != "" {
		return rec.Cwd, true
	}
	if rec.Workspace != "" {
		return rec.Workspace, true
	}
	return "", false
}
