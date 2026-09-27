package provider

import (
	"context"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// Ollama monitors an Ollama daemon via /api/ps. Ollama publishes no token
// counters over HTTP, so throughput comes from probes or agent events.
type Ollama struct {
	base    string
	version versionCache
}

func NewOllama(base string) Provider {
	o := &Ollama{base: strings.TrimRight(base, "/")}
	return Provider{Label: "ollama", Addr: o.base, Kind: core.KindOllama, Poll: o.poll}
}

func (o *Ollama) poll(ctx context.Context) (*Metrics, error) {
	m := &Metrics{}
	// expires_at and size are not decoded: nothing reads either, and a field
	// a daemon (or a proxy in front of one) spells its own way fails the whole
	// decode, losing the model list and the version with it.
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			Model    string `json:"model"`
			SizeVRAM uint64 `json:"size_vram"`
		} `json:"models"`
	}
	if err := getJSON(ctx, o.base+"/api/ps", &ps); err != nil {
		return nil, err
	}
	for _, mm := range ps.Models {
		name := mm.Name
		if name == "" {
			name = mm.Model
		}
		if name == "" {
			continue
		}
		m.Models = append(m.Models, core.ModelInfo{Name: name, SizeVRAM: mm.SizeVRAM})
	}
	m.Version = o.version.fetch(ctx, o.base)
	return m, nil
}
