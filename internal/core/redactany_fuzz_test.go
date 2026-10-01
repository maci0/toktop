// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzRedactAnyUserHome drives the account-agnostic home redactor with
// arbitrary text. It is the redactor the ingest path runs on every field a
// remote client posts (RedactAnyUserHome wraps RedactHome in
// internal/ingest's scrub), so the text is whatever sender chose: a note, an
// error, an agent's working directory, a model id. The account is read off
// the path rather than supplied, so the scan is the only thing between a
// posted string and a report that names every account on the host.
//
// The scan walks runes under case folding, which is where a redaction parser
// breaks: a wide rune the fold matches at one width and not another can leave
// the scan between a rune's bytes, and a replacement length that disagrees
// with the text it replaced can duplicate or drop the surrounding bytes. So
// the properties asserted are that a valid input yields a valid output, that
// the result is never longer than the text it was given, that the same text
// always redacts the same way, and that nothing which reads as a home
// followed by an account name survives into the result.
func FuzzRedactAnyUserHome(f *testing.F) {
	for _, seed := range []string{
		"",
		"checksum mismatch",
		"/home/asmith/.bashrc: No such file",
		"failed in /var/home/bchen/proj",
		"read /nfs/home/rpatel/x, write /srv/homes/rmora/y",
		`/Users/dana/bin: access denied`,
		`C:\Users\eli\bin: access denied`,
		"/home/ASMITH/.bashrc",
		"/export/home/jo/.config/x",
		// A directory spelled "home" is not one, and neither is a hidden
		// directory in it: those are the cases a prefix match alone would
		// fold and this one must not.
		"x/home/me",
		"/home/",
		"/home//home/x",
		"/home/.config/toktop/x",
		"/home/my files/x",
		"/home/README.md",
		// One account named twice, and a home spelled two ways in one line.
		"read /home/asmith/.bashrc, write /home/asmith/.profile",
		"/home/me/home/other",
		"/home/meC:/home/me",
		`C:\Users\me-too`,
		`C:\Users\me`,
		"Z:\\Users\\me\\x",
		`/Users\me/x`,
		// Case folding over the runes where ToLower and EqualFold disagree.
		"/home/K/x",
		"/home/\u212a/x",
		"/home/mÉ",
		"/home/me\u0301/x",
		// The name-length bound: a run past it is a file, not an account.
		"/home/" + strings.Repeat("n", maxAccountNameLen) + "/x",
		"/home/" + strings.Repeat("n", maxAccountNameLen+1) + "/x",
		// Terminal and control bytes reaching a field that gets drawn.
		"\x1b]0;/home/me\x07title",
		"/home/me\x00\x01\x7f",
		"/home/me\n/home/me",
		// Malformed UTF-8 around a home, and a wide rune the scan must not
		// split.
		"\xff\xfe/home/me/\xc0\x80",
		"/home/mé\xff/x",
		// Long runs, which is what an over-cap input looks like.
		strings.Repeat("/home/me/", 64),
		strings.Repeat("/home/", 200) + "x",
		strings.Repeat("a", 4096) + "/home/me",
		"{broken",
		// Text that already carries a tilde: a message from an agent that
		// spells a folded path must come through unchanged, and a "~" the
		// input brought is not one the redaction wrote.
		"~",
		"~/.claude/config.json",
		"~ not a home /home/me",
		"~~/home/me~~",
		"/home/~/x",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, msg string) {
		got := RedactAnyUserHome(msg)
		if utf8.ValidString(msg) && !utf8.ValidString(got) {
			t.Fatalf("RedactAnyUserHome(%s) split a rune: %s", brief(msg), brief(got))
		}
		if again := RedactAnyUserHome(msg); again != got {
			t.Fatalf("RedactAnyUserHome(%s) is not deterministic: %s then %s", brief(msg), brief(got), brief(again))
		}
		// Every fold replaces a prefix and an account name with one byte, so
		// the result can only shrink. A result longer than the text it was
		// given means the resume offset lost a byte and copied text twice.
		if len(got) > len(msg) {
			t.Fatalf("RedactAnyUserHome grew the text: %d -> %d (%s from %s)", len(msg), len(got), brief(got), brief(msg))
		}
		// Redacting text that has already been redacted is the same answer:
		// the feed redacts on the way in and again on the way out, so a
		// second pass that folds differently would show a field changing
		// under a viewer that never touched it.
		if twice := RedactAnyUserHome(got); twice != got {
			t.Fatalf("RedactAnyUserHome is not idempotent: %s -> %s -> %s", brief(msg), brief(got), brief(twice))
		}
		assertNoHomeAccountSurvives(t, msg, got)
		assertNoHomeInvented(t, msg, got)
	})
}

// assertNoHomeInvented is the other half of the contract, and the half that
// decides whether the redaction is useful at all: a fold may only remove text
// the input actually spelled as an account under a real path boundary.
// Over-folding is not a cosmetic bug, it destroys the path an operator needs
// to find the project ("failed in /var/home/bchen/proj" becoming
// "failed in /var~/proj" names neither the account nor the directory), and a
// prefix that is really the tail of a longer word ("x/home/me") is not a home
// at all.
//
// A "~" the input already carried is not one the redaction wrote: a message
// arriving from an agent that already spells a folded path "~/.claude" is left
// alone, and counting its tilde as a fold would fail every such message. So
// the input's own tildes are counted first and only the ones the output has
// beyond that are the redaction's to account for. That is the one property a
// redaction has to have regardless of how it decides what to fold, and it
// covers the case where the input carried no tilde at all, which is the case
// that says anything.
func assertNoHomeInvented(t *testing.T, msg, got string) {
	t.Helper()
	tilde := func(s string) int {
		n := 0
		for i := range len(s) {
			if s[i] == '~' {
				n++
			}
		}
		return n
	}
	// A "~" the input already carried is not one the redaction wrote, so the
	// difference is what it added. It can only be negative if the redaction
	// removed a tilde, which is its own failure.
	if added := tilde(got) - tilde(msg); added < 0 {
		t.Fatalf("RedactAnyUserHome(%s) removed a ~ the text carried: %s",
			brief(msg), brief(got))
	} else if added > 0 {
		if !foldIsAccountAt(msg) {
			t.Fatalf("RedactAnyUserHome(%s) wrote %d ~ for text that names no account: %s",
				brief(msg), added, brief(got))
		}
	}
}

// foldIsAccountAt reports whether msg spells an account under a home prefix
// somewhere at all, read the way a person reading the message would: a home
// prefix standing on its own, with a single name-shaped component after it.
// This is deliberately a plainer reading than the fold's own, and it reads
// the prefix with a plain case-folded search rather than the fold's own
// indexFold, so a change to the fold's boundary rules cannot quietly make
// this assertion agree with it.
func foldIsAccountAt(msg string) bool {
	scan := strings.ToLower(normalizeSpelling(msg))
	for _, prefix := range userHomePrefixes {
		lower := strings.ToLower(normalizeSpelling(prefix))
		for at := 0; at+len(lower) <= len(scan); {
			i := strings.Index(scan[at:], lower)
			if i < 0 {
				break
			}
			pos := at + i
			if boundaryAhead(scan, pos) {
				if _, isAccount := accountName(scan[pos+len(lower):]); isAccount {
					return true
				}
			}
			at = pos + 1
		}
	}
	return false
}

// boundaryAhead reports whether the character before a prefix ends a word, so
// the prefix stands on its own. A letter, digit or one of the name punctuation
// ahead of it means the prefix is the tail of a longer name like "x/home" and
// is not a directory.
func boundaryAhead(s string, at int) bool {
	if at == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:at])
	return !(r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
}

// assertNoHomeAccountSurvives checks the property the function exists for: no
// text left in the output reads as a home directory holding an account name.
// A prefix that names no account ("x/home/me", "/home/.config") legitimately
// survives, and a directory named README.md inside a home is folded with the
// home it sits in, so this walks the output with the same two readers the
// fold itself uses rather than looking for substrings.
func assertNoHomeAccountSurvives(t *testing.T, msg, got string) {
	t.Helper()
	for _, prefix := range userHomePrefixes {
		scan := normalizeSpelling(got)
		for {
			at, n, ok := indexFold(scan, prefix)
			if !ok {
				break
			}
			// A prefix ahead of a name character is the tail of a longer
			// word, not a directory, and holds no account.
			if homeNameStartsAt(scan, at) {
				if name, isAccount := accountName(scan[at+n:]); isAccount {
					t.Fatalf("RedactAnyUserHome(%s) left the account %q in %s", brief(msg), name, brief(got))
				}
			}
			scan = scan[at+n:]
		}
	}
}

// brief trims a failing case down to what identifies it. A fuzzer's input is
// routinely kilobytes of a mutation, and a failure that pastes all of it back
// buries the one line that says what went wrong.
func brief(s string) string {
	const limit = 120
	if len(s) <= limit {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q... (%d bytes)", s[:limit], len(s))
}
