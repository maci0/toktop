package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
)

const vllmFixture = `# HELP vllm:num_requests_running Number of requests currently running.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model="qwen"} 3.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model="qwen"} 7.0
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{model="qwen"} 0.62
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{model="qwen"} 1000.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{model="qwen"} 2500.0
vllm:time_to_first_token_seconds_sum{model="qwen"} 4.0
vllm:time_to_first_token_seconds_count{model="qwen"} 20.0
vllm:request_success_total{finished_reason="stop",model_name="qwen"} 11
`

func TestParsePromSumsLabeledSeries(t *testing.T) {
	// Two series of one family: vllmFixture has every family exactly once,
	// so it can only prove the last-wins overwrite, never the accumulation.
	fam := parseProm(vllmFixture + "vllm:prompt_tokens_total{model=\"other\"} 250.0\n")
	if got := fam["vllm:prompt_tokens_total"]; got != 1250 {
		t.Fatalf("prompt_tokens_total = %v, want 1250 (1000 + 250)", got)
	}
	if _, ok := fam["vllm:num_requests_running"]; !ok {
		t.Fatal("labels were not stripped from family names")
	}
}

func TestParsePromTimestampedSamples(t *testing.T) {
	for _, suffix := range []string{"", " 1700000000000", "\t1700000001000"} {
		t.Run(suffix, func(t *testing.T) {
			text := "vllm:generation_tokens_total{model=\"qwen\"} 100" + suffix + "\n" +
				"vllm:generation_tokens_total{model=\"other\"}\t50" + suffix + "\n" +
				"vllm:prompt_tokens_total 200" + suffix + "\n" +
				"vllm:num_requests_running\t3" + suffix + "\n"
			var m Metrics
			classify(parseProm(text), &m)
			if m.OutTotal != 150 || m.InTotal != 200 || m.Running != 3 {
				t.Fatalf("timestamp changed measurements: %+v", m)
			}
		})
	}
}

func TestParsePromSkipsCommentsAndBuckets(t *testing.T) {
	fam := parseProm("# comment\nm_bucket{le=\"1\"} 2\nother 3\n")
	if len(fam) != 1 || fam["other"] != 3 {
		t.Fatalf("unexpected families: %#v", fam)
	}
}

// The exposition format permits NaN/+Inf values (0/0 gauges on idle engines);
// one such series must not poison the summed family, or the collector's
// stored totals keep every derived rate NaN until restart.
func TestParsePromRejectsNonFinite(t *testing.T) {
	fam := parseProm("gen_total{m=\"a\"} 5\ngen_total{m=\"b\"} NaN\ngen_max{m=\"a\"} +Inf\n")
	if fam["gen_total"] != 5 {
		t.Fatalf("NaN series poisoned the family sum: %v", fam["gen_total"])
	}
	if _, ok := fam["gen_max"]; ok {
		t.Fatal("+Inf series parsed as data")
	}
}

// Two finite series of the same family can sum past MaxFloat64 even though
// each line parsed clean; the poisoned family must not reach the stored
// totals, or every derived rate stays broken until restart.
func TestParsePromRejectsOverflowingSum(t *testing.T) {
	fam := parseProm("gen_total{m=\"a\"} 1e308\ngen_total{m=\"b\"} 1e308\n")
	// The first series stored a finite sum, so the family keeps it and the
	// overflowing second series is dropped, leaving 1e308.
	if v := fam["gen_total"]; v != 1e308 {
		t.Fatalf("overflowing family sum = %v, want the last finite sum 1e308", v)
	}
	var m Metrics
	classify(parseProm("vllm:generation_tokens_total{a=\"1\"} 1e308\n"+
		"vllm:generation_tokens_total{a=\"2\"} 1e308\n"+
		"vllm:prompt_tokens_total{a=\"1\"} 7\n"), &m)
	if m.OutTotal != 1e308 {
		t.Fatalf("OutTotal = %v from an overflowing family, want the last finite sum 1e308", m.OutTotal)
	}
	// A non-overflowing family in the same scrape still lands, so the guard
	// drops the one poisoned sum rather than the whole exposition.
	if m.InTotal != 7 {
		t.Fatalf("InTotal = %v, want 7 from the family that does not overflow", m.InTotal)
	}

	// The mean divides a huge sum by a denormal count; an overflowing
	// quotient is no measurement.
	var ttft Metrics
	classify(parseProm("ttft_seconds_sum 1e308\nttft_seconds_count 1e-320\n"), &ttft)
	if math.IsInf(ttft.TTFTms, 0) || math.IsNaN(ttft.TTFTms) {
		t.Fatalf("TTFTms = %v from an overflowing mean, want the reading dropped", ttft.TTFTms)
	}
}

func TestClassifyVLLM(t *testing.T) {
	var m Metrics
	classify(parseProm(vllmFixture), &m)
	if m.InTotal != 1000 || m.OutTotal != 2500 {
		t.Errorf("totals = in:%v out:%v", m.InTotal, m.OutTotal)
	}
	if m.Running != 3 || m.Waiting != 7 {
		t.Errorf("queues = run:%d wait:%d", m.Running, m.Waiting)
	}
	// 0.62 in the fixture, scaled by 100 in classify: exactly 62, so assert
	// exactly. A band would hide a wrong-but-nearby scale factor.
	if !m.HasKV || m.KVPct != 62 {
		t.Errorf("kv pct = %v hasKV=%v", m.KVPct, m.HasKV)
	}
	if want := 4.0 / 20.0 * 1000; m.TTFTms != want {
		t.Errorf("ttft ms = %v, want %v", m.TTFTms, want)
	}
}

func TestClassifyRunningIgnoresDurations(t *testing.T) {
	for _, name := range []string{
		"request_processing_duration_seconds_sum",
		"request_processing_time_sum",
		"request_processing_seconds_count",
		"req_processing_duration_seconds_sum",
	} {
		t.Run(name, func(t *testing.T) {
			var m Metrics
			classify(parseProm("num_requests_running 3\n"+name+" 12\n"), &m)
			if m.Running != 3 {
				t.Errorf("running = %d, want 3", m.Running)
			}
		})
	}
}

func TestClassifyRatioVsPercentCache(t *testing.T) {
	var ratio Metrics
	classify(map[string]float64{"llamacpp:kv_cache_usage_ratio": 0.75}, &ratio)
	if ratio.KVPct != 75 {
		t.Errorf("ratio -> pct = %v", ratio.KVPct)
	}
	var pct Metrics
	classify(map[string]float64{"kv_cache_usage_perc": 40}, &pct)
	if pct.KVPct != 40 {
		t.Errorf("percent passthrough = %v", pct.KVPct)
	}
	// vLLM's gpu_cache_usage_perc publishes a 0..1 fraction despite the
	// suffix, so the rescale keys on the value and not on the name.
	var frac Metrics
	classify(map[string]float64{"vllm:gpu_cache_usage_perc": 0.62}, &frac)
	if frac.KVPct != 62 {
		t.Errorf("fraction under a _perc name = %v, want 62", frac.KVPct)
	}
}

// A broken or lying /metrics endpoint may publish any finite float. The
// plain conversion this used to apply is implementation-defined past the
// type's range: on amd64, int(1e300) renders as a huge negative queue depth.
func TestClassifySaturatesAbsurdGauges(t *testing.T) {
	var m Metrics
	classify(map[string]float64{"vllm:num_requests_running": 1e300}, &m)
	if m.Running != math.MaxInt {
		t.Errorf("running = %d, want saturation at MaxInt", m.Running)
	}

	var junk Metrics
	classify(map[string]float64{"sglang:num_queue_reqs": -3}, &junk)
	if junk.Waiting != 0 {
		t.Errorf("waiting = %d from a negative gauge, want 0", junk.Waiting)
	}
}

func TestSplitMetric(t *testing.T) {
	for _, line := range []string{
		`a:b_c{label="x,y z"} 1.5e2`,
		`a:b_c{label="x,y z"} 1.5e2 1700000000000`,
		`a:b_c{label="x\"} y",path="z\\"} 1.5e2 1700000000000`,
		"a:b_c\t1.5e2\t1700000000000",
		"a:b_c{} \t 1.5e2 \t 1700000000000",
	} {
		t.Run(line, func(t *testing.T) {
			name, val, ok := splitMetric(line)
			if !ok || name != "a:b_c" || val != 150 {
				t.Fatalf("got %q %v %v", name, val, ok)
			}
		})
	}
	for _, line := range []string{
		"garbage", "garbage \t", `a:b_c{label="unfinished 150`,
		"a:b_c NaN 1700000000000", "a:b_c +Inf 1700000000000",
		"a:b_c invalid 1700000000000",
	} {
		if _, _, ok := splitMetric(line); ok {
			t.Errorf("invalid sample parsed: %q", line)
		}
	}
}

// Ensure kind constants stay stable; they key UI colors and probes.
func TestKindConstants(t *testing.T) {
	cases := []struct{ got, want string }{
		{core.KindOllama, "ollama"},
		{core.KindVLLM, "vllm"},
		{core.KindLlamaCPP, "llama.cpp"},
		{core.KindOpenAI, "openai"},
		{core.KindSGLang, "sglang"},
		{core.KindTRTLLM, "trt-llm"},
		{core.KindMLX, "mlx"},
		{core.KindLMStudio, "lmstudio"},
		{core.KindKoboldCPP, "koboldcpp"},
		{core.KindLocalAI, "localai"},
		{core.KindTGI, "tgi"},
		{core.KindLiteLLM, "litellm"},
		{core.KindGPUStack, "gpustack"},
		{core.KindLemonade, "lemonade"},
		{core.KindOmniRoute, "omnirouter"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("kind constant %q drifted from %q", c.got, c.want)
		}
	}
}

func TestIdentifyOmniRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OmniRoute-Route-Class", "CLIENT_API")
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"auto","context_length":1048576}]}`))
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	if kind := identify(context.Background(), srv.URL); kind != core.KindOmniRoute {
		t.Errorf("Identify = %q, want omnirouter", kind)
	}
}

func TestIdentifyNotOmniRouteWithoutHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	if kind := identify(context.Background(), srv.URL); kind == core.KindOmniRoute {
		t.Error("plain server misidentified as omnirouter")
	}
}

func TestPollCarriesBearerAndContextLength(t *testing.T) {
	t.Cleanup(func() { bearer.Set("") })
	bearer.Set("sk-live")

	var gotAuth atomic.Value
	gotAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"data":[{"id":"auto/big","context_length":200000}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "test", core.KindOmniRoute)
	// Without an explicit Allow the token must not ride a poll: this
	// provider was not named by the operator, only built against a URL.
	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := gotAuth.Load().(string); a != "" {
		t.Errorf("Authorization = %q before Allow, want unset", a)
	}

	bearer.Allow(srv.URL)
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := gotAuth.Load().(string); a != "Bearer sk-live" {
		t.Errorf("Authorization = %q", a)
	}
	if len(m.Models) != 1 || m.Models[0].Name != "auto/big" {
		t.Fatalf("models = %+v", m.Models)
	}
	if m.Models[0].CtxMax != 200000 {
		t.Errorf("CtxMax = %d, want 200000", m.Models[0].CtxMax)
	}
}

// A same-origin redirect is followed and the token rides it. A hop to another
// origin is refused: every base here is a scanned loopback port or an
// ssh-forwarded remote port, so following a redirect off it would let the
// listener answer a poll by sending the operator's host to any URL instead.
func TestProviderRedirectAuthorization(t *testing.T) {
	bearer.Set("sk-test")
	t.Cleanup(func() { bearer.Set("") })
	for _, crossOrigin := range []bool{false, true} {
		for _, fetch := range []struct {
			name string
			run  func(string) error
		}{
			{"json", func(url string) error {
				return getJSON(context.Background(), url, &struct{}{})
			}},
			{"text", func(url string) error {
				_, err := getText(context.Background(), httpClient, url)
				return err
			}},
			{"scan text", func(url string) error {
				_, err := getText(context.Background(), scanClient, url)
				return err
			}},
			{"scan", func(url string) error {
				resp, err := get(context.Background(), scanClient, url)
				if err == nil {
					resp.Body.Close()
				}
				return err
			}},
		} {
			t.Run(fmt.Sprintf("%s/cross=%v", fetch.name, crossOrigin), func(t *testing.T) {
				var received atomic.Int32
				final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received.Add(1)
					if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
						t.Errorf("redirect Authorization = %q, want the token", got)
					}
					fmt.Fprint(w, `{}`)
				})
				other := httptest.NewServer(final)
				defer other.Close()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/final" {
						final.ServeHTTP(w, r)
						return
					}
					if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
						t.Errorf("initial Authorization = %q", got)
					}
					target := "/final"
					if crossOrigin {
						target = other.URL + target
					}
					http.Redirect(w, r, target, http.StatusFound)
				}))
				defer srv.Close()
				bearer.Allow(srv.URL)
				err := fetch.run(srv.URL)
				wantRequests := int32(1)
				if crossOrigin {
					if err == nil {
						t.Fatal("cross-origin redirect was followed")
					}
					wantRequests = 0
				} else if err != nil {
					t.Fatal(err)
				}
				if got := received.Load(); got != wantRequests {
					t.Errorf("final requests = %d, want %d", got, wantRequests)
				}
			})
		}
	}
}

func TestPollFetchesMetricsAndModelsTogether(t *testing.T) {
	both := make(chan struct{})
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics", "/v1/models":
			if n.Add(1) == 2 {
				close(both)
			}
			select {
			case <-both:
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
				t.Errorf("%s started without the other request in flight", r.URL.Path)
				return
			}
			if r.URL.Path == "/metrics" {
				w.Write([]byte("vllm:generation_tokens_total 9\n"))
				return
			}
			w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "vllm", core.KindOpenAI)
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.OutTotal != 9 {
		t.Errorf("OutTotal = %v, want 9", m.OutTotal)
	}
	if len(m.Models) != 1 || m.Models[0].Name != "qwen" {
		t.Fatalf("models = %+v", m.Models)
	}
}

func TestProviderKind(t *testing.T) {
	if k := NewOllama("http://127.0.0.1:11434").Kind; k != core.KindOllama {
		t.Errorf("Ollama.Kind = %q, want %q", k, core.KindOllama)
	}
	if k := NewOpenAICompat("http://127.0.0.1:8000", "vllm", core.KindVLLM).Kind; k != core.KindVLLM {
		t.Errorf("OpenAICompat.Kind = %q, want %q", k, core.KindVLLM)
	}
}

func TestCandidatePortsIncludeOmniRoute(t *testing.T) {
	if slices.Contains(CandidatePorts(), 20128) {
		return
	}
	t.Error("20128 missing from candidate ports")
}

// The Ollama provider must list loaded models from /api/ps, falling back to
// the `model` field when `name` is absent, and carry the daemon version.
func TestPollOllamaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(`{"models":[` +
				`{"name":"llama3:latest","size_vram":8000000000},` +
				`{"model":"qwen2:7b-instruct-q4_K_M","size_vram":4700000000}]}`))
		case "/api/version":
			w.Write([]byte(`{"version":"0.5.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOllama(srv.URL)
	if p.Addr != srv.URL || p.Label != "ollama" || p.Kind != core.KindOllama {
		t.Fatalf("identity = %s/%s/%s", p.Label, p.Addr, p.Kind)
	}
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 2 {
		t.Fatalf("models = %+v, want 2", m.Models)
	}
	if m.Models[0].Name != "llama3:latest" || m.Models[0].SizeVRAM != 8000000000 {
		t.Errorf("model[0] = %+v", m.Models[0])
	}
	if m.Models[1].Name != "qwen2:7b-instruct-q4_K_M" {
		t.Errorf("empty name must fall back to model field: %+v", m.Models[1])
	}
	if m.Version != "0.5.4" {
		t.Errorf("version = %q, want 0.5.4", m.Version)
	}

	srv.Close()
	if _, err := p.Poll(context.Background()); err == nil {
		t.Error("poll against dead daemon must fail")
	}
}

// expires_at is a bare integer on some Ollama-compatible proxies and a
// gateway may drop the offset. Nothing reads it, so it must not decide
// whether the poll succeeds.
func TestPollOllamaToleratesNonRFC3339ExpiresAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(`{"models":[` +
				`{"name":"llama3:latest","size_vram":8000000000,"expires_at":1750000000},` +
				`{"name":"qwen2:7b","size_vram":4700000000,"expires_at":""}]}`))
		case "/api/version":
			w.Write([]byte(`{"version":"0.5.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m, err := NewOllama(srv.URL).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 2 {
		t.Fatalf("models = %+v, want 2", m.Models)
	}
	if m.Version != "0.5.4" {
		t.Errorf("version = %q, want 0.5.4", m.Version)
	}
}

// LM Studio enrichment must replace the thin OpenAI listing with the native
// v0 feed, dropping non-LLM and unloaded entries so a probe cannot JIT-load.
func TestPollLMStudioEnrichment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"qwen"},{"id":"stale-only-here"}]}`))
		case "/api/v0/models":
			w.Write([]byte(`{"data":[` +
				`{"id":"cold","type":"llm","state":"not-loaded","max_context_length":4096},` +
				`{"id":"embedder","type":"embeddings"},` +
				`{"id":"qwen","type":"llm","state":"loaded","max_context_length":8192}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "lmstudio", core.KindLMStudio)
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ModelInfo{{Name: "qwen", CtxMax: 8192}}
	if len(m.Models) != 1 || m.Models[0] != want[0] {
		t.Fatalf("models = %+v, want %+v (unloaded and non-llm dropped, v0 feed wins)", m.Models, want)
	}
}

// Lemonade enrichment must surface the health version and the loaded-model
// inventory in preference to anything scraped elsewhere.
func TestPollLemonadeEnrichment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"status":"ok","version":"9.3.3","all_models_loaded":[` +
				`{"model_name":"Llama-3.2-1B","ctx_size":4096}]}`))
		case "/v1/models":
			http.Error(w, "needs auth", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "lemonade", core.KindLemonade)
	m, err := p.Poll(context.Background()) // tolerant kind: auth-walled models must not fail the poll
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "9.3.3" {
		t.Errorf("version = %q, want 9.3.3", m.Version)
	}
	want := []core.ModelInfo{{Name: "Llama-3.2-1B", CtxMax: 4096}}
	if len(m.Models) != 1 || m.Models[0] != want[0] {
		t.Fatalf("models = %+v, want %+v", m.Models, want)
	}
}

// LiteLLM and other engines that do not need both /metrics and /v1/models
// still have to fail when neither answers: an empty success rendered them
// as idle on the dashboard while they were down.
func TestPollFailsWhenNeitherEndpointAnswers(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	for _, kind := range []string{
		core.KindOpenAI, core.KindLiteLLM, core.KindGPUStack,
		core.KindOmniRoute, core.KindTRTLLM, core.KindLMStudio, core.KindLemonade,
	} {
		p := NewOpenAICompat(srv.URL, kind, kind)
		_, err := p.Poll(context.Background())
		if err == nil {
			t.Errorf("kind %s: poll succeeded against a server with no engine API", kind)
		} else if !strings.Contains(err.Error(), "no known endpoints") {
			t.Errorf("kind %s: err = %v, want it to name the missing endpoints", kind, err)
		}
	}
}

// One of the two OpenAI-shaped endpoints is enough: LiteLLM often has
// /v1/models and no Prometheus /metrics.
func TestPollSucceedsWithModelsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "litellm", core.KindLiteLLM)
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 1 || m.Models[0].Name != "m" {
		t.Fatalf("models = %+v, want [{m}]", m.Models)
	}

	// Same server as vLLM: a model listing alone does not describe a vLLM
	// engine, so a missing /metrics must fail the poll.
	vllm := NewOpenAICompat(srv.URL, core.KindVLLM, core.KindVLLM)
	if m, err := vllm.Poll(context.Background()); err == nil {
		t.Errorf("vLLM poll succeeded on /v1/models alone: %+v", m)
	}
}

// Version extraction feeds the UI header across wildly different engine
// responses; pin every accepted shape and every rejection.
func TestExtractVersionField(t *testing.T) {
	cases := map[string]string{
		`{"version":"0.5.4"}`:                                 "0.5.4",
		`{"Version":"1.2.3"}`:                                 "1.2.3",
		`{"server_info":{"version":"9.9"}}`:                   "9.9",
		`{"backend_version_info":{"sglang_version":"0.4.2"}}`: "0.4.2",
		`"b1234"`:                "b1234", // llama.cpp bare quoted string
		"b4600":                  "b4600", // llama.cpp plain text
		`{"nope":1}`:             "",
		"":                       "",
		`{"a":1} trailing junk`:  "",
		strings.Repeat("x", 129): "", // over-long text is not a version
		// versionCap counts characters, and three bytes per character is what
		// makes the two readings differ: a byte-counted reject drops the 128
		// characters below and keeps the ASCII version of the same length.
		// U+7248, built from its code point so no editor can leave an ASCII
		// lookalike that reduces the case to the one above.
		strings.Repeat(string(rune(0x7248)), versionCap):   strings.Repeat(string(rune(0x7248)), versionCap),
		strings.Repeat(string(rune(0x7248)), versionCap+1): "",
		"\x8c":            "", // a bare invalid byte is not a version
		"\"\n\"":          "", // nor is a line break the engine quoted
		"\"\x1b[2J\"":     "", // a quoted escape sequence is not a version
		"b4600  \t b4600": "b4600 b4600",
	}
	for body, want := range cases {
		if got := extractVersionField(body); got != want {
			t.Errorf("extractVersionField(%q) = %q, want %q", body, got, want)
		}
	}
}

// A version probe fired while the engine is still starting must not be
// cached as a permanent miss: later polls retry after versionRetry, and
// the first success is memoized so healthy engines are asked once per
// refresh window rather than once per poll.
func TestVersionCacheRetriesUntilResolved(t *testing.T) {
	var failing, reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs.Add(1)
		if failing.Load() == 1 { // every version endpoint dark: engine starting up
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"version":"2.7.1"}`)
	}))
	defer srv.Close()

	now := fakeClock(t)
	var vc versionCache
	ctx := context.Background()
	failing.Store(1)
	if got := vc.fetch(ctx, srv.URL); got != "" {
		t.Fatalf("first fetch during outage = %q, want empty", got)
	}
	failing.Store(0)
	if got := vc.fetch(ctx, srv.URL); got != "" {
		t.Fatalf("fresh miss = %q, want empty until versionRetry", got)
	}
	if n := reqs.Load(); n != 3 { // outage sweep of 3 paths, then cache silence
		t.Fatalf("server hit %d times before expiry, want 3", n)
	}

	now(versionRetry + time.Second) // the injected clock, not real elapsed time, expires the window
	if got := vc.fetch(ctx, srv.URL); got != "2.7.1" {
		t.Fatalf("fetch after recovery = %q, want 2.7.1", got)
	}
	for range 3 { // resolved: no further traffic
		if got := vc.fetch(ctx, srv.URL); got != "2.7.1" {
			t.Fatalf("cached fetch = %q, want 2.7.1", got)
		}
	}
	if n := reqs.Load(); n != 4 { // outage sweep of 3 paths, then one resolving request, then cache silence
		t.Errorf("server hit %d times, want 4", n)
	}
}

// An engine replaced under a running dashboard (container re-pulled, remote
// host restarted with a new image) answers a different version. A resolved
// hit kept for the process lifetime would pin the old one until the operator
// quit, so the cache re-asks after versionRefresh and keeps showing the last
// good version while the engine is down rather than blanking the readout.
func TestVersionCacheRefreshesResolvedHit(t *testing.T) {
	var version atomic.Value
	version.Store("0.6.0")
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs.Add(1)
		v, _ := version.Load().(string)
		if v == "" { // engine restarting: every version endpoint dark
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"version":%q}`, v)
	}))
	defer srv.Close()

	now := fakeClock(t)
	var vc versionCache
	ctx := context.Background()
	if got := vc.fetch(ctx, srv.URL); got != "0.6.0" {
		t.Fatalf("first fetch = %q, want 0.6.0", got)
	}
	version.Store("0.7.0")
	if got := vc.fetch(ctx, srv.URL); got != "0.6.0" {
		t.Fatalf("fetch inside the refresh window = %q, want the cached 0.6.0", got)
	}

	now(versionRefresh + time.Second)
	if got := vc.fetch(ctx, srv.URL); got != "0.7.0" {
		t.Fatalf("fetch after the refresh window = %q, want 0.7.0", got)
	}

	// A dark refresh must not blank a version the engine already reported.
	version.Store("")
	now(2 * (versionRefresh + time.Second))
	if got := vc.fetch(ctx, srv.URL); got != "0.7.0" {
		t.Fatalf("fetch while the engine is down = %q, want the last good 0.7.0", got)
	}
	if reqs.Load() < 5 {
		t.Errorf("server hit %d times, want the expired windows re-probed", reqs.Load())
	}
}

// A frozen clock is the degenerate case a replay runs under: nothing may
// expire on its own, so the cache has to answer from memory for as long as
// the run lasts. Time elapsing on the wall clock must not expire a window the
// replay's clock has not reached.
func TestVersionCacheFrozenClockNeverExpires(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs.Add(1)
		fmt.Fprint(w, `{"version":"3.1.4"}`)
	}))
	defer srv.Close()

	fakeClock(t) // frozen: never advanced
	var vc versionCache
	ctx := context.Background()
	if got := vc.fetch(ctx, srv.URL); got != "3.1.4" {
		t.Fatalf("first fetch = %q, want 3.1.4", got)
	}
	time.Sleep(time.Millisecond) // real time passes; the injected clock does not
	for range 3 {
		if got := vc.fetch(ctx, srv.URL); got != "3.1.4" {
			t.Fatalf("fetch under a frozen clock = %q, want the cached 3.1.4", got)
		}
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("server hit %d times, want 1: a frozen clock must not re-probe", n)
	}
}

// The seam has to reach a real poll, not just the cache struct: the version
// endpoint is hit once per window, and the window is the injected clock's.
// Without SetNow the same two polls a replay steps through reach the network
// or not according to how long the process happened to be up.
func TestPollVersionTrafficFollowsInjectedClock(t *testing.T) {
	var versionReqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"llama3:latest"}]}`))
		case "/api/version":
			versionReqs.Add(1)
			w.Write([]byte(`{"version":"0.5.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := fakeClock(t)
	p := NewOllama(srv.URL)
	ctx := context.Background()
	for range 3 { // one resolving request, then silence for the window
		if _, err := p.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := versionReqs.Load(); n != 1 {
		t.Fatalf("version endpoint hit %d times inside the window, want 1", n)
	}
	now(versionRefresh + time.Second)
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if n := versionReqs.Load(); n != 2 {
		t.Fatalf("version endpoint hit %d times after the window, want 2", n)
	}
}

// fakeClock installs a stepped clock for the version caches and returns the
// function that advances it. The window a fetch sees is the only thing that
// decides its network traffic, so a test that ages the cache by sleeping is a
// test that can flake; this one moves time explicitly and restores the wall
// clock afterwards.
func fakeClock(t *testing.T) func(time.Duration) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	SetNow(func() time.Time { return cur })
	t.Cleanup(func() { SetNow(nil) })
	return func(d time.Duration) { cur = cur.Add(d) }
}

// The version chain is up to three HTTP round trips under the caller's
// context, and a cache shared by a poll and an ad-hoc identify must not pin
// one behind the other for the whole of it. A second fetch reaches the engine
// and answers while the first is still sitting on /api/version, so a wedged
// endpoint costs the fetch that asked for it and nobody else.
func TestVersionCacheProbeDoesNotHoldTheCacheLock(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reqs.Add(1) == 1 { // the first probe stalls until the test releases it
			entered <- struct{}{}
			<-gate
		}
		fmt.Fprint(w, `{"version":"4.5.0"}`)
	}))
	release := sync.OnceFunc(func() { close(gate) })
	defer srv.Close()
	defer release() // runs before srv.Close, which waits for the handler

	fakeClock(t)
	var vc versionCache
	first := make(chan string, 1)
	go func() { first <- vc.fetch(context.Background(), srv.URL) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first fetch never reached the engine")
	}

	// The stalled probe is holding no cache lock. A fetch that took c.mu
	// around the whole chain would fail this the moment the engine answered
	// /api/version, and would pin every other reader of the cache for three
	// round trips a time.
	if !vc.mu.TryLock() {
		t.Error("an in-flight version probe holds the cache lock")
	} else {
		vc.mu.Unlock()
	}

	// And the second fetch gets there on its own, rather than waiting for the
	// gate the first one is parked on.
	second := make(chan string, 1)
	go func() { second <- vc.fetch(context.Background(), srv.URL) }()
	select {
	case got := <-second:
		if got != "4.5.0" {
			t.Fatalf("concurrent fetch = %q, want 4.5.0", got)
		}
	case <-time.After(10 * time.Second):
		t.Error("concurrent fetch blocked behind an in-flight probe")
	}

	release()
	if got := <-first; got != "4.5.0" {
		t.Fatalf("stalled fetch = %q, want 4.5.0", got)
	}
}

// Engines explain rejections in the error body (OOM, bad api key, model
// not found); poll failures surface in the ENGINES panel and must carry
// that text, not just the status code.
func TestHTTPErrorCarriesEngineBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "{\"error\":\"CUDA out of memory\"}\n")
	}))
	defer srv.Close()

	err := getJSON(context.Background(), srv.URL+"/api/ps", &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Fatalf("getJSON err = %v, want status plus body snippet", err)
	}

	_, err = getText(context.Background(), httpClient, srv.URL+"/metrics")
	if err == nil || !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Fatalf("getText err = %v, want status plus body snippet", err)
	}
}

// Client.Do returns the last response along with a redirect error; that body
// must be closed or the connection stays checked out of the pool.
func TestGetJSONClosesBodyOnRedirectError(t *testing.T) {
	var live atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			live.Add(1)
		case http.StateClosed, http.StateHijacked:
			live.Add(-1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	if err := getJSON(context.Background(), srv.URL+"/", &struct{}{}); err == nil {
		t.Fatal("redirect loop must fail")
	}
	deadline := time.Now().Add(2 * time.Second)
	for live.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := live.Load(); n > 0 {
		t.Fatalf("redirect error left %d connection(s) open", n)
	}
}

// A model id is engine-chosen data. It reaches the dashboard, the probe
// request body and the --json report, so every listing path must trim,
// sanitize and cap it before it becomes a ModelInfo.
func TestPollBoundsEngineSuppliedModelNames(t *testing.T) {
	const esc = "\x1b"
	const bel = "\a"
	oversized := strings.Repeat("z", core.ModelNameMax*3)
	// The listings are marshaled rather than written as literal JSON: a raw
	// control byte inside a JSON string does not decode, and the poll would
	// fail before any name reached the cap.
	listing, err := json.Marshal(map[string]any{"data": []map[string]any{
		{"id": esc + "]52;c;YU9UQw==" + bel + "llama3", "context_length": 4096},
		{"id": oversized, "context_length": 4096},
		{"id": esc + "[31m" + esc + "[0m", "context_length": 4096},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := json.Marshal(map[string]any{"models": []map[string]any{
		{"name": " " + oversized + " ", "size_vram": 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write(listing)
		case "/api/ps":
			w.Write(ps)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m, err := NewOpenAICompat(srv.URL, "localai", core.KindLocalAI).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 2 {
		t.Fatalf("models = %+v, want 2: an id of pure escapes names no model", m.Models)
	}
	if m.Models[0].Name != "llama3" {
		t.Errorf("model[0] = %q, want llama3 with the escape stripped", m.Models[0].Name)
	}
	if m.Models[1].Name != strings.Repeat("z", core.ModelNameMax) {
		t.Errorf("model[1] = %d chars, want %d", len(m.Models[1].Name), core.ModelNameMax)
	}

	o, err := NewOllama(srv.URL).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Models) != 1 || o.Models[0].Name != strings.Repeat("z", core.ModelNameMax) {
		t.Errorf("ollama models = %+v, want one id of %d chars", o.Models, core.ModelNameMax)
	}
}

// A version arrives from the engine, so it takes the same cap, sanitizer and
// one-line collapse as every other version producer. An engine answering a
// newline would otherwise buy itself a second row in the version readout.
func TestPollCapsTheLemonadeVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"version":"1.0\n2.0` + strings.Repeat("x", 4*versionCap) + `"}`))
		case "/metrics":
			w.Write([]byte(""))
		default:
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
		}
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "test", core.KindLemonade)
	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// The whitespace run collapses to one space, then the tail is cut to
	// versionCap clusters. Asserting the exact value also pins that the
	// version arrived at all, which the two checks below cannot.
	if want := "1.0 2.0" + strings.Repeat("x", versionCap-7); m.Version != want {
		t.Fatalf("version = %q (%d clusters), want %q", m.Version, len([]rune(m.Version)), want)
	}
	if strings.ContainsAny(m.Version, "\n\r") {
		t.Fatalf("version kept a line break: %q", m.Version)
	}
	if len([]rune(m.Version)) > versionCap {
		t.Fatalf("version kept %d clusters, cap is %d", len([]rune(m.Version)), versionCap)
	}
}

// A metrics body past the cap is refused, not truncated: the partial
// exposition parses into counters below the last good ones, and the
// collector reads that fall as a restart, losing the tail for good.
func TestGetTextRefusesAnOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), textCap+1))
	}))
	defer srv.Close()

	if _, err := getText(context.Background(), httpClient, srv.URL); err == nil {
		t.Fatal("oversized body accepted as a complete scrape")
	}
}

func TestGetTextAcceptsABodyAtTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), textCap))
	}))
	defer srv.Close()

	b, err := getText(context.Background(), httpClient, srv.URL)
	if err != nil {
		t.Fatalf("getText at the cap: %v", err)
	}
	if len(b) != textCap {
		t.Fatalf("read %d bytes, want %d", len(b), textCap)
	}
}

// A discovery probe is a whole-word match, and FoldASCII folds ASCII A-Z
// only. A non-ASCII letter is therefore still a letter in the body, and
// reading its bytes as a word break lets "йeep" answer for the word "eep":
// any engine could then advertise a capability it does not have by
// embedding the needle in a different word.
func TestProbeContainsWordTreatsForeignLettersAsWord(t *testing.T) {
	cases := []struct {
		body, needle string
		want         bool
	}{
		{"llama-server is running", "llama-server", true},
		{"vllm engine ready", "vllm", true},
		{"не-vllm here", "vllm", true}, // the hyphen is a real boundary
		{"йeep-alive", "eep", false},
		{"йeep-alive", "йeep", true},
		{"vllm", "vllm", true},
		{"2vllm", "vllm", false},
		{"vllm2", "vllm", false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(c.body))
		}))
		got := probeContainsWord(context.Background(), srv.URL, "/probe", c.needle)
		srv.Close()
		if got != c.want {
			t.Errorf("probeContainsWord(%q, %q) = %v, want %v", c.body, c.needle, got, c.want)
		}
	}
}
