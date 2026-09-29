package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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

var scanClient = &http.Client{Timeout: scanTimeout, CheckRedirect: bearer.CheckRedirect}

// decodeScanJSON decodes a scan response under the same cap every other
// engine body in this package gets. Discovery talks to whatever is listening
// on a well-known port, so a body is untrusted by construction: without the
// cap a process that answers one of these ports with an endless JSON document
// grows the decoder until the dashboard is killed, and the ports it can
// squat are the ones an engine would have used.
func decodeScanJSON(resp *http.Response, out any) bool {
	return json.NewDecoder(io.LimitReader(resp.Body, jsonBodyMax)).Decode(out) == nil
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
// Order matters. OmniRoute and Ollama go first because both serve the OpenAI
// surface too, so the later OpenAI probing would claim either: OmniRoute is
// settled by a single response header, Ollama by a GET of /api/ps carrying a
// models member. Then the specific metrics prefixes, then engine-specific
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
	// Triton Inference Server answers the KServe v2 readiness endpoint.
	if healthOK(ctx, base, "/v2/health/ready") {
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
	// TGI matches /info on either needle, so the two cases are one fetch each;
	// probeContains ANDs its needles. /readyz costs the same single probe:
	// probeContainsWord differs in how it compares, not in how many fetches
	// it makes.
	case probeContains(ctx, base, "/info", `"version"`),
		probeContains(ctx, base, "/info", "text-generation"):
		return core.KindTGI
	case probeContainsWord(ctx, base, "/readyz", "ok"):
		return core.KindLocalAI
	case probeContains(ctx, base, "/", "gpustack"):
		return core.KindGPUStack
	case healthOK(ctx, base, "/health"):
		return core.KindLlamaCPP // llama.cpp /health answers {"status":"ok"}
	default:
		return core.KindOpenAI
	}
}

// probeContains fetches a path and checks that every needle appears. The
// body is an engine's own, and the needles are ASCII literals, so both sides
// fold with core.FoldASCII: strings.ToLower also folds runes whose lowercase
// form is ASCII, which would let a body spell a needle out of U+0130 or
// U+212A and claim an engine identity the engine never served.
func probeContains(ctx context.Context, base, path string, needles ...string) bool {
	text, err := getText(ctx, scanClient, base+path)
	if err != nil {
		return false
	}
	lower := core.FoldASCII(text)
	for _, n := range needles {
		if !strings.Contains(lower, core.FoldASCII(n)) {
			return false
		}
	}
	return true
}

// probeContainsWord is probeContains for a bare word needle, which must stand
// on its own in the body. A two-letter needle like "ok" otherwise matches
// inside any larger word the engine happened to serve, and an engine identity
// is a far worse answer than no answer.
//
// The needle is folded alongside the body, the same rule probeContains states
// and follows. A needle left unfolded made the comparison case-sensitive on one
// side only, so a needle spelled "OK" or "Ok" matched nothing in a body that
// spells it "ok" and the engine went unidentified, which is the same failure
// in the other direction as the U+0130 match probeContains refuses to make.
func probeContainsWord(ctx context.Context, base, path, needle string) bool {
	text, err := getText(ctx, scanClient, base+path)
	if err != nil {
		return false
	}
	needle = core.FoldASCII(needle)
	lower := core.FoldASCII(text)
	for at := 0; at < len(lower); {
		i := strings.Index(lower[at:], needle)
		if i < 0 {
			return false
		}
		start, end := at+i, at+i+len(needle)
		at = end
		if (start == 0 || !isWordByteAt(lower, start-1)) && (end == len(lower) || !isWordByteAt(lower, end)) {
			return true
		}
	}
	return false
}

// isWordByteAt reports whether the character of s that ends at index i is a
// letter or a digit, the condition for a word boundary there. i is either a
// rune start or a continuation byte, so a byte at or above 0x80 is backed up
// to its own lead byte and decoded.
//
// FoldASCII folds ASCII A-Z only and leaves every non-ASCII letter as it is,
// so a body is not known to be ASCII here. Reading those bytes as boundaries
// makes a foreign letter a word break, and a body of "йeep" then contains the
// whole word "eep": an engine could advertise a capability by embedding the
// needle in a different word.
func isWordByteAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	if b := s[i]; b < utf8.RuneSelf {
		return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// sglangInfoOK detects SGLang via its native /get_model_info endpoint.
func sglangInfoOK(ctx context.Context, base string) bool {
	resp, err := get(ctx, scanClient, base+"/get_model_info")
	if err != nil {
		return false
	}
	defer bearer.DrainAndClose(resp.Body)
	var body struct {
		ModelPath string `json:"model_path"`
	}
	return decodeScanJSON(resp, &body) && body.ModelPath != ""
}

// idsLookMLX reports whether any served model id looks like an MLX build
// (mlx-community/…, …-mlx-q4 …).
func idsLookMLX(mr *modelsResp) bool {
	for _, d := range mr.Data {
		if strings.Contains(core.FoldASCII(d.ID), "mlx") {
			return true
		}
	}
	return false
}

// isOmniRoute detects the OmniRoute gateway by its distinctive routing
// header on the API surface. One GET of /v1/models is enough; no auth
// required (the header rides 401s too), so any status may carry it and
// get's 200-only rule does not apply here.
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
	bearer.DrainAndClose(resp.Body)
	return resp.Header.Get("X-OmniRoute-Route-Class") != ""
}

func isOllama(ctx context.Context, base string) bool {
	resp, err := get(ctx, scanClient, base+"/api/ps")
	if err != nil {
		return false
	}
	defer bearer.DrainAndClose(resp.Body)
	var body struct {
		Models []json.RawMessage `json:"models"`
	}
	return decodeScanJSON(resp, &body) && body.Models != nil
}

type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func getOpenAIModels(ctx context.Context, base string) *modelsResp {
	resp, err := get(ctx, scanClient, base+"/v1/models")
	if err != nil {
		return nil
	}
	defer bearer.DrainAndClose(resp.Body)
	var mr modelsResp
	// A well-formed empty listing is an answer, not a silence: an engine
	// serving no model yet still speaks the API, and the switch in identify
	// reaches every kind only on this branch. Absent "data" leaves the slice
	// nil, which is the shape of an endpoint that is not a listing.
	if !decodeScanJSON(resp, &mr) || mr.Data == nil {
		return nil
	}
	return &mr
}

// healthOK reports whether base's health path answers at all.
func healthOK(ctx context.Context, base, path string) bool {
	resp, err := get(ctx, scanClient, base+path)
	if err != nil {
		return false
	}
	bearer.DrainAndClose(resp.Body)
	return true
}
