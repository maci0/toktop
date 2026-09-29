package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

const runTimeout = 2500 * time.Millisecond

// pipeGrace bounds Output's wait on the command's output pipes after the
// process exits or is killed by the context deadline: a grandchild inheriting
// stdout (nested shells, CLI wrappers) would otherwise hold the read end
// open past every deadline and pin the sampler's caller indefinitely.
const pipeGrace = 500 * time.Millisecond

type toolInfo struct {
	path string
	ok   bool
	at   time.Time // when the answer was recorded
}

var tools sync.Map // tool name -> *toolInfo

// toolRetry spaces out LookPath of a missing vendor CLI. A miss cached
// forever would blank the GPU row for a session that started before the
// driver module (or its bin dir) was on PATH.
const toolRetry = 30 * time.Second

// toolHitTTL is how long a resolved path is reused. A hit kept for the
// process lifetime would leave the GPU row empty for the rest of the session
// after a driver or container reinstall moved or removed the binary: the
// cached path is executed every poll, fails, and nothing re-resolves it. The
// window is long because a tool's location changes rarely, and re-resolving
// costs one directory scan per vendor per window rather than one per frame.
const toolHitTTL = 10 * time.Minute

// toolWindow is how long the recorded answer stands: a miss is retried on the
// short retry spacing, a hit on the long one.
func toolWindow(ok bool) time.Duration {
	if ok {
		return toolHitTTL
	}
	return toolRetry
}

// lookPath is exec.LookPath, swapped in tests.
var lookPath = exec.LookPath

// clock is the instant every cache in this package ages against: the vendor
// CLI path memo, the platform lookups, and the tool outage latch. It is a var
// so SetNow can expose it, matching sysmon and provider. Left on the wall
// clock, a sampler whose frames are stamped on a seeded timeline would decide
// its own frame's contents by how long the process happened to run, which is
// the one thing a replay cannot reproduce.
var (
	clockMu sync.RWMutex
	clock   = time.Now
)

// SetNow overrides the clock this package's caches age against, restoring the
// wall clock for nil. Call it before sampling starts, the way
// sysmon.SetNow asks.
func SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	clockMu.Lock()
	clock = fn
	clockMu.Unlock()
}

// instant reads the injected clock, calling it outside the lock.
func instant() time.Time {
	clockMu.RLock()
	fn := clock
	clockMu.RUnlock()
	return fn()
}

func lookup(name string) (string, bool) {
	if v, ok := tools.Load(name); ok {
		ti := v.(*toolInfo)
		if core.Age(instant(), ti.at) < toolWindow(ti.ok) {
			return ti.path, ti.ok
		}
	}
	p, err := lookPath(name)
	ti := &toolInfo{path: p, ok: err == nil, at: instant()}
	tools.Store(name, ti)
	return ti.path, ti.ok
}

// run invokes a vendor CLI and reports its stdout. A failure returns
// (nil, false) and is audited once per outage, not once per poll.
//
// name is the tool this build looks up and path is what lookup resolved it to.
// The outage latch is keyed by the name and the audit line carries the path:
// a resolved path changes when a driver is reinstalled or a symlink flips, and
// keying the latch by it would make one tool look like a new one every time
// the binary moved.
//
// The audit matters because the alternative is indistinguishable from the
// truth: a driver that has been unloaded, a wedged nvidia-smi, a container
// whose GPU device vanished all blank the GPU row, and a machine with no GPU
// at all looks identical on screen. Without a line naming the tool and its
// reason, an operator debugging a missing GPU readout has nothing to read.
//
// decode judges whether the output is usable and returns why it is not. It
// runs before noteRunOK, because a tool that exits 0 with output this build
// cannot read is not a tool that came back: clearing the outage first would
// log "answering again" for a tool that is producing nothing, and the GPU row
// would stay blank for the rest of the session with no line naming the cause.
// A nil decode means the exit status is the whole signal, for a tool whose
// empty answer is a valid answer.
func run(ctx context.Context, name, path string, decode func([]byte) error, args ...string) ([]byte, bool) {
	c, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, path, args...)
	cmd.WaitDelay = pipeGrace
	core.GroupKill(cmd)
	// Capped like every HTTP body this process reads. Output would grow an
	// internal buffer for whatever the tool printed inside runTimeout, and a
	// wedged or hostile nvidia-smi on PATH has no reason to stop: the sampler
	// runs it several times per tick, so an uncapped read is a memory
	// exhaustion the dashboard cannot defend against. Over the cap the write
	// fails, the read end closes under the tool, and the call is audited and
	// reported as a miss like any other.
	var out cappedOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		noteRunFailure(name, path, err)
		return nil, false
	}
	b := out.buf.Bytes()
	if decode != nil {
		if err := decode(b); err != nil {
			noteRunFailure(name, path, err)
			return nil, false
		}
	}
	noteRunOK(name, path)
	return b, true
}

// xpuUsable checks that xpu-smi answered in the JSON its -j flag promises. An
// empty device list is a legitimate answer (a host with no Intel device), so
// this judges the encoding and not the emptiness: a banner, an error string,
// or a driver that changed the shape all fail here while a quiet tool passes.
func xpuUsable(b []byte) error {
	if !json.Valid(b) {
		return errors.New("output is not JSON")
	}
	return nil
}

// audit builds the logger for the vendor-CLI lines. A var so a test can point
// it at a handler it can read.
var audit = logcfg.Logger

// runState tracks whether a vendor CLI is currently failing, so the audit log
// records the start of an outage once and its end once, rather than one line
// per poll. A run lasting days would otherwise write a line every interval
// for a tool that was uninstalled hours ago.
//
// An entry is never removed. The keys are the vendor CLIs this build looks
// up, so the table is bounded at a handful, and dropping an entry on recovery
// loses a failure a concurrent poll had just recorded: the next failure reads
// as a fresh outage and the log says a tool failed twice for one run of it.
// Keyed by name rather than by resolved path for that bound to hold: lookup
// re-resolves after toolHitTTL, and a driver reinstall moves the binary.
var runState sync.Map // tool name -> *toolRun

type toolRun struct {
	mu     sync.Mutex
	failed bool
	since  time.Time
}

// noteRunFailure records the start of an outage. Repeat failures of a tool
// already recorded as failing add nothing: the line for the start of the
// outage named the reason, and every later poll would only repeat it.
func noteRunFailure(name, path string, err error) {
	s, _ := runState.LoadOrStore(name, &toolRun{})
	t := s.(*toolRun)
	t.mu.Lock()
	first := !t.failed
	if first {
		t.failed, t.since = true, instant()
	}
	t.mu.Unlock()
	if !first {
		return
	}
	audit().Warn("toktop: gpu vendor tool failed",
		"tool", logcfg.Field(name, 256),
		"path", logcfg.Field(path, 256),
		"error", logcfg.Field(err.Error(), 256))
}

// noteRunOK clears a recorded outage and says so, so a tool that comes back is
// distinguishable on the log from one that never failed.
func noteRunOK(name, path string) {
	s, ok := runState.Load(name)
	if !ok {
		return
	}
	t := s.(*toolRun)
	t.mu.Lock()
	if !t.failed {
		t.mu.Unlock()
		return
	}
	t.failed = false
	downFor := core.Age(instant(), t.since)
	t.mu.Unlock()
	audit().Info("toktop: gpu vendor tool answering again",
		"tool", logcfg.Field(name, 256),
		"path", logcfg.Field(path, 256),
		"down_for", downFor.Round(time.Second))
}

// maxToolOutput is the most stdout one vendor CLI may contribute. A healthy
// nvidia-smi answers a fixed handful of CSV lines well under this; the room
// above that is for a fleet of cards, not for output that grows without end.
const maxToolOutput = 1 << 20

// cappedOutput is a bytes.Buffer that refuses to grow past maxToolOutput.
type cappedOutput struct{ buf bytes.Buffer }

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > maxToolOutput {
		return 0, fmt.Errorf("vendor CLI wrote more than %d bytes", maxToolOutput)
	}
	return c.buf.Write(p)
}
