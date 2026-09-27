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
	"time"

	"github.com/maci0/toktop/internal/core"
)

// LevelEnv is the process environment variable that sets the audit-log floor.
// Empty means info. Main validates the value at startup.
const LevelEnv = "TOKTOP_LOG_LEVEL"

// ParseLogLevel maps a TOKTOP_LOG_LEVEL value onto a slog floor.
// Empty is info. Accepted names are debug, info, warn (or warning), and error.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("$%s must be debug, info, warn, or error, got %q", LevelEnv, s)
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

// Logger returns the process logger: a text handler on stderr at the floor
// TOKTOP_LOG_LEVEL names, stamping every record in UTC so lines from several
// machines sort against each other.
func Logger() *slog.Logger {
	lvl, err := ParseLogLevel(os.Getenv(LevelEnv))
	if err != nil {
		lvl = slog.LevelInfo // main already rejected this; stay quiet if constructed in tests
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: utcTime,
	}))
}

func utcTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(time.RFC3339Nano))
	}
	return a
}

// Field prepares attacker-shaped text for a single-line log attribute:
// terminal escapes stripped, whitespace collapsed so a payload cannot split
// the line, then capped.
func Field(s string, n int) string {
	return core.ClampField(strings.Join(strings.Fields(core.SanitizeText(s)), " "), n)
}

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
