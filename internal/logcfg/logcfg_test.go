// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package logcfg

import (
	"bytes"
	"context"
	"log/slog"
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

func TestUtcLogTime(t *testing.T) {
	tm := time.Date(2026, 3, 15, 10, 30, 0, 0, time.FixedZone("EST", -5*3600))
	attr := slog.Time(slog.TimeKey, tm)
	got := utcTime(nil, attr)
	if got.Value.Kind() != slog.KindString {
		t.Fatalf("kind = %v, want string", got.Value.Kind())
	}
	want := tm.UTC().Format(time.RFC3339Nano)
	if got.Value.String() != want {
		t.Errorf("got %q, want %q", got.Value.String(), want)
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
