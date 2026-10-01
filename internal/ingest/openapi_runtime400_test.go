// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ingest

import (
	"net/http"
	"strings"
	"testing"
)

// The runtime answers an unparseable request line with a bare 400 before any
// handler runs, on every path. TestRuntimeRefusalsAreTheRuntimes collects that
// answer, and TestOpenAPIDocumentsEveryAnswerTheServerGives only reads it back
// from the POST, which declares a 400 of its own for a rejected body. So both
// passed while the spec described the runtime's 400 only in the prose of the
// 431-keyed response, and /healthz, whose handlers write no 400 at all,
// declared none.
//
// The status is a property of the request line rather than of the path, so the
// spec states it on every operation, and each points at the response the
// runtime's answer is described by. Without that pointer the POST's
// field-naming 400 and the runtime's bare one read as a single answer.
func TestRuntimeBadRequestIsDeclaredOnEveryOperation(t *testing.T) {
	for _, e := range ingestEndpoints {
		for _, op := range splitAllOperations(t, openapiSection(t, e.path)) {
			if !containsCode(openapiCodes(op), http.StatusBadRequest) {
				t.Errorf("%s declares no 400, but the runtime answers one to every request line it cannot parse; a client generated here has no answer for it",
					opName(e.path, op))
				continue
			}
			if !strings.Contains(op, "RuntimeBadRequest") {
				t.Errorf("%s declares a 400 but points nowhere at RuntimeBadRequest, so the handler's field-naming 400 and the runtime's bare one read as one answer",
					opName(e.path, op))
			}
		}
	}
}

// The answer itself, reached the only way it can be: over a raw connection, on
// a path whose own handlers never write a 400. Otherwise the spec above
// describes a status nothing on this port produces.
func TestRuntimeBadRequestIsGivenOnAProbePath(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	if got := rawStatus(t, s, "NOT-A-REQUEST-LINE\r\n\r\n"); got != http.StatusBadRequest {
		t.Errorf("an unparseable request line to %s answered %d, want 400", healthPath, got)
	}
}
