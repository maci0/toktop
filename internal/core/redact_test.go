package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactHomeFoldsEveryOccurrence(t *testing.T) {
	home := absPath("home", "private-user")
	setHome(t, home)
	msg := "open " + filepath.Join(home, "bin", ".toktop") + ": permission denied; retry " +
		filepath.Join(home, "bin", "toktop")
	got := RedactHome(msg)
	if strings.Contains(got, home) || strings.Contains(got, "private-user") {
		t.Fatalf("RedactHome(%q) = %q; the home directory survived", msg, got)
	}
	if n := strings.Count(got, "~"); n != 2 {
		t.Errorf("RedactHome(%q) = %q, want both paths folded", msg, got)
	}
}

func TestRedactHomeLeavesOtherPathsAlone(t *testing.T) {
	home := absPath("home", "private-user")
	setHome(t, home)
	for _, msg := range []string{
		"checksum mismatch",
		filepath.Join(string(filepath.Separator)+"srv", "engines", "model.safetensors") + ": truncated",
	} {
		if got := RedactHome(msg); got != msg {
			t.Errorf("RedactHome(%q) = %q, want it unchanged", msg, got)
		}
	}
}

// The match is textual, not a path resolution: an unnormalized message that
// literally begins with home+separator has that prefix folded, and the ".."
// tail is left exactly as written rather than normalized to a real directory
// the redaction never proved anything about.
func TestRedactHomeFoldsUnnormalizedPrefixWithoutCleaningIt(t *testing.T) {
	home := absPath("home", "private-user")
	setHome(t, home)
	sep := string(filepath.Separator)
	msg := home + sep + ".." + sep + "etc" + sep + "hosts"
	want := "~" + sep + ".." + sep + "etc" + sep + "hosts"
	if got := RedactHome(msg); got != want {
		t.Errorf("RedactHome(%q) = %q, want %q", msg, got, want)
	}
}

// A message that is exactly the home directory carries the same account name
// as a path under it, and the separator-terminated match does not reach it. A
// sibling whose name merely starts with the home's is a different directory
// and must survive verbatim.
func TestRedactHomeFoldsBareHomeAndNotASibling(t *testing.T) {
	home := absPath("home", "private-user")
	setHome(t, home)
	if got := RedactHome(home); got != "~" {
		t.Errorf("RedactHome(%q) = %q, want %q", home, got, "~")
	}
	sibling := home + "-old"
	msg := sibling + string(filepath.Separator) + "hosts"
	if got := RedactHome(msg); got != msg {
		t.Errorf("RedactHome(%q) = %q, want it unchanged", msg, got)
	}
}

func TestRedactHomeFoldsCaseOnCaseInsensitivePlatforms(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("only Windows and macOS look names up without regard to case")
	}
	home := absPath("Users", "Me")
	setHome(t, home)
	other := absPath("users", "me", "bin")
	if got := RedactHome("cannot write " + other); strings.Contains(got, "me"+string(filepath.Separator)) {
		t.Errorf("RedactHome(%q) = %q, want the cased spelling folded too", other, got)
	}
	// The bare-home shortcut compares folded too, so a home spelled in another
	// case still collapses to "~" rather than surviving as a full account name.
	if got := RedactHome(strings.ToLower(home)); got != "~" {
		t.Errorf("RedactHome(%q) = %q, want %q", strings.ToLower(home), got, "~")
	}
}

// One Windows directory has two spellings, and a path in a message can carry
// either: a user-supplied argument, an ssh target, or a tool built for another
// platform all spell it with '/'. The platform separator alone would leave the
// account name in the message.
func TestRedactHomeFoldsBothWindowsSeparators(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows names one directory with either separator")
	}
	home := absPath("Users", "Me")
	setHome(t, home)
	msg := "cannot write " + filepath.ToSlash(filepath.Join(home, "bin", "toktop"))
	if got := RedactHome(msg); strings.Contains(got, "Me") {
		t.Errorf("RedactHome(%q) = %q, want the forward-slash spelling folded too", msg, got)
	}
	if got := RedactHome(filepath.ToSlash(home)); got != "~" {
		t.Errorf("RedactHome(%q) = %q, want %q", filepath.ToSlash(home), got, "~")
	}
}

func TestRedactHomeRootHomeIsNotFolded(t *testing.T) {
	root := string(filepath.Separator)
	setHome(t, root)
	// A message holding "//" is what the root guard is for: with home "/", the
	// needle is "//" and an unguarded rewrite would turn every URL in a
	// diagnostic into "http:~/host", losing the address the operator needs.
	for _, msg := range []string{
		"cannot reach http://host:8000/v1/models",
		filepath.Join(root, "srv", "engines"),
	} {
		if got := RedactHome(msg); got != msg {
			t.Errorf("RedactHome(%q) = %q; a root home would swallow every path", msg, got)
		}
	}
}

// foldCase is one replaceFold expectation, named so the fold cases built from
// a code point below can join the same table.
type foldCase struct{ in, old, new, want string }

func TestReplaceFold(t *testing.T) {
	// KELVIN and DOTTED_I are spelled by code point on purpose. Both fold
	// against an ASCII letter, both are multi-byte, and both look like ASCII
	// in an editor: written as literals they quietly reduce to the ASCII cases
	// that already pass, which is how a comment can claim to cover U+212A
	// while the test runs on "K".
	const (
		kelvin  = string(rune(0x212A))
		dottedI = string(rune(0x130))
	)
	cases := []foldCase{
		{"cannot write C:\\Users\\me\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"cannot write c:\\users\\me\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"cannot write C:\\USERS\\ME\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"no home here", "C:\\Users\\me\\", "~/", "no home here"},
		{"/Users/me/a /Users/me/b", "/Users/me/", "~/", "~/a ~/b"},
		// A different home must not be folded into this one.
		{"/home/maria/x", "/Users/me/", "~/", "/home/maria/x"},
		// Two spellings that fold together need not span the same bytes, so
		// neither the match offset nor the resume point can be taken from the
		// pattern. KELVIN is three bytes against "k"'s one: resuming by
		// len(old) splits it in half, and with the pattern carrying the wide
		// rune instead it runs the slice end past the string entirely.
		{kelvin + ":\\x " + kelvin + ":\\y", "k:\\", "~/", "~/x ~/y"},
		{"k:\\x k:\\y", kelvin + ":\\", "~/", "~/x ~/y"},
		// The fold is simple case folding, the rule strings.EqualFold applies
		// and the rule RedactHome's bare-home comparison uses further down, so
		// the two cannot disagree about which string is the home directory.
		// unicode.ToLower is a different mapping: it sends DOTTED_I to "i",
		// folding a path component the file systems behind these two
		// platforms keep apart.
		{dottedI + ":\\x", "i:\\", "~/", dottedI + ":\\x"},
	}
	for _, c := range cases {
		got := replaceFold(c.in, c.old, c.new)
		if got != c.want {
			t.Errorf("replaceFold(%q, %q, %q) = %q, want %q", c.in, c.old, c.new, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("replaceFold(%q, %q, %q) = %q, which is not valid UTF-8", c.in, c.old, c.new, got)
		}
	}
}

// The peer's account, not the local one: the local home is folded by
// RedactHome, so what a remote command reports about itself has to be folded
// by name. The home directory is spelled the way each of the systems a remote
// login lands on spells one, and none of them knows which one applies here.
func TestRedactUserHomeFoldsTheNamedAccountsHome(t *testing.T) {
	cases := []struct{ user, in, want string }{
		{"me", "/home/me/.bashrc: No such file", "~/.bashrc: No such file"},
		{"me", "cd /home/me", "cd ~"},
		{"me", "/home/me", "~"},
		{"me", `/Users/me\bin: access denied`, `~\bin: access denied`},
		{"me", `C:\Users\me\bin: access denied`, `~\bin: access denied`},
		// The peer's platform is not the local one, so the fold does not
		// depend on the local file system's case rules: a home spelled in
		// another case names the same account and is folded too.
		{"me", "/home/ME/.bashrc", "~/.bashrc"},
		// A longer name starting with the account is another account, and
		// folding it would hide a directory the message was about.
		{"me", "/home/mem/.bashrc", "/home/mem/.bashrc"},
		{"me", "/home/me-too/.bashrc", "/home/me-too/.bashrc"},
		{"me", "/srv/engines/model: truncated", "/srv/engines/model: truncated"},
		// A home under a Windows volume whose match is left alone (a longer
		// account name) keeps the volume: the branch that copies the match
		// back verbatim must not swallow "C:" on its way there, or the
		// message names a drive-relative path that was never in it.
		{"me", `C:\Users\me-too\x`, `C:\Users\me-too\x`},
		{"me", `C:\Users\mem\x`, `C:\Users\mem\x`},
		{"me", `copy C:\Users\me-a to C:\Users\me\b`, `copy C:\Users\me-a to ~\b`},
		// No account, nothing to fold: the caller has no user for the target
		// (a bare host, a ~/.ssh/config entry toktop did not read a User from).
		{"", "/home/me/.bashrc", "/home/me/.bashrc"},
		// A name carrying a separator is not one path component, and folding
		// on it would rewrite text the account never named.
		{"../me", "/home/../me/x", "/home/../me/x"},
		{"..", "/home/../x", "/home/../x"},
		{"C:", `/Users/C:/x`, `/Users/C:/x`},
	}
	for _, c := range cases {
		if got := RedactUserHome(c.user, c.in); got != c.want {
			t.Errorf("RedactUserHome(%q, %q) = %q, want %q", c.user, c.in, got, c.want)
		}
	}
}

func TestRedactUserHomeFoldsEveryOccurrence(t *testing.T) {
	msg := "read /home/me/.bashrc, write /home/me/.profile"
	if got := RedactUserHome("me", msg); strings.Contains(got, "/home/me") {
		t.Errorf("RedactUserHome(%q) = %q, want every occurrence folded", msg, got)
	}
}

// The account is read off the path, so a home naming any login folds without
// the caller knowing the name, and a path that names no account is left alone.
// The home is the one a client posts, never the one toktop runs as, so none of
// these cases set the local home.
func TestRedactAnyUserHomeFoldsAnyAccount(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/asmith/.bashrc: No such file", "~/.bashrc: No such file"},
		{"failed in /var/home/bchen/proj", "failed in ~/proj"},
		{"read /nfs/home/rpatel/x, write /srv/homes/rmora/y", "read ~/x, write ~/y"},
		{`/Users/dana/bin: access denied`, `~/bin: access denied`},
		{`C:\Users\eli\bin: access denied`, `~\bin: access denied`},
		// The peer's platform is not the local one, so the fold does not
		// depend on the local file system's case rules.
		{"/home/ASMITH/.bashrc", "~/.bashrc"},
		{"/export/home/jo/.config/x", "~/.config/x"},
		{"/srv/engines/model: truncated", "/srv/engines/model: truncated"},
		{"checksum mismatch", "checksum mismatch"},
		// A directory that happens to be spelled "home" is not one, and
		// neither is a file in it: only a component spelled like a login name
		// is an account.
		{"x/home/me", "x/home/me"},
		{"/home/README.md", "~"},
		{"/home/.config/toktop/x", "/home/.config/toktop/x"},
		{"/home/my files/x", "/home/my files/x"},
		{"/home/", "/home/"},
		// A name longer than any login a system toktop runs on grants is a
		// file, not an account.
		{"/home/" + strings.Repeat("n", maxAccountNameLen+1) + ".x", "/home/" + strings.Repeat("n", maxAccountNameLen+1) + ".x"},
	}
	for _, c := range cases {
		if got := RedactAnyUserHome(c.in); got != c.want {
			t.Errorf("RedactAnyUserHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactAnyUserHomeFoldsEveryOccurrence(t *testing.T) {
	msg := "read /home/asmith/.bashrc, write /home/asmith/.profile"
	if got := RedactAnyUserHome(msg); strings.Contains(got, "asmith") {
		t.Errorf("RedactAnyUserHome(%q) = %q, want every occurrence folded", msg, got)
	}
}

// absPath builds an absolute path under a fake root, spelled the way this
// platform spells one. Windows paths need a volume, so "\home\private-user" is
// drive-relative there and RedactHome rightly leaves it alone; a test that
// wants a folded path has to name one the platform calls absolute.
func absPath(parts ...string) string {
	root := string(filepath.Separator)
	if vol := filepath.VolumeName(os.TempDir()); vol != "" {
		root = vol + string(filepath.Separator)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// setHome points UserHomeDir at a path under the test's own temp dir, which
// must not itself live under the real home: the platform resolves the home
// from different variables.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("home", home)
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Skipf("cannot redirect the home directory (got %q, %v)", got, err)
	}
}
