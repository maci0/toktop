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

// NewOllama monitors the daemon at base, reachable over plain HTTP without a
// token.
func NewOllama(base string) Provider { return NewOllamaLabeled(base, "ollama") }

// NewOllamaLabeled is [NewOllama] under a label the caller chose. Discovery
// labels a local daemon "ollama" and cannot tell two of them apart, while a
// remote one shares a host with every other engine forwarded out of it and
// needs "host:port". Choosing the constructor by kind is this package's
// decision; the label is the caller's.
func NewOllamaLabeled(base, label string) Provider {
	o := &Ollama{base: strings.TrimRight(base, "/")}
	return Provider{Label: label, Addr: o.base, Kind: core.KindOllama, Poll: o.poll}
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
		name := core.ModelName(mm.Name)
		if name == "" {
			name = core.ModelName(mm.Model)
		}
		if name == "" {
			continue
		}
		m.Models = append(m.Models, core.ModelInfo{Name: name, SizeVRAM: mm.SizeVRAM})
	}
	m.Version = o.version.fetch(ctx, o.base)
	return m, nil
}
