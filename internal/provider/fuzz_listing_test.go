package provider

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// fuzzBodyCap bounds one served body. getJSON reads 4 MiB and getText 8 MiB,
// so a body well past those only exercises the caps at copy cost.
const fuzzBodyCap = 1 << 20

// FuzzPollModelListings drives every engine poll against one server that
// answers every path with the fuzzed body. A localhost port that answers is
// untrusted: discovery probes dozens of them, and whatever sits there
// decides which models are listed, how much context they claim, and which
// version string reaches the readout. A decoded listing must therefore carry
// only named models with sane context lengths, keep every derived number
// finite and non-negative, and answer the same body identically twice.
func FuzzPollModelListings(f *testing.F) {
	for _, seed := range []string{
		`{"data":[{"id":"auto/big","context_length":200000}]}`,
		`{"data":[{"id":""}]}`,
		`{"data":[{"id":"m","context_length":-1,"max_context_length":-1}]}`,
		`{"data":[{"id":"m","context_length":9223372036854775807,"max_context_length":-9223372036854775808}]}`,
		`{"data":[{"id":"m","context_length":1e400}]}`,
		`{"data":[{"id":"m","context_length":"200000"}]}`,
		`{"data":"notalist"}`,
		`{"data":null}`,
		`{}`,
		`{"models":[{"name":"llama3:latest","size_vram":8000000000},{"model":"qwen2:7b","size_vram":-1}]}`,
		`{"models":[{"name":"","model":""}]}`,
		`{"version":"0.5.4","all_models_loaded":[{"model_name":"m","ctx_size":1e308}]}`,
		`{"version":"","model_loaded":"m"}`,
		`{"status":"ok","version":9}`,
		`{"data":[{"id":"m","type":"llm","state":"loaded","max_context_length":-4096}]}`,
		`{"data":[{"id":"m","type":"embeddings","state":"loaded","max_context_length":4096}]}`,
		`{"data":[{"id":"m","type":"","state":"","max_context_length":4096}]}`,
		"[]",
		"null",
		"",
		"{",
		"\x00\xff",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > fuzzBodyCap {
			t.Skip()
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		polls := []Provider{
			NewOpenAICompat(srv.URL, "fuzz-vllm", core.KindVLLM),
			NewOpenAICompat(srv.URL, "fuzz-lmstudio", core.KindLMStudio),
			NewOpenAICompat(srv.URL, "fuzz-lemonade", core.KindLemonade),
			NewOllama(srv.URL),
		}
		for _, p := range polls {
			m, err := p.Poll(ctx)
			if err != nil || m == nil {
				continue // a body that decodes as nothing is not a listing
			}
			for i, mi := range m.Models {
				if mi.Name == "" {
					t.Fatalf("%s: model %d has an empty name: %+v", p.Label, i, m.Models)
				}
				if mi.CtxMax > math.MaxInt64 {
					t.Fatalf("%s: model %q claims CtxMax %d, past the int64 the readout renders", p.Label, mi.Name, mi.CtxMax)
				}
			}
			if m.HasKV && (m.KVPct < 0 || m.KVPct > 100) {
				t.Fatalf("%s: HasKV with KVPct %v", p.Label, m.KVPct)
			}
			if m.Running < 0 || m.Waiting < 0 {
				t.Fatalf("%s: queue depths went negative: running=%d waiting=%d", p.Label, m.Running, m.Waiting)
			}
			for name, v := range map[string]float64{
				"OutTotal": m.OutTotal, "InTotal": m.InTotal,
				"TTFTms": m.TTFTms, "DirectOutPS": m.DirectOutPS,
			} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					t.Fatalf("%s: %s = %v", p.Label, name, v)
				}
			}
			if again, err2 := p.Poll(ctx); err2 == nil && again != nil {
				if len(again.Models) != len(m.Models) || again.Version != m.Version {
					t.Fatalf("%s: not deterministic: %+v then %+v", p.Label, m, again)
				}
			}
		}
	})
}
