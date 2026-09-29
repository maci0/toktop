package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// The `toktop completion <shell>` subcommand: it prints a shell script that
// completes toktop's flags and its subcommands, each subcommand with its own
// flags, so the shell and the binary cannot drift apart. Nothing here writes
// to the filesystem, so installing is the operator's own redirect, the three
// examples in the subcommand's help are the whole of it.

// completionShells are the shells a script is published for, in the order the
// help screen lists them.
var completionShells = []string{"bash", "zsh", "fish"}

// completionSubs are the words that select a subcommand, so a shell can offer
// them before the first character of a flag is typed.
var completionSubs = []string{"completion", "help", "update", "version"}

// plainSubFlags are the flags every subcommand that parses no FlagSet of its
// own accepts: completion, help and version answer --help and --version and
// nothing else. Offering them to `update` as well was a usage error waiting
// to be typed, and offering update's to these was one waiting to be
// completed.
var plainSubFlags = []string{"--help", "--version"}

// plainSubFS declares the flags above so takesValue reads them off a real
// FlagSet rather than a table that has to keep up with the shells.
var plainSubFS = func() *flag.FlagSet {
	fs := flag.NewFlagSet("toktop subcommand", flag.ContinueOnError)
	fs.Bool("help", false, "")
	fs.Bool("version", false, "")
	return fs
}()

// updateFS and updateFlags are the FlagSet `toktop update` parses and the
// flags on it, read rather than written out here, so a flag added in update.go
// is completed the day it lands the way a top-level one is. A flag landing
// only on the top-level list would leave the subcommand's half of the
// completion stale, which is the drift the help screen rules out.
var updateFS, _ = updateFlagSet()

// updateFlags is updateFS read out, held beside it so a script does not
// rebuild the set to name the flags on it.
var updateFlags = flagNames(updateFS)

// flagNames is every flag in a FlagSet, in its long spelling, in the order a
// shell offers them. The -h and -v aliases are skipped: the FlagSet carries
// them as flags of their own, so a flag that took them by name would offer
// "--h", and a shell completing the prefix "--" does not offer a short
// spelling anyway.
func flagNames(fs *flag.FlagSet) []string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if isFlagAlias(f.Name) {
			return
		}
		names = append(names, "--"+f.Name)
	})
	slices.Sort(names)
	return names
}

// completionUsage prints the subcommand's help screen to w.
func completionUsage(w io.Writer) error {
	_, err := io.WriteString(w, `toktop completion - print a shell completion script

Usage:
  toktop completion <`+strings.Join(completionShells, "|")+`>
  toktop completion --help
  toktop completion --version

Examples:
  toktop completion bash > /etc/bash_completion.d/toktop
  toktop completion zsh  > "${fpath[1]}/_toktop"
  toktop completion fish > ~/.config/fish/completions/toktop.fish

The script is printed on stdout and nothing else goes there, so it can be
redirected or inspected before it is installed. It completes the flags this
build actually defines, so it does not go stale as flags change.

The three shells above are the ones a script is published for, and the paths
in the examples are the directories those shells read on a POSIX system. On
Windows the scripts are the same bytes and work under the bash, zsh or fish
you installed (Git Bash, WSL, MSYS2, a package manager), but none of the
example paths exists there: redirect to whatever directory that shell's
completion setup already reads, such as
~/.local/share/bash-completion/completions for a user install under bash.

A missing or unknown shell is a usage error; 'bash' is the one that drops into
a file wherever a completion belongs.
`)
	return err
}

// runCompletion implements `toktop completion <shell>`.
func runCompletion(out io.Writer, args []string) int {
	if len(args) > 0 {
		if isHelpArg(args[0]) {
			if len(args) > 1 {
				return rejectExtra("toktop completion", args[1])
			}
			return outputStatus(completionUsage(out))
		}
		if isVersionArg(args[0]) {
			if len(args) > 1 {
				return rejectExtra("toktop completion", args[1])
			}
			_, err := fmt.Fprintln(out, "toktop", version)
			return outputStatus(err)
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "toktop completion: no shell given (see 'toktop completion --help')")
		completionUsage(os.Stderr)
		return 2
	}
	shell := args[0]
	if len(args) > 1 {
		return rejectExtra("toktop completion", args[1])
	}
	script, ok := completionScript(shell, topFlags(), topFS)
	if !ok {
		fmt.Fprintf(os.Stderr, "toktop completion: unknown shell %q, want one of %s (see 'toktop completion --help')\n",
			shell, strings.Join(completionShells, ", "))
		completionUsage(os.Stderr)
		return 2
	}
	_, err := io.WriteString(out, script)
	return outputStatus(err)
}

// completionScript renders the script for one shell, or false for a shell with
// no published script.
func completionScript(shell string, flags []string, fs *flag.FlagSet) (string, bool) {
	switch shell {
	case "bash":
		return bashCompletion(fs, flags), true
	case "zsh":
		return zshCompletion(fs, flags), true
	case "fish":
		return fishCompletion(fs, flags), true
	}
	return "", false
}

// topFlags is every top-level flag in its long spelling, minus the -h and -v
// aliases a shell does not offer on its own. The list is read from the
// FlagSet rather than written out here, so a flag added to flags.go is
// completed the day it lands.
func topFlags() []string {
	registerFlags()
	return flagNames(topFS)
}

// subFlags is the flag list for one subcommand, or nil for a word that is not
// one. Each subcommand's own flags are offered under that subcommand alone, so
// a completion is never a command line the binary rejects.
func subFlags(sub string) []string {
	switch sub {
	case "update":
		return updateFlags
	case "completion", "help", "version":
		return plainSubFlags
	}
	return nil
}

// pathFlags are the value flags whose value is a filesystem path, so the
// shells offer a file list for them and for nothing else. A flag added to
// flags.go is a path flag once it is named here.
var pathFlags = map[string]bool{"--ssh-key": true}

// takesValue reports whether a long flag spelling takes a value. flagPlaceholder
// already answers that from the flag's type (a boolean takes none), so the
// three shells cannot disagree about which flags need an argument.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(strings.TrimPrefix(name, "--"))
	if f == nil {
		return true
	}
	return flagPlaceholder(f.Name, f) != ""
}

// bashCompletion completes flags by prefix and subcommands as bare words.
func bashCompletion(fs *flag.FlagSet, flags []string) string {
	return fmt.Sprintf(`# bash completion for toktop
# shellcheck disable=SC2207  # compgen output must word-split into COMPREPLY
_toktop() {
    local cur prev i subflags
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    local topflags="%s"
    local subs="%s"
    local plainflags="%s"
    local upflags="%s"
    subflags=""
    for ((i = 1; i < COMP_CWORD; i++)); do
        if [ "$i" -eq 1 ]; then
            case "${COMP_WORDS[i]}" in
                completion|help|version) subflags="$plainflags" ;;
                update) subflags="$upflags" ;;
            esac
            if [ -n "$subflags" ]; then
                case "$cur" in
                    -*) COMPREPLY=($(compgen -W "$subflags" -- "$cur")) ;;
                    *) COMPREPLY=() ;;
                esac
                return 0
            fi
        fi
    done
    case "$prev" in
%s    esac
    if [[ "$cur" == -* ]]; then
        COMPREPLY=($(compgen -W "$topflags $subs" -- "$cur"))
    else
        COMPREPLY=($(compgen -W "$subs" -- "$cur"))
    fi
    return 0
}
complete -F _toktop toktop
`, strings.Join(flags, " "), strings.Join(completionSubs, " "),
		strings.Join(plainSubFlags, " "), strings.Join(updateFlags, " "),
		fileCaseArm(fs, flags, "            COMPREPLY=($(compgen -f -- \"$cur\"))\n"))
}

// fileCaseArm is the `case "$prev"` arm that hands a value to the shell's file
// completion. It is built from pathFlags through the same takesValue test the
// flag list uses, so a path flag is a path flag in every shell at once.
func fileCaseArm(fs *flag.FlagSet, flags []string, body string) string {
	names := pathFlagNames(fs, flags)
	if len(names) == 0 {
		return "        --)\n            ;;\n"
	}
	return "        " + strings.Join(names, "|") + ")\n" + body + "            return 0\n            ;;\n        *)\n            ;;\n"
}

// pathFlagNames is the subset of flags whose value is a filesystem path, so
// the three shells cannot disagree about which ones offer a file list.
func pathFlagNames(fs *flag.FlagSet, flags []string) []string {
	var names []string
	for _, name := range flags {
		if pathFlags[name] && takesValue(fs, name) {
			names = append(names, name)
		}
	}
	return names
}

// zshCompletion completes the same words, and asks zsh for a file list only
// where a flag's value is a path.
func zshCompletion(fs *flag.FlagSet, flags []string) string {
	return fmt.Sprintf(`#compdef toktop
# zsh completion for toktop
_toktop() {
    local -a topflags subs plainflags upflags subflags
    topflags=(%s)
    subs=(%s)
    plainflags=(%s)
    upflags=(%s)
    subflags=()
    # words[2] rather than CURRENT: a subcommand is only ever the second word,
    # and the flags that follow it are that subcommand's, whatever is being
    # completed. Testing CURRENT instead answered with the top-level flags
    # for every word past the subcommand.
    case ${words[2]} in
        completion|help|version) subflags=($plainflags) ;;
        update) subflags=($upflags) ;;
    esac
    if (( ${#subflags} )); then
        if [[ ${words[CURRENT]} == -* ]]; then
            _describe -t flags 'flag' subflags
        fi
        return
    fi
    case ${words[CURRENT-1]} in
%s    esac
    if [[ ${words[CURRENT]} == -* ]]; then
        _describe -t flags 'flag' topflags
    else
        _describe -t commands 'command' subs
    fi
}
compdef _toktop toktop
`, zshList(flags), zshList(completionSubs), zshList(plainSubFlags), zshList(updateFlags), zshFileArm(fs, flags))
}

// zshFileArm is the `case ${words[CURRENT-1]}` arm for a flag whose value is a
// path. It returns rather than falling through, so a file list is never offered
// alongside the flag and subcommand words the branch below would add.
func zshFileArm(fs *flag.FlagSet, flags []string) string {
	names := pathFlagNames(fs, flags)
	if len(names) == 0 {
		return "        --)\n            ;;\n"
	}
	return "        " + strings.Join(names, "|") + ")\n            _files\n            return\n            ;;\n"
}

func zshList(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + w + "'"
	}
	return strings.Join(quoted, " ")
}

// fishCompletion uses fish's own `complete`: -r says the flag takes a value
// and its absence says it takes none, -F that the value is a path, and -f that
// it is not, which is what keeps fish from offering a file list for every
// flag the completion defines. A top-level flag carries -n __fish_use_subcommand
// and a subcommand's flags carry -n __fish_seen_subcommand_from <sub>, so
// `toktop update --<TAB>` offers update's flags and not the top-level ones.
func fishCompletion(fs *flag.FlagSet, flags []string) string {
	var b strings.Builder
	b.WriteString("# fish completion for toktop\n")
	for _, name := range flags {
		fmt.Fprintf(&b, "complete -c toktop %s -n \"__fish_use_subcommand\"\n", fishFlagSpec(fs, name))
	}
	for _, sub := range completionSubs {
		fmt.Fprintf(&b, "complete -c toktop -f -n \"__fish_use_subcommand\" -a %s\n", sub)
	}
	for _, sub := range completionSubs {
		for _, name := range subFlags(sub) {
			fmt.Fprintf(&b, "complete -c toktop -n \"__fish_seen_subcommand_from %s\" %s\n",
				sub, fishFlagSpec(subFlagSet(sub), name))
		}
	}
	return b.String()
}

// fishFlagSpec is the `complete` specification for one flag: its long and short
// spellings, whether it takes a value, and whether that value is a path.
func fishFlagSpec(fs *flag.FlagSet, name string) string {
	spec := ""
	if long, ok := strings.CutPrefix(name, "--"); ok {
		spec = "-l " + long
	} else {
		spec = "-s " + strings.TrimPrefix(name, "-")
	}
	if takesValue(fs, name) {
		if pathFlags[name] {
			return spec + " -r -F"
		}
		return spec + " -r -f"
	}
	return spec + " -f"
}

// subFlagSet is the FlagSet a subcommand's flags are read from, so a flag
// that takes a value is completed as one.
func subFlagSet(sub string) *flag.FlagSet {
	if sub == "update" {
		return updateFS
	}
	return plainSubFS
}
