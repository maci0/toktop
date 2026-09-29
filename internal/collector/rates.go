package collector

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
)

// Rates and the history they are drawn from: the per-endpoint counter
// baselines, the smoothing applied to every derived tok/s, and the fixed
// rings the charts plot. The port helpers at the end answer which engine a
// listening process belongs to, which every provider's frame needs.

const emaAlpha = 0.35

type prevSample struct {
	at       time.Time
	outTotal float64
	inTotal  float64
	outEMA   float64
	inEMA    float64
}

// rates derives smoothed tok/s deltas since the previous sample.
func (c *Collector) rates(key string, m *provider.Metrics, now time.Time) (outPS, inPS float64) {
	pv, had := c.prev[key]
	if !had {
		// The baseline is seeded so the next scrape's delta starts here,
		// but a direct gauge does not need history: report it now instead
		// of blanking a live engine for one interval.
		if m.HasDirectOutPS {
			outPS = m.DirectOutPS
		}
		c.prev[key] = prevSample{at: now, outTotal: m.OutTotal, inTotal: m.InTotal, outEMA: outPS}
		return outPS, 0
	}
	dt := now.Sub(pv.at).Seconds()
	if dt < 0 {
		// The wall clock moved backwards (NTP correction, a resume from
		// sleep). Elapsed time over this interval is negative, so no rate is
		// defined, and holding the prior one would report it unchanged for as
		// long as the step lasts. Report no throughput for the interval the
		// step made and re-seed the baseline here: kept at the older sample,
		// every later sample lands before it too, so the endpoint reads zero
		// until the clock walks back past the step (minutes, on a real
		// correction). The prior rate rides along so the re-seed restarts the
		// interval, not the smoothing.
		c.prev[key] = prevSample{
			at: now, outTotal: m.OutTotal, inTotal: m.InTotal,
			outEMA: pv.outEMA, inEMA: pv.inEMA,
		}
		return 0, 0
	}
	if dt == 0 {
		// Zero elapsed time cannot yield a rate: 0/0 is NaN and n/0 is
		// +Inf, and either would poison this EMA and every later sample
		// derived from it. Hold the prior rate; keep the older baseline so
		// the next real interval accounts for these tokens too.
		return pv.outEMA, pv.inEMA
	}
	rawOut := max((m.OutTotal-pv.outTotal)/dt, 0) // clamp on counter reset
	rawIn := max((m.InTotal-pv.inTotal)/dt, 0)
	if m.HasDirectOutPS { // trust the engine's own tok/s gauge when present
		rawOut = m.DirectOutPS
	}
	outPS = ema(pv.outEMA, rawOut)
	inPS = ema(pv.inEMA, rawIn)
	c.prev[key] = prevSample{
		at: now, outTotal: m.OutTotal, inTotal: m.InTotal,
		outEMA: outPS, inEMA: inPS,
	}
	return outPS, inPS
}

func ema(prev, raw float64) float64 { return prev*(1-emaAlpha) + raw*emaAlpha }

// timedRing is a value history carrying the wall-clock time of every sample,
// so charts can place each point on an absolute time axis.
//
// The backing buffer is allocated once at HistoryLen and reused head-first:
// after warm-up push never allocates or copies, where sliding a slice
// (vals = vals[1:]) would realloc on every push for the life of the ring.
// Times ride in a parallel ring with the same head, so a sample and its
// instant are always overwritten together.
type timedRing struct {
	buf  []float64   // fixed capacity HistoryLen, samples in insertion order
	ts   []time.Time // the instant each buf entry was pushed
	head int         // 0 until the ring wraps, then the index of the oldest sample
}

func (r *timedRing) push(v float64, now time.Time) {
	if r.buf == nil { // one reservation for the ring's whole life
		r.buf = make([]float64, 0, core.HistoryLen)
		r.ts = make([]time.Time, 0, core.HistoryLen)
	}
	if len(r.buf) < core.HistoryLen { // filling: keep appending in order
		r.buf = append(r.buf, v)
		r.ts = append(r.ts, now)
		return
	}
	// head is always in [0, HistoryLen) here: filling leaves it at 0,
	// and each overwrite below wraps it after incrementing.
	r.buf[r.head] = v // overwrite the oldest sample
	r.ts[r.head] = now
	r.head++
	if r.head == core.HistoryLen {
		r.head = 0
	}
}

// copy returns the samples in insertion order (oldest first), detached from
// the ring so snapshots stay stable across later pushes.
func (r *timedRing) copy() []float64 {
	if len(r.buf) == 0 {
		return nil
	}
	out := make([]float64, len(r.buf))
	n := copy(out, r.buf[r.head:])
	copy(out[n:], r.buf[:r.head])
	return out
}

// times returns each sample's instant, oldest first, paired with copy.
func (r *timedRing) times() []time.Time {
	if len(r.ts) == 0 {
		return nil
	}
	out := make([]time.Time, len(r.ts))
	n := copy(out, r.ts[r.head:])
	copy(out[n:], r.ts[:r.head])
	return out
}

func (c *Collector) ring(m map[string]*timedRing, key string) *timedRing {
	r, ok := m[key]
	if !ok {
		r = &timedRing{}
		m[key] = r
	}
	return r
}

// procsByPort indexes engine processes by their effective listen port;
// on a collision the first sample wins.
func procsByPort(infos []procs.Info) map[int]procs.Info {
	byPort := make(map[int]procs.Info, len(infos))
	for _, p := range infos {
		if port := p.ListenPort(); port > 0 {
			if _, dup := byPort[port]; !dup {
				byPort[port] = p
			}
		}
	}
	return byPort
}

// httpPort extracts the TCP port from a backend URL. Non-http(s) addresses
// (tests use fake://, and a blank Addr is not a listener) must not fall
// through to port 80: that would attach GPUStack-on-80 process stats to
// an unrelated provider.
func httpPort(u *url.URL) int {
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0
	}
	if _, port, err := net.SplitHostPort(u.Host); err == nil {
		if p, err := strconv.Atoi(port); err == nil && p >= 1 && p <= 65535 {
			return p
		}
		return 0
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// loopbackPort answers both halves of the engine-block process lookup from
// one parse. Every provider's frame asks for both, so each engine would
// otherwise pay two parses per frame to decide whether a listening port
// belongs to it.
func loopbackPort(addr string) (port int, loopback bool) {
	u, err := url.Parse(addr)
	if err != nil {
		return 0, false
	}
	host := u.Hostname()
	return httpPort(u), strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}
