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
// completes toktop's flags, subcommands and ssh:// targets, so the shell and
// the binary cannot drift apart. Nothing here writes to the filesystem, so
// installing is the operator's one command, shown at the bottom of every
// script and in the subcommand's help.

// completionShells are the shells a script is published for, in the order the
// help screen lists them.
var completionShells = []string{"bash", "zsh", "fish"}

// completionSubs are the words that select a subcommand, so a shell can offer
// them before the first character of a flag is typed.
var completionSubs = []string{"completion", "help", "update", "version"}

// updateFlags are the flags `toktop update` owns. They live on the subcommand
// rather than on the top-level FlagSet, so the top-level list cannot name them.
var updateFlags = []string{"--check", "--help", "--repo", "--version"}

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
// aliases a shell completes as part of their long flag anyway. The list is
// read from the FlagSet rather than written out here, so a flag added to
// flags.go is completed the day it lands.
func topFlags() []string {
	registerFlags()
	var names []string
	topFS.VisitAll(func(f *flag.Flag) {
		if isFlagAlias(f.Name) {
			return
		}
		names = append(names, "--"+f.Name)
	})
	slices.Sort(names)
	return names
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
    local cur prev i
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    local topflags="%s"
    local subs="%s"
    local upflags="%s"
    for ((i = 1; i < COMP_CWORD; i++)); do
        if [ "$i" -eq 1 ]; then
            case "${COMP_WORDS[i]}" in
                completion|help|update|version)
                    case "$cur" in
                        -*) COMPREPLY=($(compgen -W "$upflags" -- "$cur")) ;;
                        *) COMPREPLY=() ;;
                    esac
                    return 0
                    ;;
            esac
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
		strings.Join(updateFlags, " "), fileCaseArm(fs, flags, "            COMPREPLY=($(compgen -f -- \"$cur\"))\n"))
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
    local -a topflags subs upflags
    topflags=(%s)
    subs=(%s)
    upflags=(%s)
    # words[2] rather than CURRENT: a subcommand is only ever the second word,
    # and the flags that follow it are that subcommand's, whatever is being
    # completed. Testing CURRENT instead answered with the top-level flags
    # for every word past the subcommand.
    case ${words[2]} in
        completion|help|update|version)
            if [[ ${words[CURRENT]} == -* ]]; then
                _describe -t flags 'flag' upflags
            fi
            return
            ;;
    esac
    case ${words[CURRENT-1]} in
%s    esac
    if [[ ${words[CURRENT]} == -* ]]; then
        _describe -t flags 'flag' topflags
    else
        _describe -t commands 'command' subs
    fi
}
compdef _toktop toktop
`, zshList(flags), zshList(completionSubs), zshList(updateFlags), zshFileArm(fs, flags))
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

// fishCompletion uses fish's own `complete`: -f says the flag takes no value,
// -r that it takes one, and -F that the value is a path.
func fishCompletion(fs *flag.FlagSet, flags []string) string {
	var b strings.Builder
	b.WriteString("# fish completion for toktop\n")
	for _, name := range flags {
		long := strings.TrimPrefix(name, "--")
		var spec string
		switch {
		case pathFlags[name]:
			spec = "-l " + long + " -r -F"
		case takesValue(fs, name):
			spec = "-l " + long + " -r -f"
		default:
			spec = "-l " + long + " -f"
		}
		fmt.Fprintf(&b, "complete -c toktop %s\n", spec)
	}
	for _, sub := range completionSubs {
		fmt.Fprintf(&b, "complete -c toktop -f -n \"__fish_use_subcommand\" -a %s\n", sub)
	}
	return b.String()
}
