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
	"fmt"
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
func Logger() *slog.Logger {
	lvl, err := ParseLogLevel(os.Getenv(LevelEnv))
	if err != nil {
		lvl = slog.LevelInfo // main already rejected this; stay quiet if constructed in tests
	}
	return slog.New(HomeHandler{Handler: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: utcTime,
	})})
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
	// home directory is forwarded without collecting anything.
	changed := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() != slog.KindString {
			return true
		}
		if s := core.RedactHome(a.Value.String()); s != a.Value.String() {
			changed = true
			return false
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
// caller's own attributes are never written through. A non-string attribute is
// carried over untouched: a group is the one value this does not walk, and a
// number, a duration or a time hold no path.
func foldHomeAttrs(attrs []slog.Attr) []slog.Attr {
	folded := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		if a.Value.Kind() == slog.KindString {
			a.Value = slog.StringValue(core.RedactHome(a.Value.String()))
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
