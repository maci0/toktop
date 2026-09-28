// Package core defines the shared data model flowing from collectors to the UI.
//
// The rest of the package is the set of helpers more than one of those
// packages needs and none of them should own: redaction and sanitizing of
// text that reaches a screen, a log or a file name, tilde and $HOME path
// expansion, the saturating conversions vendor telemetry arrives as, and the
// short spelling of a working directory the dashboard prints. Each of those
// files says in its own doc comment which packages it is shared with, and why
// it is here rather than there. What is not here is anything that polls,
// opens a connection or renders: core holds what the collectors produce and
// the vocabulary around it, so a package that does I/O can depend on it
// without the reverse.
package core

import (
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

const HistoryLen = 180 // rolling samples per provider (~3 min at 1s poll)

// Rolling history caps for snapshot payloads.
const (
	// AgentRateWindow is how far back per-agent tok/s looks. Long enough
	// that a pause between turns does not read as a stall, short enough to
	// track a real change.
	AgentRateWindow = 30 * time.Second
	// AgentHistoryLen is the retained event cap. --agents emits at most one
	// event per agent per second, so a cap smaller than (agents ×
	// AgentRateWindow) lets busy agents evict quieter ones before that
	// window elapses. 512 covers ~15 concurrent agents at 1 Hz for 30s;
	// ingest floods still cannot grow without bound.
	AgentHistoryLen = 512
	ProbeHistoryLen = 128 // probe samples kept per snapshot
)

// Provider kinds.
const (
	KindOllama    = "ollama"
	KindVLLM      = "vllm"
	KindLlamaCPP  = "llama.cpp" // llama-server, llamafile, ramalama
	KindOpenAI    = "openai"    // generic openai-compatible
	KindSGLang    = "sglang"
	KindTRTLLM    = "trt-llm" // TensorRT-LLM via trtllm-serve or Triton
	KindMLX       = "mlx"     // mlx-lm / LM Studio serving Metal models
	KindLMStudio  = "lmstudio"
	KindKoboldCPP = "koboldcpp"
	KindLocalAI   = "localai"
	KindTGI       = "tgi"
	KindLiteLLM   = "litellm"
	KindGPUStack  = "gpustack"
	KindLemonade  = "lemonade"   // AMD Ryzen AI server
	KindOmniRoute = "omnirouter" // OmniRoute local AI gateway (port 20128)
)

// ModelInfo is one model an engine reports it holds. The sizes are whatever
// the engine published: zero means the engine did not say, which is not the
// same as an empty model.
type ModelInfo struct {
	Name     string
	SizeVRAM uint64
	CtxMax   uint64
}

// ProviderSnapshot is one observation of an inference backend.
type ProviderSnapshot struct {
	Label   string
	Kind    string
	Addr    string
	OK      bool
	Err     string
	Version string // engine version, best effort

	PID     int     // serving process, when found locally
	ProcRSS uint64  // resident memory of that process
	ProcCPU float64 // percent of one core

	Models []ModelInfo

	OutTokPS float64
	InTokPS  float64
	Running  int
	Waiting  int
	KVPct    float64 // kv cache usage, 0..100
	TTFTms   float64 // engine-reported avg ttft if available

	OutHist []float64
	InHist  []float64
	// OutStamps/InStamps carry the instant each history sample was taken, oldest
	// first and the same length as the history they belong to. Samples are
	// not evenly spaced: a scrape can take up to the poll timeout, a
	// coalesced tick can push two samples at once, and an engine can join
	// late. Deriving a time from the sample's index and the collector
	// cadence would invent a spacing that never happened and skew every
	// chart that shared the axis, so the stamps travel with the values.
	OutStamps []time.Time
	InStamps  []time.Time
}

// MaxEventTokens bounds one token count on one AgentEvent. Real usage never
// approaches it; a sender claiming more is lying or broken, and every
// producer of an event (the ingest endpoint, a transcript reader) clamps to
// the same ceiling, so a value above it is refused rather than kept: the
// retained window's totals are saturated, so an unclamped MaxInt64 would
// survive into Tokens and report a rate billions of times too high. One
// constant, because two producers drifting apart would let a value in that
// the other would have refused.
const MaxEventTokens = 1 << 40

// MaxEventSpan bounds the model-reported duration on one AgentEvent. Span is
// the denominator of the rate core.SummarizeAgents prefers over the gap
// between events, so an unclamped value from a sender does not add tokens
// twice: it divides them by a duration that never happened and reports a rate
// orders of magnitude too low. A turn is bounded by a day; anything past that
// is a sender reporting its own uptime, not one turn.
const MaxEventSpan = 24 * time.Hour

// ClampEventSpan bounds one event's reported duration to [0, MaxEventSpan].
// The ingest endpoint is the only producer of a Span from the wire, and it
// routes through here so the bound has one definition.
func ClampEventSpan(d time.Duration) time.Duration {
	if d < 0 || d > MaxEventSpan {
		return 0
	}
	return d
}

// ClampEventTokens bounds one event's token count to MaxEventTokens, dropping
// anything outside [0, MaxEventTokens] to zero. Both producers of an agent
// event (the ingest endpoint, the local process watcher) route through it, so
// neither can drift into accepting a count the other would refuse.
func ClampEventTokens(n int64) int64 {
	if n < 0 || n > MaxEventTokens {
		return 0
	}
	return n
}

// Agent event kinds. Unknown values are accepted on the wire (forward
// compatible with a harness that invents one) and render as a generic event.
const (
	AgentKindTurn  = "turn"
	AgentKindTool  = "tool"
	AgentKindError = "error"
	AgentKindNote  = "note"
)

// AgentRecorder is the sink for agent events (ingest HTTP and --agents).
// RecordAgent reports whether the event reached the retained feed: false
// means it was not kept, either because the id was already recorded inside
// the recorder's dedup window or because it sorted behind the whole retained
// window, so a sender retrying a POST whose response was lost is told its
// replay stored nothing and an event too old to display is not counted as
// stored either.
type AgentRecorder interface {
	RecordAgent(ev AgentEvent) bool
}

// AgentEvent is a token-usage event pushed by an agent or harness. The HTTP
// wire shape is defined separately by ingest's agentEventWire.
type AgentEvent struct {
	At             time.Time
	ID             string // caller-chosen; a repeat still inside the dedup window is ignored
	Agent          string
	Model          string
	Kind           string // AgentKindTurn/Tool/Error/Note, or a sanitized unknown
	PromptTokens   int64
	OutputTokens   int64
	ThinkingTokens int64  // reasoning share of OutputTokens, when the agent says so
	ViaEngine      string // monitored engine already counting this output; aggregates skip it
	Note           string
	// Span is how long the model spent producing this event's tokens, when
	// the sender knows it. Zero means the rate is the gap between events.
	Span time.Duration
}

// HasAgentID reports whether events already contain this id. The empty string
// never matches, so events without an id are not treated as duplicates of
// each other. IDs are compared under NFC normalization.
func HasAgentID(events []AgentEvent, id string) bool {
	if id == "" {
		return false
	}
	id = norm.NFC.String(id)
	return slices.ContainsFunc(events, func(e AgentEvent) bool { return norm.NFC.String(e.ID) == id })
}

// AgentCmp compares two AgentEvents for newest-last ordering. Time is the
// primary key; equal timestamps then order by Agent, ID, and Note so
// concurrent ingest cannot shuffle a replay.
func AgentCmp(a, b AgentEvent) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if c := strings.Compare(norm.NFC.String(a.Agent), norm.NFC.String(b.Agent)); c != 0 {
		return c
	}
	if c := strings.Compare(norm.NFC.String(a.ID), norm.NFC.String(b.ID)); c != 0 {
		return c
	}
	return strings.Compare(norm.NFC.String(a.Note), norm.NFC.String(b.Note))
}

// ProbeSample is one generation-probe result (TTFT and decode rate).
type ProbeSample struct {
	At     time.Time
	Addr   string
	Model  string
	OK     bool
	Err    string
	TTFTms float64
	TokPS  float64
	Tokens int
	// RetryAfter is how long the caller should wait before probing this
	// backend again. Set on 429/503 so an overloaded or billed gateway is
	// not hammered; zero means no extra backoff.
	RetryAfter time.Duration
}

// ProbeCmp compares two ProbeSamples for newest-last ordering. Time is the
// primary key; equal timestamps then order by Addr and Model so two
// sources with the same seed cannot disagree about probe order.
func ProbeCmp(a, b ProbeSample) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if c := strings.Compare(a.Addr, b.Addr); c != 0 {
		return c
	}
	return strings.Compare(a.Model, b.Model)
}

// AppendSorted appends item to s at its stable position, after every element
// cmp reports as less than or equal to it, then drops whatever falls outside
// the newest max entries. Time is the primary key; equal timestamps then
// order by identity so concurrent completions cannot shuffle a replay. One
// binary search plus one shift replaces a full re-sort per event, and the
// window is trimmed in the same call, so a caller cannot keep one entry too
// many or one too few.
func AppendSorted[T any](s []T, item T, max int, cmp func(a, b T) int) []T {
	s = append(s, item)
	lastIdx := len(s) - 1
	insertIdx := sort.Search(lastIdx, func(j int) bool { return cmp(s[j], item) > 0 })
	copy(s[insertIdx+1:], s[insertIdx:])
	s[insertIdx] = item
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

// InsertSorted returns s with its last element moved to its stable position,
// after every element cmp reports as less than or equal to it. A caller that
// has already appended the item delegates here, so the trim stays the caller's
// business and the ordering is one binary search plus one shift.
func InsertSorted[T any](s []T, cmp func(a, b T) int) []T {
	if len(s) < 2 {
		return s
	}
	item := s[len(s)-1]
	i := sort.Search(len(s)-1, func(j int) bool { return cmp(s[j], item) > 0 })
	copy(s[i+1:], s[i:])
	s[i] = item
	return s
}

// AppendRetained appends item to a newest-last feed of at most max entries
// and reports whether it was retained.
//
// An item sorting at or ahead of the oldest entry the feed already holds is
// what [AppendSorted] would trim away on the same call: it would be dropped
// the instant it landed, and no consumer would ever read it. Refusing it
// instead keeps "did the feed take it" a truthful answer for a caller that
// reports the count back to a sender, and keeps the id a caller ledgers for
// suppressed replays off entries nothing holds.
func AppendRetained[T any](s []T, item T, max int, cmp func(a, b T) int) ([]T, bool) {
	if len(s) == max && cmp(item, s[0]) <= 0 {
		return s, false
	}
	return AppendSorted(s, item, max, cmp), true
}

// TempReading is one thermal sensor value in millidegrees Celsius.
type TempReading struct {
	Label  string
	MilliC int
	IsGPU  bool
}

// GPUDevice is one accelerator as reported by sysfs/vendor CLIs (no vendor
// libraries linked).
type GPUDevice struct {
	Vendor   string // nvidia | amd | intel | apple
	Index    int
	Name     string
	MilliC   int
	MemUsed  uint64
	MemTotal uint64
	UtilPct  float64
	PowerW   float64
	Driver   string
}

// SysSample carries host-level vitals: RAM, swap, load and temperatures.
type SysSample struct {
	CPUModel string
	OsName   string // PRETTY_NAME / product version
	Kernel   string // uname -r

	MemTotal uint64
	MemUsed  uint64

	SwapTotal uint64
	SwapUsed  uint64

	Load1  float64
	Load5  float64
	Load15 float64

	HostUptime time.Duration
	Drivers    map[string]string // vendor -> version
	NPUs       []string          // detected accelerator drivers
	RemoteHost string            // set when stats come via ssh
	// RemoteErr is why the last remote vitals poll failed, empty while the
	// ssh target is answering. It rides along with RemoteHost past the
	// staleness window so a target that went dark names itself and its
	// reason instead of the header quietly reverting to local readings.
	RemoteErr string

	Temps []TempReading
	GPUs  []GPUDevice
}

// Snapshot is everything the UI needs for one frame.
type Snapshot struct {
	At        time.Time
	Uptime    time.Duration
	Providers []ProviderSnapshot
	Agents    []AgentEvent // newest last
	Probes    []ProbeSample
	Sys       *SysSample
}
