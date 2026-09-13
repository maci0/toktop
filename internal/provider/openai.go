package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/maci0/toktop/internal/core"
)

// OpenAICompat scrapes any server exposing Prometheus /metrics plus
// /v1/models: vLLM, SGLang, llama.cpp-server, TGI, LocalAI, LiteLLM,
// GPUStack, Lemonade and anything else OpenAI-shaped.
type OpenAICompat struct {
	base    string
	label   string
	kind    string
	version versionCache
}

func NewOpenAICompat(base, label, kind string) Provider {
	o := &OpenAICompat{base: strings.TrimRight(base, "/"), label: label, kind: kind}
	return Provider{Label: o.label, Addr: o.base, Kind: o.kind, Poll: o.poll}
}

func (o *OpenAICompat) poll(ctx context.Context) (*Metrics, error) {
	m := &Metrics{}
	var lm struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			MaxContextLen int64  `json:"max_context_length"` // LM Studio shape
		} `json:"data"`
	}
	// /metrics and /v1/models are independent GETs against the same host;
	// waiting out one before starting the other doubles poll latency on
	// the collector's critical path (every backend, every interval).
	var (
		text      string
		merr      error
		modelsErr error
	)
	var wg sync.WaitGroup
	wg.Go(func() {
		text, merr = getText(ctx, httpClient, o.base+"/metrics")
	})
	wg.Go(func() {
		modelsErr = getJSON(ctx, o.base+"/v1/models", &lm)
	})
	wg.Wait()
	if merr != nil && o.kind == core.KindVLLM {
		return nil, merr // vLLM without metrics is not worth showing
	}
	if merr == nil {
		classify(parseProm(text), m)
	}
	haveModels := modelsErr == nil
	if haveModels {
		for _, d := range lm.Data {
			if d.ID != "" {
				mi := core.ModelInfo{Name: d.ID}
				if n := max(d.ContextLength, d.MaxContextLen); n > 0 {
					mi.CtxMax = uint64(n)
				}
				m.Models = append(m.Models, mi)
			}
		}
	}

	enriched := false
	switch o.kind {
	case core.KindLMStudio:
		enriched = enrichLMStudio(ctx, o.base, m)
	case core.KindLemonade:
		enriched = enrichLemonade(ctx, o.base, m)
	}
	// One of /metrics, /v1/models, or a native enrich endpoint is
	// enough; none of them answering is a down engine, not an idle one.
	if !haveModels && merr != nil && !enriched {
		return nil, fmt.Errorf("no known endpoints on %s: %w", o.base, merr)
	}
	if m.Version == "" {
		m.Version = o.version.fetch(ctx, o.base)
	}
	return m, nil
}

// enrichLMStudio pulls the native /api/v0/models feed: loaded LLMs and
// context lengths that the thin OpenAI listing lacks. Unloaded catalog
// entries are dropped so a probe cannot JIT-load a cold weight.
func enrichLMStudio(ctx context.Context, base string, m *Metrics) bool {
	var v0 struct {
		Data []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			State      string `json:"state"`
			MaxContext int64  `json:"max_context_length"`
		} `json:"data"`
	}
	if getJSON(ctx, base+"/api/v0/models", &v0) != nil {
		return false
	}
	m.Models = m.Models[:0]
	for _, d := range v0.Data {
		if d.Type != "" && !strings.EqualFold(d.Type, "llm") {
			continue // embeddings and friends are noise here
		}
		// JIT-loading an unloaded catalog entry on 'p' burns VRAM and
		// folds load time into TTFT. Missing state (older servers) stays.
		if d.State != "" && !strings.EqualFold(d.State, "loaded") {
			continue
		}
		mi := core.ModelInfo{Name: d.ID}
		if d.MaxContext > 0 {
			mi.CtxMax = uint64(d.MaxContext)
		}
		m.Models = append(m.Models, mi)
	}
	return true
}

// enrichLemonade reads /v1/health: version plus loaded-model inventory.
func enrichLemonade(ctx context.Context, base string, m *Metrics) bool {
	var h struct {
		Version     string `json:"version"`
		ModelLoaded string `json:"model_loaded"`
		AllLoaded   []struct {
			ModelName string  `json:"model_name"`
			CtxSize   float64 `json:"ctx_size"`
		} `json:"all_models_loaded"`
	}
	if getJSON(ctx, base+"/v1/health", &h) != nil {
		return false
	}
	if h.Version != "" {
		m.Version = h.Version
	}
	switch {
	case len(h.AllLoaded) > 0:
		m.Models = m.Models[:0]
		for _, mm := range h.AllLoaded {
			mi := core.ModelInfo{Name: mm.ModelName}
			if mm.CtxSize > 0 {
				mi.CtxMax = satUint(mm.CtxSize)
			}
			m.Models = append(m.Models, mi)
		}
	case h.ModelLoaded != "":
		m.Models = []core.ModelInfo{{Name: h.ModelLoaded}}
	}
	return true
}
