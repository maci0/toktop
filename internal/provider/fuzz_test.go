package provider

import (
	"math"
	"testing"
)

// FuzzParsePromClassify drives the /metrics scrape pipeline (parseProm's
// exposition split plus classify's fuzzy name matching) with arbitrary
// bytes. Any local process or remote host a user points toktop at can serve
// this body, so whatever comes back must obey the boundary guarantees every
// consumer relies on: family values and classified metrics are usable
// numbers (no NaN/Inf survives into stored totals or rendered gauges), queue
// depths are non-negative, the KV percentage stays within 0..100 whenever
// it is reported at all, and parsing is deterministic.
func FuzzParsePromClassify(f *testing.F) {
	for _, seed := range []string{
		vllmFixture,
		"vllm:num_requests_running{model=\"q\"} 3\nvllm:num_requests_waiting{model=\"q\"} -7\n",
		"sglang:num_queue_reqs 1e300\nsglang:token_usage 0.5\n",
		"gen_total{a=\"1\"} 1e308\ngen_total{a=\"2\"} 1e308\ngen_total{a=\"3\"} -1e308\n",
		"ttft_seconds_sum 1e308\nttft_seconds_count 1e-320\n",
		"ttft_seconds_sum 4\nttft_seconds_count 20\n",
		"x_token_usage 1.5\nx_cache_usage_ratio nan\nx_cache_utilization +Inf\n",
		"# HELP m help\n# TYPE m counter\nm_bucket{le=\"+Inf\"} 2\nm_sum 5\nm_count 3\n",
		`metric{label="a,b c=d",other} 42`,
		"garbage\n\n   \nno_space_here\n1.5 trailing name 6\n",
		"-0.0 -0\nNaN NaN\n+Inf -Inf",
		"a 1e-323\nb 5e-324\nc 0x10\nd 0b101",
		"",
		"\n\n\n",
		"throughput_decode_generation_tokens_per_second 12.5\nrequest_wait_time_seconds 3\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		text := string(body)
		fam := parseProm(text)
		for name, v := range fam {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("family %q = %v is not a usable number", name, v)
			}
		}

		var m Metrics
		classify(fam, &m)

		if m.Running < 0 || m.Waiting < 0 {
			t.Fatalf("queue depths went negative: running=%d waiting=%d", m.Running, m.Waiting)
		}
		if m.HasKV && (m.KVPct < 0 || m.KVPct > 100) {
			t.Fatalf("HasKV with KVPct outside 0..100: %v", m.KVPct)
		}
		if m.HasDirectOutPS && m.DirectOutPS < 0 {
			t.Fatalf("HasDirectOutPS with a negative gauge: %v", m.DirectOutPS)
		}
		for name, v := range map[string]float64{
			"InTotal": m.InTotal, "OutTotal": m.OutTotal,
			"TTFTms": m.TTFTms, "DirectOutPS": m.DirectOutPS,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s = %v is not a usable number", name, v)
			}
			if v < 0 {
				t.Fatalf("%s = %v is negative", name, v)
			}
		}

		var again Metrics
		classify(parseProm(text), &again)
		if again.InTotal != m.InTotal || again.OutTotal != m.OutTotal ||
			again.Running != m.Running || again.Waiting != m.Waiting ||
			again.KVPct != m.KVPct || again.HasKV != m.HasKV ||
			again.TTFTms != m.TTFTms || again.DirectOutPS != m.DirectOutPS ||
			again.HasDirectOutPS != m.HasDirectOutPS {
			t.Fatalf("classify is not deterministic: %+v vs %+v", m, again)
		}
	})
}
