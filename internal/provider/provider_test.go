package provider

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	fam := parseProm(vllmFixture)
	if got := fam["vllm:prompt_tokens_total"]; got != 1000 {
		t.Fatalf("prompt_tokens_total = %v, want 1000", got)
	}
	if _, ok := fam["vllm:num_requests_running"]; !ok {
		t.Fatal("labels were not stripped from family names")
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
	if v := fam["gen_total"]; math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("overflowing family sum = %v, want the family dropped", v)
	}
	var m Metrics
	classify(parseProm("vllm:generation_tokens_total{a=\"1\"} 1e308\n"+
		"vllm:generation_tokens_total{a=\"2\"} 1e308\n"), &m)
	if math.IsInf(m.OutTotal, 0) || math.IsNaN(m.OutTotal) {
		t.Fatalf("OutTotal = %v from an overflowing family, want the last finite sum", m.OutTotal)
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
	if !m.HasKV || m.KVPct < 61.9 || m.KVPct > 62.1 {
		t.Errorf("kv pct = %v hasKV=%v", m.KVPct, m.HasKV)
	}
	if want := 4.0 / 20.0 * 1000; m.TTFTms != want {
		t.Errorf("ttft ms = %v, want %v", m.TTFTms, want)
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

func TestSatCoercions(t *testing.T) {
	if got := satInt(2.7); got != 2 {
		t.Errorf("satInt(2.7) = %d, want 2", got)
	}
	if got := satUint(8192); got != 8192 {
		t.Errorf("satUint(8192) = %d", got)
	}
	for _, v := range []float64{0, -5} {
		if got := satInt(v); got != 0 {
			t.Errorf("satInt(%v) = %d, want 0", v, got)
		}
		if got := satUint(v); got != 0 {
			t.Errorf("satUint(%v) = %d, want 0", v, got)
		}
	}
	if satInt(1e300) != math.MaxInt || satUint(1e300) != math.MaxUint64 {
		t.Error("huge magnitudes must saturate, not wrap")
	}
}

func TestSplitMetric(t *testing.T) {
	name, val, ok := splitMetric(`a:b_c{label="x,y z"} 1.5e2`)
	if !ok || name != "a:b_c" || val != 150 {
		t.Fatalf("got %q %v %v", name, val, ok)
	}
	if _, _, ok := splitMetric("garbage"); ok {
		t.Fatal("valueless line parsed")
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
	}
	for body, want := range cases {
		if got := extractVersionField(body); got != want {
			t.Errorf("extractVersionField(%q) = %q, want %q", body, got, want)
		}
	}
}

// A version probe fired while the engine is still starting must not be
// cached as a permanent miss: later polls retry after versionRetry, and
// the first success is memoized so healthy engines are asked only once.
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

	vc.mu.Lock()
	vc.at = time.Now().Add(-versionRetry - time.Second)
	vc.mu.Unlock()
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
