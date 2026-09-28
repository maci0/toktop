// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package logcfg

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "", want: slog.LevelInfo},
		{in: "info", want: slog.LevelInfo},
		{in: "INFO", want: slog.LevelInfo},
		{in: " debug ", want: slog.LevelDebug},
		{in: "warn", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "trace", wantErr: true},
		{in: "inf", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseLogLevel(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseLogLevel(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, %v, want %v, nil", tt.in, got, err, tt.want)
		}
	}
}

func TestLoggerHonorsLogLevel(t *testing.T) {
	t.Setenv(LevelEnv, "error")
	lg := Logger()
	if !lg.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error should be enabled")
	}
	if lg.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be disabled at error floor")
	}
}

// main rejects an unparseable value at startup, so Logger never sees one from
// the command line. A library caller, a test, or an operator who edited the
// environment after start can, and the fallback has to be the quiet one
// rather than the zero value: slog.Level(0) is Info, but a future default
// must not silently open the log up.
func TestLoggerFallsBackToInfoOnAnUnparseableLevel(t *testing.T) {
	t.Setenv(LevelEnv, "trace")
	lg := Logger()
	if !lg.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info should be enabled after an unparseable level")
	}
	if lg.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug should be disabled after an unparseable level")
	}
}

func TestUtcLogTime(t *testing.T) {
	tm := time.Date(2026, 3, 15, 10, 30, 0, 0, time.FixedZone("EST", -5*3600))
	attr := slog.Time(slog.TimeKey, tm)
	got := utcTime(nil, attr)
	if got.Value.Kind() != slog.KindString {
		t.Fatalf("kind = %v, want string", got.Value.Kind())
	}
	if want := tm.UTC().Format(utcStamp); got.Value.String() != want {
		t.Errorf("got %q, want %q", got.Value.String(), want)
	}
	// The stamp is UTC so lines from several machines sort against each
	// other, which only holds if the field widths are fixed: a whole second
	// and a fraction of the same second must compare as the digits say.
	base := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		ns   int
		want string
	}{
		{0, "2026-03-15T10:30:00.000000000Z"},
		{500_000_000, "2026-03-15T10:30:00.500000000Z"},
		{900_000_000, "2026-03-15T10:30:00.900000000Z"},
		{1, "2026-03-15T10:30:00.000000001Z"},
	} {
		line := utcTime(nil, slog.Time(slog.TimeKey, base.Add(time.Duration(tc.ns)))).Value.String()
		if line != tc.want {
			t.Errorf("stamp = %q, want %q", line, tc.want)
		}
		if whole := base.UTC().Format(utcStamp); tc.ns > 0 && line <= whole {
			t.Errorf("stamp %q does not sort after the whole second it shares (%q)", line, whole)
		}
	}
	other := slog.String("other", "value")
	if gotOther := utcTime(nil, other); gotOther.Key != other.Key || gotOther.Value.String() != other.Value.String() {
		t.Errorf("non-time attr was modified: %+v", gotOther)
	}
}

func TestAddrRedactHandler(t *testing.T) {
	var buf bytes.Buffer
	baseHandler := slog.NewTextHandler(&buf, nil)
	h := RedactHandler{Handler: baseHandler}
	hWithAttrs := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	hWithGroup := h.WithGroup("grp")
	// The wrapper is the whole point of both methods: the derived handlers
	// must still be RedactHandler, not the bare inner handler, or a
	// dialed server logs peer addresses unredacted.
	if _, ok := hWithAttrs.(RedactHandler); !ok {
		t.Fatalf("WithAttrs returned %T, want RedactHandler", hWithAttrs)
	}
	if _, ok := hWithGroup.(RedactHandler); !ok {
		t.Fatalf("WithGroup returned %T, want RedactHandler", hWithGroup)
	}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "connect from 192.168.1.1:8080 and 127.0.0.1:9090", 0)
	if err := h.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "192.168.1.1:8080") {
		t.Errorf("remote IP leaked: %q", out)
	}
	if !strings.Contains(out, "remote") || !strings.Contains(out, "loopback:9090") {
		t.Errorf("redacted output missing expected tags: %q", out)
	}

	buf.Reset()
	derived := slog.New(hWithAttrs)
	derivedRecord := slog.NewRecord(time.Now(), slog.LevelWarn, "dial 192.168.1.1:8080", 0)
	if err := derived.Handler().Handle(context.Background(), derivedRecord); err != nil {
		t.Fatal(err)
	}
	derivedOut := buf.String()
	if strings.Contains(derivedOut, "192.168.1.1:8080") {
		t.Errorf("remote IP leaked through WithAttrs: %q", derivedOut)
	}
	if !strings.Contains(derivedOut, "remote") {
		t.Errorf("WithAttrs output missing expected tag: %q", derivedOut)
	}
}

func TestRedactLogAddrs(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http: panic serving 203.0.113.9:54321: boom", "http: panic serving remote: boom"},
		{"http: panic serving 127.0.0.1:9999: boom", "http: panic serving loopback:9999: boom"},
		{"http: panic serving [::1]:80: boom", "http: panic serving loopback:80: boom"},
		{"http: TLS handshake error from [2001:db8::1]:443: EOF", "http: TLS handshake error from remote: EOF"},
		{"no address here", "no address here"},
	}
	for _, tc := range cases {
		if got := RedactAddrs(tc.in); got != tc.want {
			t.Errorf("RedactAddrs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Remote is the only thing standing between a peer address and the audit log.
// The unparsable branch matters: a caller that hands it anything but host:port
// must get "unknown" rather than the input echoed back, IP and all.
func TestRemote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:54321", "loopback:54321"},
		{"127.0.0.53:53", "loopback:53"},
		{"[::1]:8080", "loopback:8080"},
		{"[fe80::1%eth0]:22", "remote"},
		{"192.168.1.1:8080", "remote"},
		{"[2001:db8::1]:443", "remote"},
		{"0.0.0.0:22", "remote"},
		{"", "unknown"},
		{"192.168.1.1", "unknown"},
		{"192.168.1.1:8080:extra", "unknown"},
		{"example.com:22", "remote"},
	}
	for _, tc := range cases {
		if got := Remote(tc.in); got != tc.want {
			t.Errorf("Remote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// LogLevelName exists so the startup config line and the docs spell the
// floor the way TOKTOP_LOG_LEVEL does, and ParseLogLevel is the only reader
// of that spelling. Anything the name renders must parse back to the same
// level, or the line names a setting the log does not apply.
func TestLogLevelName(t *testing.T) {
	for _, lvl := range []slog.Level{
		slog.LevelDebug, slog.LevelDebug + 2,
		slog.LevelInfo, slog.LevelInfo + 2,
		slog.LevelWarn, slog.LevelWarn + 2,
		slog.LevelError, slog.LevelError + 2,
	} {
		name := LogLevelName(lvl)
		if name == "" {
			t.Errorf("LogLevelName(%v) is empty", lvl)
			continue
		}
		if strings.ToUpper(name) == name && strings.ToLower(name) != name {
			t.Errorf("LogLevelName(%v) = %q, want the lower-case TOKTOP_LOG_LEVEL spelling", lvl, name)
		}
		got, err := ParseLogLevel(name)
		if err != nil {
			t.Errorf("ParseLogLevel(LogLevelName(%v)=%q) = %v", lvl, name, err)
			continue
		}
		// ParseLogLevel snaps to the named floor, so the bucket is what has
		// to survive the round trip, not the exact level.
		if LogLevelName(got) != name {
			t.Errorf("round trip of %v: LogLevelName(%v) = %q, want %q", lvl, got, LogLevelName(got), name)
		}
	}
	if got := LogLevelName(slog.LevelInfo - 4); got != "debug" {
		t.Errorf("LogLevelName below debug = %q", got)
	}
	if got := LogLevelName(slog.LevelError + 4); got != "error" {
		t.Errorf("LogLevelName above error = %q", got)
	}
}

// Field is what every attacker-shaped string reaches on its way into an audit
// line: an HTTP method, a request path, a request id, an engine's error text.
// Three properties have to hold together, and none is visible from any single
// input: escapes go, the result cannot contain a newline, and it is capped.
func TestField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"plain text is untouched", "POST", 16, "POST"},
		{"a path keeps its slashes", "/v1/events", 64, "/v1/events"},
		{"a newline collapses to a space", "GET\nlevel=INFO forged", 64, "GET level=INFO forged"},
		// A CR is dropped rather than collapsed, so the tokens either side
		// of it join: what matters is that no line break survives.
		{"a carriage return is dropped", "GET\rmsg=hi", 64, "GETmsg=hi"},
		{"runs of whitespace collapse to one", "a \t\n  b", 64, "a b"},
		{"an escape sequence is removed", "\x1b[31mred\x1b[0m", 64, "red"},
		{"an OSC title set is removed", "x\x1b]0;pwned\x07y", 64, "xy"},
		{"control characters go", "a\x00b\x1fc", 64, "abc"},
		{"a bidi override is removed", "a\u202eb", 64, "ab"},
		{"an empty string stays empty", "", 16, ""},
		{"whitespace only collapses to nothing", " \t\n ", 16, ""},
		{"a cap of zero yields nothing", "abc", 0, ""},
		{"a negative cap yields nothing", "abc", -1, ""},
		{"the cap truncates", "abcdefgh", 3, "abc"},
		{"the cap is in characters, not bytes", "héllo wörld", 5, "héllo"},
		{"an emoji is not cut in half", "a👩‍💻b", 2, "a👩‍💻"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Field(c.in, c.n)
			if got != c.want {
				t.Fatalf("Field(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
			// Whatever the exact bytes, no output may carry a line break or
			// an escape: that is the whole reason this wrapper exists.
			if strings.ContainsAny(got, "\n\r\x1b") {
				t.Errorf("Field(%q, %d) = %q still carries a line break or escape", c.in, c.n, got)
			}
		})
	}
}

// The cap has to survive the sanitizing pass too: sanitizing shortens a
// string, so a caller that sized the cap for the raw value would get a line
// longer than it asked for.
func TestFieldCapsAfterSanitizing(t *testing.T) {
	got := Field("\x1b[31mabcdefghij\x1b[0m", 4)
	if got != "abcd" {
		t.Errorf("Field with escapes = %q, want the first 4 clean characters", got)
	}
}

// The audit log is pasted into issues, so no line may carry a path under
// $HOME: it names the account. The fold lives in the handler, so it covers a
// value a call site never thought about (a request path, a rejected header).
func TestHomeHandlerFoldsHomeInMessageAndAttrs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	var buf bytes.Buffer
	lg := slog.New(HomeHandler{Handler: slog.NewTextHandler(&buf, nil)})
	lg.Error("cannot read "+filepath.Join(home, "toktop", "agents.json"),
		"path", filepath.Join(home, "toktop"),
		"status", http.StatusNotFound,
		"remote", "loopback:1234")
	got := buf.String()
	if strings.Contains(got, home) {
		t.Fatalf("line kept the home directory: %s", got)
	}
	// The fold keeps the separator it matched, so the expectation has to be
	// spelled with this platform's one. slog renders a message carrying a
	// space quoted and escaped, which is exactly what strconv.Quote produces:
	// on Windows the line holds "~\\toktop\\agents.json" where POSIX holds
	// "~/toktop/agents.json".
	sep := string(filepath.Separator)
	folded := "cannot read ~" + sep + "toktop" + sep + "agents.json"
	for _, want := range []string{
		"path=~" + sep + "toktop",
		strconv.Quote(folded),
		`status=404`,
		`remote=loopback:1234`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line lost %q: %s", want, got)
		}
	}
}

// A folded attribute rebuilds the record, so an attribute that came before the
// fold and carried no home of its own still has to reach the inner handler.
func TestHomeHandlerKeepsAttrsBeforeTheFold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	var buf bytes.Buffer
	lg := slog.New(HomeHandler{Handler: slog.NewTextHandler(&buf, nil)})
	lg.Warn("engine not answering",
		"engine", "ollama",
		"addr", filepath.Join(home, "toktop"),
		"status", http.StatusBadGateway)
	got := buf.String()
	if strings.Contains(got, home) {
		t.Fatalf("line kept the home directory: %s", got)
	}
	for _, want := range []string{"engine=ollama", "status=502", "addr=~"} {
		if !strings.Contains(got, want) {
			t.Errorf("line lost %q: %s", want, got)
		}
	}
}

// A record whose attributes carry no path keeps every other value as it was:
// the fold rewrites a home directory and nothing else.
func TestHomeHandlerLeavesOtherValuesAlone(t *testing.T) {
	t.Setenv("HOME", filepath.Join(string(filepath.Separator), "home", "someone"))
	var buf bytes.Buffer
	lg := slog.New(HomeHandler{Handler: slog.NewTextHandler(&buf, nil)})
	lg.Warn("ingest", "accepted", 3, "stored", 2, "note", "/srv/models")
	got := buf.String()
	for _, want := range []string{"accepted=3", "stored=2", "note=/srv/models"} {
		if !strings.Contains(got, want) {
			t.Errorf("line lost %q: %s", want, got)
		}
	}
}

// The wrapper exists for its composition: the address fold has to run before
// the collapse and the cap, or a payload after the address splits the redacted
// tag across two lines and the peer address survives the cap unredacted. Field
// alone is covered above; this is the order RedactedField pins.
func TestRedactedFieldRedactsBeforeTheCapAndCollapse(t *testing.T) {
	cases := []struct {
		name, in, want string
		n              int
	}{
		{"off-box peer", "dial tcp 192.168.1.1:8080 refused", "dial tcp remote refused", 40},
		{"loopback keeps its port", "dial tcp 127.0.0.1:54321 refused", "dial tcp loopback:54321 refused", 40},
		{"v6 peer", "dial tcp [2001:db8::1]:443 refused", "dial tcp remote refused", 40},
		// A payload long enough to cut the field must cut it after the fold,
		// never between the words of the redacted tag.
		{"cap after the fold", "10.0.0.5:8000 " + strings.Repeat("y", 60), "remote yyyyy", 12},
	}
	for _, c := range cases {
		got := RedactedField(c.in, c.n)
		if got != c.want {
			t.Errorf("%s: RedactedField(%q, %d) = %q, want %q", c.name, c.in, c.n, got, c.want)
		}
		for _, ip := range []string{"192.168.1.1", "10.0.0.5", "127.0.0.1", "2001:db8::1"} {
			if strings.Contains(got, ip) {
				t.Errorf("%s: RedactedField(%q, %d) = %q still carries %s", c.name, c.in, c.n, got, ip)
			}
		}
	}
}
