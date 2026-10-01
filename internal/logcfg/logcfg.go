// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package logcfg owns how toktop writes its audit log: the environment
// variable that sets the floor, the logger built from it, and the redaction
// every line goes through.
//
// The level is a process-wide setting, not an ingest one: the startup
// validation and config line read it whether or not the endpoint runs, so
// main depends on this package rather than on the server that happens to be
// the first thing to want a logger.
package logcfg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/maci0/toktop/internal/core"
)

// LevelEnv is the process environment variable that sets the audit-log floor.
// Empty means info. Main validates the value at startup.
const LevelEnv = "TOKTOP_LOG_LEVEL"

// ParseLogLevel maps a TOKTOP_LOG_LEVEL value onto a slog floor.
// Empty is info. Accepted names are debug, info, warn (or warning), and error.
// Folded with core.FoldASCII: the accepted names are ASCII literals, and
// strings.ToLower also folds runes whose lowercase form is ASCII, so a level
// spelled with U+0130 or U+212A would be accepted as one the operator did not
// name.
func ParseLogLevel(s string) (slog.Level, error) {
	switch core.FoldASCII(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("$%s must be debug, info, warn (or warning), or error, got %q", LevelEnv, s)
	}
}

// LogLevelName renders a level as the TOKTOP_LOG_LEVEL word that selects it,
// so the startup config line and the documentation spell it the same way.
// slog's own String() would print "WARN".
func LogLevelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// utcStamp is RFC3339 with a fixed nine-digit fraction. RFC3339Nano strips
// trailing zeros and drops the point entirely on a whole second, so
// 12:00:00.9Z sorts before 12:00:00Z and every whole-second line lands at the
// end of its own second, which is the one order a UTC stamp is chosen for.
const utcStamp = "2006-01-02T15:04:05.000000000Z07:00"

// Logger returns the process logger: a text handler on stderr at the floor
// TOKTOP_LOG_LEVEL names, stamping every record in UTC so lines from several
// machines sort against each other, and folding the home directory out of
// every line through [HomeHandler].
//
// The handler writes through a [LossHandler], so a line that stderr refused is
// counted rather than dropped in silence: slog's own API discards whatever
// Handle returns, and an audit line is the only record that a store backup
// failed, a store was read back from its copy, or a rename could not be made
// durable, so a stderr that is full, redirected to a full disk, or closed
// loses the operator exactly the line they would have read. See the handler
// for what is reported and when.
func Logger() *slog.Logger {
	lvl, err := ParseLogLevel(os.Getenv(LevelEnv))
	if err != nil {
		lvl = slog.LevelInfo // main already rejected this; stay quiet if constructed in tests
	}
	// One counter for the stderr this logger writes to, and the same stderr
	// for every record: the packages that audit all reach it through one
	// SwapLogger holding the logger this returns, so a process counts its lost
	// lines once against the one channel they went missing from.
	lw := &lossWriter{w: os.Stderr}
	return slog.New(HomeHandler{Handler: LossHandler{w: lw, inner: slog.NewTextHandler(lw, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: utcTime,
	})}})
}

// lossWriter is the stderr the audit log is written to, counting the lines it
// refused. One write is one audit line, which is what makes the count a line
// count: the handler below writes each record in a single call, and a partial
// write reports a non-nil error, so one refusal never stands for a fraction of
// a line.
//
// A refused line is counted and nothing more. Repeating the warning on every
// refused line turns one full disk into a flood on a stderr that is itself the
// thing failing, and a warning nobody can read is not a signal either; the
// count is reported once, by the next write stderr accepts, which is also the
// first moment there is a channel to say it on.
type lossWriter struct {
	w io.Writer

	mu      sync.Mutex
	lost    uint64
	opened  bool  // a line was lost and the loss has not been reported yet
	lastErr error // why, from the first refusal since the last report
}

// Write forwards to stderr, and remembers a refusal. A short write is a loss
// as surely as a failed one, and io.Writer's contract makes both a non-nil
// error, so the two are one branch here.
func (l *lossWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil || n < len(p) {
		if err == nil {
			err = errWriteRefused
		}
		l.mu.Lock()
		l.lost++
		l.opened = true
		// The first reason is the one kept: a stderr that refused once
		// because a disk filled and then again because a pipe closed is the
		// same channel failing two ways, and the first is the one that
		// explains the rest.
		if l.lastErr == nil {
			l.lastErr = err
		}
		l.mu.Unlock()
		if n == 0 {
			return n, err
		}
		// A writer that reported progress and then an error still failed to
		// write the line; reporting only the error would let a caller read a
		// short write as a whole one.
		return n, errWriteRefused
	}
	return n, nil
}

// errWriteRefused is what a short write reports. The wrapped error is the
// one stderr gave, so the reason a full disk, a closed pipe or a broken
// redirect refused reaches the operator with the count naming it.
var errWriteRefused = errors.New("the audit line was only partly written")

// takeLoss reports the lines lost since the last call and clears the record,
// so the same loss is never reported twice and a loss that nothing follows is
// still pending rather than forgotten.
//
// It answers false when nothing was lost, which is the case every healthy run
// takes and the only one that costs no more than a counter read.
func (l *lossWriter) takeLoss() (uint64, error, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.opened {
		return 0, nil, false
	}
	// Captured before the reset: the reason is read here, and an assignment
	// that cleared it first would return the nil it just wrote.
	lost, why := l.lost, l.lastErr
	l.opened, l.lastErr, l.lost = false, nil, 0
	return lost, why, true
}

// LossHandler reports the audit lines stderr refused, on the next line stderr
// accepts. It wraps the text handler rather than replacing it, so the level
// floor, the UTC stamp and the folding are the inner handler's and unchanged.
//
// The count waits for a line that lands, because stderr is the only sink this
// logger has: a line lost to a full disk is a line lost to a channel that is
// out of room, so saying so into the same channel is how it is lost twice. The
// pending loss is carried until stderr takes a line again, which is the first
// moment there is somewhere to say it, and it is said once rather than once
// per refusal, because a flood on a channel that is already failing is not a
// signal. Writing the notice through the inner handler rather than to stderr
// directly is what keeps the home-directory fold applied to it, and a count
// that carried a home directory would be the one line of the log that leaked
// it.
type LossHandler struct {
	inner slog.Handler
	w     *lossWriter
}

// Handle implements slog.Handler.
func (h LossHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if err == nil {
		if lost, lerr, ok := h.w.takeLoss(); ok {
			// The notice is its own record, so the loss reads on a line of
			// its own rather than being a prefix an operator's log parser
			// attributes to the audit line that happened to follow it.
			_ = h.inner.Handle(ctx, slog.NewRecord(r.Time, slog.LevelError, lostNotice(lost, lerr), 0))
		}
		return nil
	}
	return err
}

// WithAttrs implements slog.Handler. The loss is reported through h.inner, so
// the attributes bound here are the ones a later notice carries rather than
// none, and WithGroup is passed through for the same reason.
func (h LossHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LossHandler{inner: h.inner.WithAttrs(attrs), w: h.w}
}

// WithGroup implements slog.Handler.
func (h LossHandler) WithGroup(name string) slog.Handler {
	return LossHandler{inner: h.inner.WithGroup(name), w: h.w}
}

// Enabled implements slog.Handler.
func (h LossHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// lostNotice is the one line that says audit lines were lost. It names the
// count and the reason, and nothing about the lines themselves: they are
// already gone, and a handler that printed them would be trying to write them
// to the channel that refused them.
func lostNotice(lost uint64, err error) string {
	if err == nil {
		return fmt.Sprintf("toktop: %d audit line(s) could not be written to stderr and are lost; "+
			"the record of them does not exist anywhere else", lost)
	}
	return fmt.Sprintf("toktop: %d audit line(s) could not be written to stderr and are lost: %v", lost, err)
}

// A SwapLogger is the logger a package's audit lines go to, with the
// replacement a test installs so it can read what was written. The swap and
// the read take the same lock: a Go func value is two words, so a goroutine
// left over from an earlier test would otherwise call a new code pointer
// against the closure word the old one carried.
type SwapLogger struct {
	mu sync.Mutex
	fn func() *slog.Logger
}

// NewSwapLogger returns a SwapLogger that starts at fn.
func NewSwapLogger(fn func() *slog.Logger) *SwapLogger {
	return &SwapLogger{fn: fn}
}

// Logger returns the logger to write the next line to.
func (s *SwapLogger) Logger() *slog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fn()
}

// Set sends the lines from here on to the logger fn returns. A nil restores
// the process logger.
func (s *SwapLogger) Set(fn func() *slog.Logger) {
	if fn == nil {
		fn = Logger
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fn = fn
}

// HomeHandler rewrites the home directory to "~" in the message and in every
// string attribute of a record. A path under $HOME names the account, and
// these lines are the ones pasted into issues, so the fold belongs here
// rather than in each call site: an attribute that reaches the audit log
// without it is a path leak, and every one of them is written by code that
// did not think about it (a request path, a rejected header, an error text
// from a library).
//
// Only the top level is folded. A group attribute's members are not walked:
// nothing in this tree logs one, and a fold that missed a nested value would
// be worse than one documented as covering the top level. Attributes bound
// with WithAttrs are folded as they are bound, which is the only point that
// reaches them: the inner handler owns them from then on and Handle never sees
// them.
type HomeHandler struct {
	slog.Handler
}

// Handle implements slog.Handler.
func (h HomeHandler) Handle(ctx context.Context, r slog.Record) error {
	msg := core.RedactHome(r.Message)
	// One walk to decide whether a rebuild is needed, so a record carrying no
	// home directory is forwarded without collecting anything. An error
	// attribute always counts as changed: foldHomeAttrs rewrites it to a
	// string whatever it says, so the fast path has to leave the record it is
	// handed or the attribute keeps the kind it arrived with.
	changed := false
	r.Attrs(func(a slog.Attr) bool {
		switch a.Value.Kind() {
		case slog.KindString:
			if s := core.RedactHome(a.Value.String()); s != a.Value.String() {
				changed = true
				return false
			}
		case slog.KindAny:
			if _, ok := a.Value.Any().(error); ok {
				changed = true
				return false
			}
		}
		return true
	})
	if !changed {
		r.Message = msg
		return h.Handler.Handle(ctx, r)
	}
	// Every attribute is collected, not only the ones from the first fold on:
	// the rebuilt record is all the inner handler ever sees, and an attribute
	// left out of it is dropped rather than passed through.
	attrs := make([]slog.Attr, 0, 8)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	// slog.Record hands out its attributes one at a time and offers no way to
	// put a rewritten one back, so a record that changed is rebuilt: the
	// message, the time, the level and the call site all carry over, and the
	// inner handler never sees the home directory.
	out := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	out.AddAttrs(foldHomeAttrs(attrs)...)
	return h.Handler.Handle(ctx, out)
}

// foldHomeAttrs rewrites the home directory in the string attributes of a
// record or of a WithAttrs call, and copies the slice it is given so a
// caller's own attributes are never written through.
//
// An error is folded too, and to a string, because that is the value a stat,
// open or read failure arrives as. Handed over as a value rather than as a
// string it is a kind this walk used to pass over untouched, so a line reading
// "error", err printed the account's own directory name in full on the very
// line that exists to explain the failure, while the path beside it folded to
// "~". os.PathError is the shape that turns up: its Error names the file it
// failed on. Rewriting it as a string also drops the type the log had no use
// for, since the text is what a reader reads.
//
// The remaining kinds are carried over: a group is the one value this does not
// walk, and a number, a duration or a time hold no path.
func foldHomeAttrs(attrs []slog.Attr) []slog.Attr {
	folded := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		switch a.Value.Kind() {
		case slog.KindString:
			a.Value = slog.StringValue(core.RedactHome(a.Value.String()))
		case slog.KindAny:
			if err, ok := a.Value.Any().(error); ok && err != nil {
				a.Value = slog.StringValue(core.RedactHome(err.Error()))
			}
		}
		folded[i] = a
	}
	return folded
}

// WithAttrs implements slog.Handler. The attributes are folded before the
// inner handler takes them, because that is the last point at which this one
// still sees them: a logger built by Logger().With("path", ...) writes that
// value into every line it logs, and an unfolded one names the account for as
// long as the log is kept.
func (h HomeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return HomeHandler{Handler: h.Handler.WithAttrs(foldHomeAttrs(attrs))}
}

// WithGroup implements slog.Handler.
func (h HomeHandler) WithGroup(name string) slog.Handler {
	return HomeHandler{Handler: h.Handler.WithGroup(name)}
}

func utcTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(utcStamp))
	}
	return a
}

// FieldCap is the cap to pass Field and RedactedField when the attribute's own
// width is not a fact about the caller: a host, a path, a tool name. A width
// that is a fact, a request id, an HTTP method, an agent name, is named where
// the field is built and passed as its own constant.
const FieldCap = 256

// Field prepares attacker-shaped text for a single-line log attribute:
// terminal escapes stripped, whitespace collapsed so a payload cannot split
// the line, then capped. [core.SingleLine] and [core.ClampField] are the two
// halves, and composing them here keeps the audit line and a dashboard cell
// folded the same way.
func Field(s string, n int) string { return core.ClampField(core.SingleLine(s), n) }

// Remote prepares a peer address for the audit line. Loopback keeps the port
// so a local sender can be told apart; any other IP is dropped. The address
// is personal data when the ingest endpoint is bound off loopback.
func Remote(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return net.JoinHostPort("loopback", port)
	}
	return "remote"
}

// remoteAddrPat matches the host:port form net.Addr.String uses for TCP:
// dotted IPv4, or bracketed IPv6 (including zone ids).
var remoteAddrPat = regexp.MustCompile(`(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9A-Fa-f:.]+(?:%[^\]\r\n]+)?\]):\d{1,5}`)

// RedactAddrs rewrites host:port appearances with Remote so a line cannot
// carry a peer IP.
func RedactAddrs(s string) string {
	if !strings.ContainsAny(s, ".[") {
		return s
	}
	return remoteAddrPat.ReplaceAllStringFunc(s, Remote)
}

// RedactedField is [Field] on text that carries a peer address, which an error
// from the ssh or http stacks does: the fold has to run before the cap and the
// collapse, or a redacted address could be split across two lines by a payload
// after it.
func RedactedField(s string, n int) string { return Field(RedactAddrs(s), n) }

// RedactHandler rewrites slog messages the way Remote rewrites the audit
// line's remote attribute. http.Server.ErrorLog is a *log.Logger, so the
// peer address arrives as text in the message, not as a structured attr.
type RedactHandler struct {
	slog.Handler
}

// Handle implements slog.Handler.
func (h RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = RedactAddrs(r.Message)
	return h.Handler.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return RedactHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h RedactHandler) WithGroup(name string) slog.Handler {
	return RedactHandler{h.Handler.WithGroup(name)}
}
