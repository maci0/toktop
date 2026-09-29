package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maci0/toktop/internal/core"
)

const sglangFixture = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs 4.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs 9.0
# TYPE sglang:token_usage gauge
sglang:token_usage 0.71
# TYPE sglang:gen_throughput gauge
sglang:gen_throughput 512.5
# TYPE sglang:prompt_tokens_total counter
sglang:prompt_tokens_total 100.0
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total 200.0
`

func TestClassifySGLang(t *testing.T) {
	var m Metrics
	classify(parseProm(sglangFixture), &m)
	if m.Running != 4 || m.Waiting != 9 {
		t.Errorf("queues = run:%d wait:%d", m.Running, m.Waiting)
	}
	if !m.HasKV || m.KVPct != 71 {
		t.Errorf("token_usage -> kv = %v hasKV=%v", m.KVPct, m.HasKV)
	}
	if m.DirectOutPS != 512.5 {
		t.Errorf("direct throughput = %v", m.DirectOutPS)
	}
	// The collector branches on the flag, not the value: clearing it here
	// would leave every engine-reported tok/s silently unreported.
	if !m.HasDirectOutPS {
		t.Errorf("HasDirectOutPS = false with DirectOutPS = %v; the collector would not show it", m.DirectOutPS)
	}
	if m.InTotal != 100 || m.OutTotal != 200 {
		t.Errorf("totals = %v/%v", m.InTotal, m.OutTotal)
	}
}

// Queue-matching must not mistake latency histograms for request counts.
func TestClassifyQueueIgnoresDurations(t *testing.T) {
	var m Metrics
	classify(map[string]float64{
		"request_queue_duration_seconds_sum": 12.0,
		"time_to_first_token_seconds":        3.0,
	}, &m)
	if m.Waiting != 0 {
		t.Errorf("duration histogram leaked into waiting: %d", m.Waiting)
	}
	// A throughput family with no generation or decode word in its name is a
	// prompt rate, and the collector must not present it as output tok/s.
	var tp Metrics
	classify(parseProm("# HELP x request_throughput 88.0\n# TYPE x request_throughput gauge\nx:request_throughput 88.0\n"), &tp)
	if tp.HasDirectOutPS || tp.DirectOutPS != 0 {
		t.Errorf("request_throughput set DirectOutPS = %v has=%v, want no output rate", tp.DirectOutPS, tp.HasDirectOutPS)
	}
}

const tritonFixture = `# HELP nv_inference_request_success Number of successful requests.
nv_inference_request_success 500
# TYPE nv_inference_pending_request_count gauge
nv_inference_pending_request_count{model="trt_llm"} 6
`

func TestClassifyTriton(t *testing.T) {
	var m Metrics
	classify(parseProm(tritonFixture), &m)
	if m.Waiting != 6 {
		t.Errorf("pending = %d, want 6", m.Waiting)
	}
}

func httptestKind(t *testing.T, routes map[string]fakeRoute) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route, ok := routes[r.URL.Path]; ok {
			w.WriteHeader(route.code)
			w.Write([]byte(route.body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return identify(context.Background(), srv.URL)
}

type fakeRoute struct {
	code int
	body string
}

func TestIdentifySGLangViaMetrics(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/metrics": {200, "sglang:gen_throughput 1\n"},
	})
	if kind != core.KindSGLang {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifySGLangViaModelInfo(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/get_model_info": {200, `{"model_path":"/models/qwen"}`},
	})
	if kind != core.KindSGLang {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyTriton(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v2/health/ready": {200, "{}"},
	})
	if kind != core.KindTRTLLM {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyTRTLLMMetricsPrefix(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/metrics": {200, "trtllm_requests_total 5\n"},
	})
	if kind != core.KindTRTLLM {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyMLXModels(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"data":[{"id":"mlx-community/Llama-3.2-3B-Instruct-4bit"}]}`},
	})
	if kind != core.KindMLX {
		t.Errorf("kind = %q", kind)
	}
}

// An engine serving no model yet still speaks the API: the empty listing is
// an answer, and dropping it as a silence loses the engine from discovery
// while the poll layer reports it up.
func TestIdentifyEmptyModelListing(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"object":"list","data":[]}`},
	})
	if kind != core.KindOpenAI {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyLlamaCppStillWinsWithPlainHealth(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/health":    {200, `{"status":"ok"}`},
		"/v1/models": {200, `{"data":[{"id":"tinyllama"}]}`},
	})
	if kind != core.KindLlamaCPP {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyNothing(t *testing.T) {
	if got := httptestKind(t, nil); got != "" {
		t.Errorf("expected empty kind, got %q", got)
	}
	if !strings.EqualFold(core.KindTRTLLM, "trt-llm") {
		t.Error("trt-llm constant drift")
	}
}

func TestIdentifyLMStudio(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models":     {200, `{"data":[{"id":"qwen"}]}`},
		"/api/v0/models": {200, `{"data":[{"id":"qwen","type":"llm","state":"loaded","max_context_length":4096}]}`},
	})
	if kind != core.KindLMStudio {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyKoboldCPP(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models":         {200, `{"data":[{"id":"koboldcpp_model"}]}`},
		"/api/extra/version": {200, `{"result":"ok","version":"1.80.2","koboldcpp_version":"1.80.2"}`},
	})
	if kind != core.KindKoboldCPP {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyTGI(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"data":[{"id":"gpt2"}]}`},
		"/info":      {200, `{"model_id":"gpt2","version":"2.0.4"}`},
	})
	if kind != core.KindTGI {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyLocalAI(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"data":[{"id":"luna-ai"}]}`},
		"/readyz":    {200, "OK"},
	})
	if kind != core.KindLocalAI {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyLocalAINeedleIsAWholeWord(t *testing.T) {
	// A /readyz body that merely contains "ok" inside a longer word names no
	// engine. Falling through here lands on the OpenAI default, which is the
	// answer a plain OpenAI-shaped listing deserves.
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"data":[{"id":"luna-ai"}]}`},
		"/readyz":    {200, "BROKEN"},
	})
	if kind != core.KindOpenAI {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyLiteLLM(t *testing.T) {
	// litellm /v1/models usually needs an API key -> 401
	kind := httptestKind(t, map[string]fakeRoute{
		"/health/liveliness": {200, "I'm alive!"},
	})
	if kind != core.KindLiteLLM {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyGPUStack(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {200, `{"data":[{"id":"llama3"}]}`},
		"/":          {200, "<html><title>GPUStack</title></html>"},
	})
	if kind != core.KindGPUStack {
		t.Errorf("kind = %q", kind)
	}
}

func TestIdentifyLemonade(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/health": {200, `{"status":"ok","version":"9.3.3","model_loaded":"Llama-3.2-1B"}`},
	})
	if kind != core.KindLemonade {
		t.Errorf("kind = %q", kind)
	}
}

// sglangAndPlainURLs starts one identifiable sglang engine and one server
// that answers nothing recognizable anywhere, returning both URLs.
func sglangAndPlainURLs(t *testing.T) (engine, plain string) {
	t.Helper()
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Write([]byte("sglang:gen_throughput 1\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(engineSrv.Close)
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(plainSrv.Close)
	return engineSrv.URL, plainSrv.URL
}

// Attach is the --add entry point: identify an explicit URL and wrap it in
// a provider, or refuse (nil) when nothing recognizable answers.
func TestAttachIdentifiesOrRefuses(t *testing.T) {
	engine, plain := sglangAndPlainURLs(t)

	p := Attach(context.Background(), engine)
	if p.Poll == nil || p.Addr != engine || p.Label != core.KindSGLang {
		t.Fatalf("Attach = %+v, want sglang provider at %s", p, engine)
	}
	if p := Attach(context.Background(), plain); p.Poll != nil {
		t.Fatalf("unidentifiable server must not attach, got %+v", p)
	}
}

// discoverBases must identify candidates concurrently yet return providers
// in candidate order, skipping non-engines.
func TestDiscoverBasesParallelKeepsOrder(t *testing.T) {
	engine, plain := sglangAndPlainURLs(t)

	found := discoverBases(context.Background(), []string{plain, engine, plain})
	if len(found) != 1 {
		t.Fatalf("providers = %d, want 1", len(found))
	}
	// newProvider labels non-ollama engines with their kind
	if found[0].Addr != engine || found[0].Label != core.KindSGLang {
		t.Fatalf("got %s (%s), want engine %s as %s",
			found[0].Addr, found[0].Label, engine, core.KindSGLang)
	}
}

func TestIdentifyOmniRoute401(t *testing.T) {
	kind := httptestKind(t, map[string]fakeRoute{
		"/v1/models": {http.StatusUnauthorized, `{"error":"unauthorized"}`},
	})
	if kind != "" {
		t.Errorf("kind = %q, want empty", kind)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OmniRoute-Route-Class", "CLIENT_API")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	if kind := identify(context.Background(), srv.URL); kind != core.KindOmniRoute {
		t.Errorf("identify on 401 with OmniRoute header = %q, want %q", kind, core.KindOmniRoute)
	}
}

func TestDiscover(t *testing.T) {
	engine, plain := sglangAndPlainURLs(t)
	orig := defaultCandidates
	defaultCandidates = []string{plain, engine, plain}
	defer func() { defaultCandidates = orig }()

	// Discover also walks the host process table, so the candidates are the
	// only part of the result this test controls.
	var ours []Provider
	for _, p := range Discover(context.Background()) {
		if p.Addr == engine || p.Addr == plain {
			ours = append(ours, p)
		}
	}
	if len(ours) != 1 {
		t.Fatalf("Discover() returned %d of our candidates, want 1", len(ours))
	}
	if ours[0].Addr != engine || ours[0].Label != core.KindSGLang {
		t.Fatalf("Discover() provider = %+v, want %s as %s", ours[0], engine, core.KindSGLang)
	}
}

// A process squatting on a well-known engine port answers the scan with a
// body it controls, so the identification decoders read untrusted bytes. A
// well-formed body past the cap must not be decoded: it is a scan answer that
// grew without bound, and the process it would have identified is whatever
// sent it.
func TestScanDecodersRefuseOversizedBody(t *testing.T) {
	pad := strings.Repeat("a", jsonBodyMax)
	for path, body := range map[string]string{
		"/get_model_info": `{"model_path":"/m","pad":"` + pad + `"}`,
		"/v1/models":      `{"data":[{"id":"mlx-community/x"}],"pad":"` + pad + `"}`,
		"/api/ps":         `{"models":[{"name":"llama3"}],"pad":"` + pad + `"}`,
	} {
		if len(body) <= jsonBodyMax {
			t.Fatalf("fixture for %s is %d bytes, not past the %d cap", path, len(body), jsonBodyMax)
		}
		if kind := httptestKind(t, map[string]fakeRoute{path: {200, body}}); kind != "" {
			t.Errorf("identify on an oversized %s body = %q, want no identification", path, kind)
		}
	}
}
