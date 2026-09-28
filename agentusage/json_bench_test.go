// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

// genericRecords is a mixed transcript as the generic adapters see it: a
// handful of records carrying counters, and the user messages, tool results and
// session notices around them that carry none. The mix is what the prefilter
// in parseGeneric pays for, so it is the shape the benchmark uses.
var genericRecords = [][]byte{
	[]byte(`{"type":"user","timestamp":"2026-09-28T10:00:00.000Z","sessionId":"6f1c","content":[{"type":"text","text":"add a bench to the json parser and tell me where the hot path is"}]}`),
	[]byte(`{"type":"tool.result","toolName":"Read","toolCallId":"call_01","status":"completed","result":"package agentusage\n\nimport (\n\t\"bytes\"\n\t\"encoding/json\"\n)\n\n// parseJSON reads one line of an agent transcript.\nfunc parseJSON(line []byte) (jsonEvent, bool) {\n\tvar doc any\n\tif err := json.Unmarshal(line, &doc); err != nil {\n\t\treturn jsonEvent{}, false\n\t}\n\treturn jsonEvent{}, true\n}\n"}`),
	[]byte(`{"type":"assistant","timestamp":"2026-09-28T10:00:04.120Z","cwd":"/home/dev/proj","message":{"role":"assistant","model":"claude-sonnet-4","usage":{"input_tokens":18,"output_tokens":412,"cache_read_input_tokens":51200,"cache_creation_input_tokens":640}}}`),
	[]byte(`{"type":"assistant","timestamp":"2026-09-28T10:00:09.004Z","cwd":"/home/dev/proj","message":{"role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"added the benchmark, it shows the decode dominating"}],"usage":{"input_tokens":4,"output_tokens":96,"cache_read_input_tokens":51700,"cache_creation_input_tokens":128}}}`),
	[]byte(`{"type":"tool.call","toolName":"Edit","toolCallId":"call_02","status":"pending","arguments":{"file_path":"/home/dev/proj/agentusage/json.go","old_string":"var doc any","new_string":"var doc any"}}`),
	[]byte(`{"type":"session.start","timestamp":"2026-09-28T09:59:58.000Z","data":{"context":{"cwd":"/home/dev/proj","model":"claude-sonnet-4"}}}`),
}

// BenchmarkParseGeneric measures one full pass over genericRecords, the work
// one poll of a copilot, cursor-agent or clanker transcript does between two
// reads. It exists to keep the prefilter honest: the win is on the records that
// carry no counters, so a corpus that held only usage records would report
// nothing.
func BenchmarkParseGeneric(b *testing.B) {
	b.ReportAllocs()
	var sink int
	for b.Loop() {
		for _, rec := range genericRecords {
			v, _, ok := parseGeneric(rec)
			if ok {
				sink += v.output
			}
		}
	}
	_ = sink
}
