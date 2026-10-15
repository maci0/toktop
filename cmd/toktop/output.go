package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Output selection for the CLI Spec. auto is text on a terminal and JSON
// when stdout is not one. An explicit text or json value wins either way.
// --json is the same choice as --output json.

const (
	exitFailure     = 1
	exitUsage       = 2
	exitTTYRequired = 4
)

type outputSpec struct {
	mode     string
	explicit bool
	json     bool
}

func (s *outputSpec) apply(mode string) error {
	switch mode {
	case "auto", "text", "json":
	default:
		return fmt.Errorf("--output must be auto, text, or json, not %q", mode)
	}
	if s.explicit && s.mode != "auto" && mode != "auto" && s.mode != mode {
		return fmt.Errorf("--output given twice with different values (%s and %s)", s.mode, mode)
	}
	if s.json && mode == "text" {
		return fmt.Errorf("--json conflicts with --output text")
	}
	if mode != "auto" {
		s.mode = mode
	}
	s.explicit = true
	return nil
}

// peelLeadingOutput takes -o, --output, and --json off the front of args.
// Global flags are accepted before the command word. Flags after that stay
// for the command's own parser, which drops the same spellings.
func peelLeadingOutput(args []string) ([]string, outputSpec, error) {
	spec := outputSpec{mode: "auto"}
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--json":
			if err := spec.apply("json"); err != nil {
				return nil, spec, err
			}
			spec.json = true
			args = args[1:]
		case a == "--output" || a == "-o":
			if len(args) < 2 || outputValueMissing(args[1]) {
				return nil, spec, fmt.Errorf("%s needs a value (auto, text, or json)", longOutput(a))
			}
			if err := spec.apply(args[1]); err != nil {
				return nil, spec, err
			}
			args = args[2:]
		case strings.HasPrefix(a, "--output="):
			if err := spec.apply(strings.TrimPrefix(a, "--output=")); err != nil {
				return nil, spec, err
			}
			args = args[1:]
		case strings.HasPrefix(a, "-o="):
			if err := spec.apply(strings.TrimPrefix(a, "-o=")); err != nil {
				return nil, spec, err
			}
			args = args[1:]
		default:
			return args, spec, nil
		}
	}
	return args, spec, nil
}

func outputValueMissing(v string) bool {
	return v == "" || (strings.HasPrefix(v, "-") && v != "-")
}

func longOutput(flagName string) string {
	if flagName == "-o" {
		return "--output"
	}
	return flagName
}

// dropOutputFlags removes output flags wherever they sit, so a subcommand
// that parses its own FlagSet does not reject a global -o written after it.
// The value is checked and then discarded: those commands do not change
// their success bytes for the flag.
func dropOutputFlags(args []string) ([]string, error) {
	spec := outputSpec{mode: "auto"}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			return rest, nil
		case a == "--json":
			if err := spec.apply("json"); err != nil {
				return nil, err
			}
			spec.json = true
		case a == "--output" || a == "-o":
			if i+1 >= len(args) || outputValueMissing(args[i+1]) {
				return nil, fmt.Errorf("%s needs a value (auto, text, or json)", longOutput(a))
			}
			if err := spec.apply(args[i+1]); err != nil {
				return nil, err
			}
			i++
		case strings.HasPrefix(a, "--output="):
			if err := spec.apply(strings.TrimPrefix(a, "--output=")); err != nil {
				return nil, err
			}
		case strings.HasPrefix(a, "-o="):
			if err := spec.apply(strings.TrimPrefix(a, "-o=")); err != nil {
				return nil, err
			}
		default:
			rest = append(rest, a)
		}
	}
	return rest, nil
}

// absorbOutput reads -o, --output, and --json wherever they sit and folds
// them into spec. A parse failure still has to know whether the failure
// itself is reported as text or as the JSON envelope.
func absorbOutput(spec outputSpec, args []string) (outputSpec, error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return spec, nil
		case a == "--json":
			if err := spec.apply("json"); err != nil {
				return spec, err
			}
			spec.json = true
		case a == "--output" || a == "-o":
			if i+1 >= len(args) || outputValueMissing(args[i+1]) {
				return spec, fmt.Errorf("%s needs a value (auto, text, or json)", longOutput(a))
			}
			if err := spec.apply(args[i+1]); err != nil {
				return spec, err
			}
			i++
		case strings.HasPrefix(a, "--output="):
			if err := spec.apply(strings.TrimPrefix(a, "--output=")); err != nil {
				return spec, err
			}
		case strings.HasPrefix(a, "-o="):
			if err := spec.apply(strings.TrimPrefix(a, "-o=")); err != nil {
				return spec, err
			}
		}
	}
	return spec, nil
}

// mergeOutput combines a leading -o/--json with the flags the top-level
// FlagSet parsed. --json forces json. Two explicit formats that disagree
// are a usage error.
func mergeOutput(spec outputSpec, f *cliFlags, explicit map[string]bool) (string, error) {
	mode := "auto"
	set := false
	if spec.explicit {
		mode = spec.mode
		set = true
	}
	if explicit["output"] || explicit["o"] {
		switch f.output {
		case "auto", "text", "json":
		default:
			return "", fmt.Errorf("--output must be auto, text, or json, not %q", f.output)
		}
		if set && mode != "auto" && f.output != "auto" && mode != f.output {
			return "", fmt.Errorf("--output given twice with different values (%s and %s)", mode, f.output)
		}
		if spec.json && f.output == "text" {
			return "", fmt.Errorf("--json conflicts with --output text")
		}
		mode = f.output
		set = true
	}
	if spec.json || explicit["json"] {
		if set && mode != "auto" && mode != "json" {
			return "", fmt.Errorf("--json conflicts with --output %s", mode)
		}
		mode = "json"
	}
	if mode == "" {
		mode = "auto"
	}
	return mode, nil
}

// resolvedFormat applies auto. Stdout is the stream whose terminal-ness
// selects the format, matching the snapshot that is about to be written there.
func resolvedFormat(mode string) string {
	switch mode {
	case "json", "text":
		return mode
	default:
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return "text"
		}
		return "json"
	}
}

type errorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type errorDoc struct {
	Error errorBody `json:"error"`
}

// usageFail reports a usage error and returns its exit code. msg may already
// carry the "toktop: " prefix the other startup lines use; text mode prints
// that prefix once.
func usageFail(mode, msg string) int {
	reportError(mode, "usage", strings.TrimPrefix(msg, "toktop: "), "")
	return exitUsage
}

// compactJSONIfPiped rewrites an indented document as one line when stdout
// is not a terminal. A terminal keeps the indented form. A document that
// does not parse is returned unchanged.
func compactJSONIfPiped(report string) string {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return report
	}
	var doc any
	if err := json.Unmarshal([]byte(report), &doc); err != nil {
		return report
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return report
	}
	return string(b)
}

// reportError writes a usage or runtime failure. Text mode is one human line
// on stderr. JSON mode (explicit, or auto when stdout is not a terminal) is
// the error envelope, and it is the last line on stderr.
func reportError(mode, kind, msg, hint string) {
	if resolvedFormat(mode) == "json" {
		b, err := json.Marshal(errorDoc{Error: errorBody{Kind: kind, Message: msg, Hint: hint}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "toktop: %s\n", msg)
			return
		}
		fmt.Fprintln(os.Stderr, string(b))
		return
	}
	fmt.Fprintf(os.Stderr, "toktop: %s\n", msg)
}

// writeJSONDoc writes one JSON document. Compact when stdout is not a
// terminal, indented when it is, so a person reading a terminal gets a
// document they can scan and a pipe gets one line.
func writeJSONDoc(w io.Writer, v any) int {
	var (
		b   []byte
		err error
	)
	if term.IsTerminal(int(os.Stdout.Fd())) {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		reportError("json", "failure", err.Error(), "")
		return exitFailure
	}
	_, err = fmt.Fprintln(w, string(b))
	return outputStatus(err)
}
