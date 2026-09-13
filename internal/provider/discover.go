package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
)

// scanTimeout bounds each identification request during discovery.
const scanTimeout = 700 * time.Millisecond

var defaultCandidates = []string{
	"http://127.0.0.1:11434", // ollama
	"http://127.0.0.1:30000", // sglang
	"http://127.0.0.1:8000",  // vllm / trtllm-serve / triton / lemonade (legacy)
	"http://127.0.0.1:13305", // lemonade server
	"http://127.0.0.1:8001",
	"http://127.0.0.1:8080", // llama.cpp server / mlx-lm / TGI / localai
	"http://127.0.0.1:8081",
	"http://127.0.0.1:1234",  // lm studio (metal/mlx models)
	"http://127.0.0.1:5001",  // koboldcpp
	"http://127.0.0.1:5000",  // tabbyapi
	"http://127.0.0.1:4000",  // litellm proxy
	"http://127.0.0.1:1337",  // jan
	"http://127.0.0.1:4891",  // gpt4all
	"http://127.0.0.1:7860",  // text-generation-webui
	"http://127.0.0.1:8790",  // prism proxy
	"http://127.0.0.1:20128", // omniroute gateway
	"http://127.0.0.1:80",    // gpustack
	"http://127.0.0.1:3000",
}

// CandidatePorts exposes the well-known engine ports (for remote probing).
func CandidatePorts() []int {
	seen := map[int]bool{}
	var out []int
	for _, base := range defaultCandidates {
		u, err := url.Parse(base)
		if err != nil {
			continue
		}
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

var scanClient = &http.Client{Timeout: scanTimeout}

// scanGet issues one identification GET through scanClient with the bearer
// token applied. The response body, when present, must be closed by the
// caller; err is nil only for HTTP 200.
func scanGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	bearer.Apply(req)
	resp, err := scanClient.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("http %s", resp.Status)
	}
	return resp, nil
}

func procCandidateURLs() []string {
	var urls []string
	for _, p := range procs.Snapshot() {
		if p.Engine == "" {
			continue
		}
		if port := p.ListenPort(); port > 0 {
			urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d", port))
		}
	}
	return urls
}

// Discover probes well-known ports plus any engine processes running locally,
// returning a provider for every backend that answers.
func Discover(ctx context.Context) []Provider {
	seen := map[string]bool{}
	var bases []string
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			bases = append(bases, u)
		}
	}
	for _, u := range procCandidateURLs() {
		add(u)
	}
	for _, base := range defaultCandidates {
		add(base)
	}
	return discoverBases(ctx, bases)
}

// IdentifyAll identifies every base concurrently and returns the kinds in
// input order, "" for bases that match no engine. Each probe cascades several
// requests with per-request timeouts; probing one at a time would let a
// single filtered port stall the caller by its full timeout chain.
func IdentifyAll(ctx context.Context, bases []string) []string {
	kinds := make([]string, len(bases))
	var wg sync.WaitGroup
	for i, base := range bases {
		wg.Go(func() {
			kinds[i] = identify(ctx, base)
		})
	}
	wg.Wait()
	return kinds
}

// discoverBases identifies every candidate concurrently and returns providers
// for those that answer, preserving candidate order.
func discoverBases(ctx context.Context, bases []string) []Provider {
	kinds := IdentifyAll(ctx, bases)

	var found []Provider
	for i, kind := range kinds {
		if kind != "" {
			found = append(found, newProvider(kind, bases[i]))
		}
	}
	return found
}

// Attach builds a provider for an explicit URL, identifying its kind first.
func Attach(ctx context.Context, base string) Provider {
	if kind := identify(ctx, base); kind != "" {
		return newProvider(kind, base)
	}
	return Provider{}
}

func newProvider(kind, base string) Provider {
	if kind == core.KindOllama {
		return NewOllama(base)
	}
	return NewOpenAICompat(base, kind, kind)
}

// identify returns the provider kind serving base, or "" if none matches.
// Order matters: specific metrics prefixes first, then engine-specific
// endpoints, then generic OpenAI probing.
func identify(ctx context.Context, base string) string {
	if isOmniRoute(ctx, base) {
		return core.KindOmniRoute
	}
	if isOllama(ctx, base) {
		return core.KindOllama
	}
	if text, err := getText(ctx, scanClient, base+"/metrics"); err == nil {
		switch {
		case strings.Contains(text, "vllm:"):
			return core.KindVLLM
		case strings.Contains(text, "sglang:"):
			return core.KindSGLang
		case strings.Contains(text, "nv_inference"), strings.Contains(text, "trtllm"):
			return core.KindTRTLLM
		}
	}
	if sglangInfoOK(ctx, base) {
		return core.KindSGLang
	}
	if tritonReady(ctx, base) {
		return core.KindTRTLLM // Triton Inference Server (typical TRT-LLM host)
	}
	if probeContains(ctx, base, "/v1/health", `"version"`, `"status"`) ||
		probeContains(ctx, base, "/api/v1/models", `"data"`) {
		return core.KindLemonade
	}
	models := getOpenAIModels(ctx, base)
	switch {
	case models == nil:
		// engines whose OpenAI listing needs auth or lives on another path
		if probeContains(ctx, base, "/health/liveliness", "alive") { // vLLM answers "I'm alive!"
			return core.KindLiteLLM
		}
		return ""
	case idsLookMLX(models):
		return core.KindMLX // mlx-community models via mlx-lm or LM Studio
	case probeContains(ctx, base, "/api/v0/models", `"state"`):
		return core.KindLMStudio
	case probeContains(ctx, base, "/api/extra/version", "koboldcpp"):
		return core.KindKoboldCPP
	case probeContains(ctx, base, "/info", `"version"`),
		probeContains(ctx, base, "/info", "text-generation"):
		return core.KindTGI
	case probeContains(ctx, base, "/readyz", "OK"),
		probeContains(ctx, base, "/readyz", "ok"):
		return core.KindLocalAI
	case probeContains(ctx, base, "/", "gpustack"):
		return core.KindGPUStack
	case healthOK(ctx, base):
		return core.KindLlamaCPP // llama.cpp /health answers {"status":"ok"}
	default:
		return core.KindOpenAI
	}
}

// probeContains fetches a path and checks that every needle appears.
func probeContains(ctx context.Context, base, path string, needles ...string) bool {
	text, err := getText(ctx, scanClient, base+path)
	if err != nil {
		return false
	}
	lower := strings.ToLower(text)
	for _, n := range needles {
		if !strings.Contains(lower, strings.ToLower(n)) {
			return false
		}
	}
	return true
}

// sglangInfoOK detects SGLang via its native /get_model_info endpoint.
func sglangInfoOK(ctx context.Context, base string) bool {
	resp, err := scanGet(ctx, base+"/get_model_info")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		ModelPath string `json:"model_path"`
	}
	return json.NewDecoder(resp.Body).Decode(&body) == nil && body.ModelPath != ""
}

// tritonReady checks the KServe v2 readiness endpoint served by Triton.
func tritonReady(ctx context.Context, base string) bool {
	resp, err := scanGet(ctx, base+"/v2/health/ready")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// idsLookMLX reports whether any served model id looks like an MLX build
// (mlx-community/…, …-mlx-q4 …).
func idsLookMLX(mr *modelsResp) bool {
	for _, d := range mr.Data {
		if strings.Contains(strings.ToLower(d.ID), "mlx") {
			return true
		}
	}
	return false
}

// isOmniRoute detects the OmniRoute gateway by its distinctive routing
// header on the API surface. One GET of /v1/models is enough; no auth
// required (the header rides 401s too), so any status may carry it and
// scanGet's 200-only rule does not apply here.
func isOmniRoute(ctx context.Context, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return false
	}
	bearer.Apply(req)
	resp, err := scanClient.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	resp.Body.Close()
	return resp.Header.Get("X-OmniRoute-Route-Class") != ""
}

func isOllama(ctx context.Context, base string) bool {
	resp, err := scanGet(ctx, base+"/api/ps")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		Models []json.RawMessage `json:"models"`
	}
	return json.NewDecoder(resp.Body).Decode(&body) == nil && body.Models != nil
}

type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func getOpenAIModels(ctx context.Context, base string) *modelsResp {
	resp, err := scanGet(ctx, base+"/v1/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var mr modelsResp
	if json.NewDecoder(resp.Body).Decode(&mr) != nil || len(mr.Data) == 0 {
		return nil
	}
	return &mr
}

func healthOK(ctx context.Context, base string) bool {
	resp, err := scanGet(ctx, base+"/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
