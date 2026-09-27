package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRedactHomeFoldsEveryOccurrence(t *testing.T) {
	home := filepath.Join(string(filepath.Separator)+"home", "private-user")
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
	home := filepath.Join(string(filepath.Separator)+"home", "private-user")
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
	home := filepath.Join(string(filepath.Separator)+"home", "private-user")
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
	home := filepath.Join(string(filepath.Separator)+"home", "private-user")
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
	home := filepath.Join(string(filepath.Separator), "Users", "Me")
	setHome(t, home)
	other := filepath.Join(string(filepath.Separator), "users", "me", "bin")
	if got := RedactHome("cannot write " + other); strings.Contains(got, "me"+string(filepath.Separator)) {
		t.Errorf("RedactHome(%q) = %q, want the cased spelling folded too", other, got)
	}
	// The bare-home shortcut compares folded too, so a home spelled in another
	// case still collapses to "~" rather than surviving as a full account name.
	if got := RedactHome(strings.ToLower(home)); got != "~" {
		t.Errorf("RedactHome(%q) = %q, want %q", strings.ToLower(home), got, "~")
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

func TestReplaceFold(t *testing.T) {
	cases := []struct{ in, old, new, want string }{
		{"cannot write C:\\Users\\me\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"cannot write c:\\users\\me\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"cannot write C:\\USERS\\ME\\bin", "C:\\Users\\me\\", "~/", "cannot write ~/bin"},
		{"no home here", "C:\\Users\\me\\", "~/", "no home here"},
		{"/Users/me/a /Users/me/b", "/Users/me/", "~/", "~/a ~/b"},
		// A different home must not be folded into this one.
		{"/home/maria/x", "/Users/me/", "~/", "/home/maria/x"},
		// K (U+212A) folds to "k" and is three bytes wide; a search over
		// lowercased copies would land on the wrong offset here.
		{"K:\\x K:\\y", "k:\\", "~/", "~/x ~/y"},
	}
	for _, c := range cases {
		if got := replaceFold(c.in, c.old, c.new); got != c.want {
			t.Errorf("replaceFold(%q, %q, %q) = %q, want %q", c.in, c.old, c.new, got, c.want)
		}
	}
}

// setHome points UserHomeDir at a path under the test's own temp dir, which
// must not itself live under the real home: the platform resolves the home
// from different variables.
func setHome(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		home = `C:\` + filepath.ToSlash(home)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("home", home)
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Skipf("cannot redirect the home directory (got %q, %v)", got, err)
	}
}
