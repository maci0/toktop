// Package probe fires small streaming generations at backends to measure
// time-to-first-token and decode throughput the way clients experience it.
//
// One concern per file: probe.go is the request, the budgets that bound a
// generation, and the HTTP handling both dialects share, ollama.go the Ollama
// dialect, openai.go the OpenAI-compatible one, model.go the choice of which
// model to ask.
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
)

var client = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// probeTokens sizes a probe: a few dozen tokens are plenty to time
// first-token latency and decode rate without turning a benchmark into an
// unbounded generation. The request asks the engine to stop there; the
// client also stops reading once this many content or reasoning frames have
// arrived, because some gateways ignore max_tokens/num_predict and would
// otherwise generate (and bill) until the HTTP timeout.
const probeTokens = 32

// probeTokenTrust is the highest engine-reported eval_count /
// completion_tokens we believe. Tokenizers can overshoot the request a
// little; a billion-token usage field is junk that would poison tok/s.
const probeTokenTrust = probeTokens * 4

// probeContentBytes is a second hang-up: frame counting treats each
// SSE/NDJSON content payload as one token, so a gateway that dumps a huge
// delta in one frame would otherwise keep the connection open (and keep
// billing) until the HTTP timeout. 32 bytes per requested token is a floor,
// not a guarantee: a multibyte or base64-wrapped 32-token reply can exceed
// it, and hanging up early is the safe direction.
const probeContentBytes = probeTokens * 32

// evalDurationBandDiv sets the floor of the band a scaled engine-reported
// decode duration must land in, as a divisor of the measured round trip.
// Generation dominates a probe, so a correct reading is not a rounding error
// of the exchange; 4 keeps a genuine tail-light decode (short prompt, small
// max_tokens) from being rescaled away.
const evalDurationBandDiv = 4

// maxEvalDuration is the longest decode an engine can honestly report. A probe
// asks for probeTokens (32) of it, so a reading past this is a malformed count
// rather than a slow host, and fitEvalDuration's fallback (the measured round
// trip) describes it better than the value does.
const maxEvalDuration = 24 * time.Hour

// probeFrameMax is the third hang-up, and the only one the other two cannot
// supply. Token and byte budgets count what the engine *generated*: an engine
// that emits frames carrying no content and no reasoning trips neither, so a
// keepalive stream, a role-only first frame, or a gateway replaying empty
// choices holds the generation open, and the generation is billed, until the
// 30s client timeout. A probe needs 32 of content, so a window this wide is
// several times any well-formed stream, and reaching it without a single token
// means the exchange is not going to produce one.
const probeFrameMax = 512

// probeLineMax is the largest SSE/NDJSON frame we will buffer, and the cap on
// a whole non-stream body. probeStreamMax (128 KiB) bounds a streamed body
// overall. A 32-token completion plus wrapper JSON is hundreds of bytes; a
// megabyte line is the engine ignoring the cap in one shot, and
// bufio.Scanner only applies the hang-up after the line is fully read.
const probeLineMax = 16 << 10

const probeStreamMax = 128 << 10

// probeBufInit is the bufio fill size for a stream scan. A frame is a few
// hundred bytes, so a small buffer is read a few times per stream and never
// held for the whole response.
const probeBufInit = 4 << 10

const promptText = "Count from one to twenty as words."

// retryAfterDefault is the floor for 429/503 backoff. A missing or tiny
// Retry-After must not disable the cap: the next --probe tick would otherwise
// POST again immediately against an overloaded or billed gateway.
const retryAfterDefault = 15 * time.Second

// retryAfterMax caps engine-supplied Retry-After. A hostile header must not
// silence probes for hours.
const retryAfterMax = 5 * time.Minute

// Request is one generation to measure: which engine dialect to speak, where
// it lives, and the model to ask for. An empty Model is refused by Run.
type Request struct {
	Kind  string // core.KindOllama | openai-compatible kinds
	Base  string
	Model string
}

// Run performs one probe and returns its sample (OK=false with Err set on failure).
func Run(ctx context.Context, r Request) core.ProbeSample {
	r.Model = capModel(r.Model)
	s := core.ProbeSample{At: time.Now(), Addr: r.Base, Model: r.Model}
	if r.Model == "" {
		// An empty id makes some engines load a default model (VRAM and,
		// on a billed gateway, tokens). Refuse rather than POST.
		s.Err = "no model"
		return s
	}
	start := time.Now()
	var (
		ttft    time.Duration
		tokens  int
		evalDur time.Duration
		err     error
	)
	if r.Kind == core.KindOllama {
		tokens, evalDur, ttft, err = probeOllama(ctx, r, &s)
	} else {
		tokens, ttft, err = probeOpenAI(ctx, r, &s)
	}
	total := time.Since(start)
	if err != nil {
		// Folded for the same reason collector.foldErr folds an engine
		// error: the text is engine-supplied and can echo a model or
		// config path under the operator's home, and the sample reaches
		// the frame and both reports, which are meant to be pasteable.
		// Snippet bounds it too, since a decoder error embeds the whole
		// offending literal.
		s.Err = core.Snippet([]byte(core.RedactHome(err.Error())))
		var se *httpStatusError
		if errors.As(err, &se) {
			s.RetryAfter = se.after
		}
		return s
	}
	// Windows clocks tick coarsely; instant local servers can land the whole
	// exchange inside one tick. Fall back to full-duration accounting.
	if ttft <= 0 {
		ttft = total
	}
	s.OK = true
	s.Tokens = tokens
	s.TTFTms = float64(ttft) / float64(time.Millisecond)
	// fitEvalDuration returns 0 when no scaling of the reported value is
	// believable, so the divisor is taken from the result rather than from
	// evalDur: dividing by that zero would store +Inf as the rate.
	evalDur = fitEvalDuration(evalDur, total)
	switch {
	case evalDur > 0:
		s.TokPS = float64(tokens) / evalDur.Seconds()
	case total > ttft && tokens > 0:
		s.TokPS = float64(tokens) / (total - ttft).Seconds()
	case tokens > 0 && total > 0:
		s.TokPS = float64(tokens) / total.Seconds()
	}
	return s
}

// nanoseconds converts a wire-reported count of nanoseconds to a Duration.
// A value outside the int64 nanosecond range, or a negative one, is not a
// duration: multiplying it would wrap into a plausible-looking number, and
// fitEvalDuration would then rescale that into the plausible band and report
// a throughput the engine never claimed. A value beyond maxEvalDuration is
// representable but no decode ran that long, so it returns zero and sends the
// caller to its wall-clock measurement.
func nanoseconds(n int64) time.Duration {
	if n <= 0 || n > int64(maxEvalDuration/time.Nanosecond) {
		return 0
	}
	return time.Duration(n) * time.Nanosecond
}

// fitEvalDuration normalizes the engine-reported eval_duration against the
// round trip we just measured wall-clock. The field is a bare integer with no
// unit on the wire; Ollama sends nanoseconds, but llama.cpp-derived and
// several gateway builds send microseconds or milliseconds, which reads
// 1000x or 1e6x slow. Generation dominates a probe, so the honest reading
// lands in the upper part of the measured exchange: take the finest unit whose
// scaling lands in that band.
//
// When the reported value is longer than the whole round trip, the raw
// nanosecond reading is the best one left (a fast local engine can
// legitimately report longer than the HTTP exchange around it). When every
// scaling is shorter than the band instead, the value is implausibly small in
// every unit: a decode that finishes more than four times faster than the
// request that carried it is a queued or proxied request, not a fast engine,
// and keeping the raw figure would read a microsecond report as nanoseconds
// and report throughput thousands of times too high. Refusing it leaves the
// caller on the wall-clock measurement it would use anyway.
func fitEvalDuration(reported, total time.Duration) time.Duration {
	if reported <= 0 || total <= 0 {
		return reported
	}
	lo := total / evalDurationBandDiv
	for _, unit := range []time.Duration{1, time.Microsecond, time.Millisecond} {
		if unit > 1 && reported > time.Duration(math.MaxInt64/int64(unit)) {
			continue // the rescaling would overflow
		}
		if scaled := reported * unit; scaled >= lo && scaled <= total {
			return scaled
		}
	}
	if reported > total {
		return reported
	}
	return 0
}

// overBudget reports that the client has seen enough generation to hang up.
// Frame count catches engines that ignore max_tokens one token at a time;
// byte count catches a single huge delta that would count as one frame; the
// raw frame count catches a stream that carries neither, which the other two
// cannot see. frames is every line the scanner yielded, not just the decoded
// ones: a keepalive the frame parser discards is still a billed round trip.
func overBudget(tokens, contentBytes, frames int) bool {
	return tokens >= probeTokens || contentBytes >= probeContentBytes || frames >= probeFrameMax
}

// resolveTokens picks a probe's token count. Engine-reported usage is
// preferred when it sits in a plausible band around the requested
// generation; anything outside is ignored in favour of content frames
// actually observed. Observed counts are capped at probeTokens because
// the client stops reading there.
func resolveTokens(observed, reported int) (tokens int, trustReported bool) {
	if observed <= 0 {
		return 0, false
	}
	if reported > 0 && reported <= probeTokenTrust {
		return reported, true
	}
	if observed > probeTokens {
		return probeTokens, false
	}
	return observed, false
}

// streamReadErr maps a scanner/body error onto a probe outcome. A cancelled
// caller's context is a real failure (shutdown must not mint a sample). A
// mid-stream drop after at least one token still yields a timed sample:
// hanging up is how we bound engines that ignore max_tokens, and a client
// timeout would otherwise throw away TTFT already measured. The same is true of
// a hang-up the client chose itself, which the transport reports as a
// truncation: without hungUp the deliberate close reads as the engine dying
// mid-generation, and a tokenless stream the client bounded on frame count
// would be filed as a broken engine instead of one that never answered.
func streamReadErr(ctx context.Context, err error, tokens int, hungUp bool) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && (hungUp || tokens > 0) {
		return nil
	}
	return err
}

// engineErrorText bounds a recognized engine error. The engine chooses the
// text, and ProbeSample keeps the last 128 samples, so an uncapped message is
// unbounded memory held across a poll cycle and an arbitrarily wide line at
// render time. core.ClampField is the cap; the unrecognized-junk path below
// gets terminal sanitization from core.Snippet instead.
func engineErrorText(s string) string {
	return core.ClampField(core.SingleLine(s), core.SnippetCap)
}

func postJSON(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	bearer.Apply(req)
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The snippet below is read for the message only, and a body closed
		// partway through goes back to TIME_WAIT rather than the idle pool.
		// A 429 or a 503 is exactly the answer an overloaded gateway gives on
		// every tick, so without the drain each one cost a fresh dial and a
		// socket to the same host for as long as the backend stayed down.
		defer drainAndClose(resp.Body)
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 4*core.SnippetCap))
		msg := fmt.Sprintf("%s: http %s", url, core.HTTPStatus(resp.Status))
		if s := core.Snippet(b); s != "" {
			msg += ": " + s
		}
		cause := errors.New(msg)
		if rerr != nil {
			// A body that stopped partway is a fragment, not what the engine
			// said. The cause is wrapped, not spelled into the text, so a
			// caller can still tell a truncated transfer from a bad payload.
			cause = fmt.Errorf("%s: %w", msg, rerr)
		}
		se := &httpStatusError{status: resp.StatusCode, err: cause}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			se.after = parseRetryAfter(resp)
		}
		return nil, se
	}
	return resp, nil
}

type httpStatusError struct {
	status int
	after  time.Duration
	err    error
}

// probeDrainCap bounds the tail drainAndClose throws away so the connection
// can be reused. The bytes go to io.Discard, so this bounds time rather than
// memory: an endpoint streaming an endless body on the error path would
// otherwise hold the caller on a read that answers nothing. Past the cap the
// connection is simply not reused, which is what closing an undrained body
// did anyway.
const probeDrainCap = 64 << 10

// drainAndClose reads out the tail of a body the caller stopped reading and
// then closes it. net/http only returns a connection to the idle pool when its
// body is closed at EOF, so a caller that stops at the end of a snippet has to
// finish the transfer here.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, probeDrainCap))
	body.Close()
}

func (e *httpStatusError) Error() string { return e.err.Error() }
func (e *httpStatusError) Unwrap() error { return e.err }

func parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return retryAfterDefault
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		secs = min(max(secs, int64(retryAfterDefault/time.Second)), int64(retryAfterMax/time.Second))
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), retryAfterDefault), retryAfterMax)
	}
	return retryAfterDefault
}
