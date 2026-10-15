package main

import (
	"fmt"
	"io"
	"strings"
)

// The CLI Spec document. schema prints it and exits. It reads no config, no
// credentials, and no network: the document is this binary's own description.

func runSchema(out io.Writer, args []string, mode string) int {
	if len(args) > 0 && (isHelpArg(args[0]) || args[0] == "help") {
		if len(args) > 1 {
			return rejectExtra("toktop schema", args[1])
		}
		return outputStatus(usage(out))
	}
	if len(args) > 0 && isVersionArg(args[0]) {
		if len(args) > 1 {
			return rejectExtra("toktop schema", args[1])
		}
		_, err := fmt.Fprintln(out, "toktop", version)
		return outputStatus(err)
	}
	rest, err := dropOutputFlags(args)
	if err != nil {
		reportError(mode, "usage", err.Error(), "see 'toktop --help'")
		return exitUsage
	}
	path := strings.Join(rest, " ")
	doc := newSchema(version)
	if path != "" {
		matched := filterCommands(doc.Commands, path)
		if len(matched) == 0 {
			reportError(mode, "usage", fmt.Sprintf("no command %q", path), "run 'toktop schema' for the command list")
			return exitUsage
		}
		doc.Commands = matched
	}
	return writeJSONDoc(out, doc)
}

func filterCommands(cmds []schemaCommand, path string) []schemaCommand {
	var out []schemaCommand
	prefix := path + " "
	for _, c := range cmds {
		if c.Name == path || strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

type schemaDoc struct {
	Clispec     string          `json:"clispec"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Output      schemaOutput    `json:"output"`
	GlobalArgs  []schemaArg     `json:"global_args"`
	Commands    []schemaCommand `json:"commands"`
	Errors      []schemaError   `json:"errors"`
}

type schemaOutput struct {
	TTY   string `json:"tty"`
	Piped string `json:"piped"`
}

type schemaArg struct {
	Name        string   `json:"name"`
	Short       string   `json:"short,omitempty"`
	Type        string   `json:"type"`
	Enum        []string `json:"enum,omitempty"`
	Default     any      `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

type schemaCommand struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Effects      string        `json:"effects"`
	OutputKind   string        `json:"output_kind,omitempty"`
	MediaType    string        `json:"media_type,omitempty"`
	Cardinality  string        `json:"cardinality,omitempty"`
	RequiresTTY  bool          `json:"requires_tty,omitempty"`
	Args         []schemaArg   `json:"args,omitempty"`
	OutputFields []schemaField `json:"output_fields,omitempty"`
	Errors       []string      `json:"errors,omitempty"`
}

type schemaField struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Nullable    bool            `json:"nullable,omitempty"`
	Description string          `json:"description,omitempty"`
	Items       *schemaTypeNode `json:"items,omitempty"`
	Fields      []schemaField   `json:"fields,omitempty"`
}

type schemaTypeNode struct {
	Type     string          `json:"type"`
	Nullable bool            `json:"nullable,omitempty"`
	Items    *schemaTypeNode `json:"items,omitempty"`
	Fields   []schemaField   `json:"fields,omitempty"`
}

type schemaError struct {
	Kind        string `json:"kind"`
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

func newSchema(ver string) schemaDoc {
	engineItems := &schemaTypeNode{
		Type: "object",
		Fields: []schemaField{
			{Name: "label", Type: "string"},
			{Name: "addr", Type: "string"},
			{Name: "ok", Type: "boolean"},
			{Name: "out_tok_per_s", Type: "number"},
			{Name: "in_tok_per_s", Type: "number"},
		},
	}
	return schemaDoc{
		Clispec:     "0.3",
		Name:        "toktop",
		Version:     ver,
		Description: "Dashboard for LLM inference engines and the agents calling them.",
		Output:      schemaOutput{TTY: "text", Piped: "json"},
		GlobalArgs: []schemaArg{
			{
				Name:        "--output",
				Short:       "-o",
				Type:        "string",
				Enum:        []string{"auto", "text", "json"},
				Default:     "auto",
				Description: "Output format. auto is text on a terminal and JSON otherwise. An explicit value always wins.",
			},
			{
				Name:        "--json",
				Type:        "boolean",
				Description: "Same as --output json.",
			},
			{Name: "--help", Short: "-h", Type: "boolean", Description: "Show help and exit."},
			{Name: "--version", Short: "-v", Type: "boolean", Description: "Print the version and exit."},
		},
		Commands: []schemaCommand{
			{
				Name:        "dashboard",
				Description: "Full-screen UI, the run with no subcommand. Needs a terminal. Without one it refuses with tty_required instead of painting control sequences or waiting.",
				Effects:     "read_only",
				OutputKind:  "opaque",
				MediaType:   "text/plain",
				RequiresTTY: true,
				Args:        snapshotArgs(),
				Errors:      []string{"usage", "tty_required", "failure", "interrupted"},
			},
			{
				Name:        "snapshot",
				Description: "Print one snapshot of the readings the dashboard collects, then exit. Same run as --once. Text on a terminal, JSON when stdout is not a terminal, unless -o says otherwise. The JSON document is one object, not a list. A retry reads again and does not change the engines.",
				Effects:     "read_only",
				Cardinality: "single",
				Args:        snapshotArgs(),
				OutputFields: []schemaField{
					{Name: "schema", Type: "integer", Description: "Report shape revision. Not the CLI Spec version."},
					{Name: "version", Type: "string"},
					{Name: "demo", Type: "boolean"},
					{Name: "demo_seed", Type: "integer", Nullable: true},
					{Name: "demo_origin", Type: "string"},
					{Name: "at", Type: "string"},
					{Name: "uptime_secs", Type: "number"},
					{Name: "engines_up", Type: "integer"},
					{Name: "engines", Type: "array", Items: engineItems},
					{Name: "out_tok_per_s", Type: "number"},
					{Name: "in_tok_per_s", Type: "number"},
					{Name: "agents", Type: "array", Items: &schemaTypeNode{Type: "object"}},
					{Name: "probes", Type: "array", Items: &schemaTypeNode{Type: "object"}},
					{Name: "system", Type: "object", Nullable: true},
					{Name: "agent_rates", Type: "array", Items: &schemaTypeNode{Type: "object"}},
				},
				Errors: []string{"usage", "failure", "interrupted"},
			},
			{
				Name:         "schema",
				Description:  "Print this CLI Spec document as one JSON object and exit. Ignores the output flag on success: the document is JSON in every mode. Needs no config, credentials, or network.",
				Effects:      "read_only",
				Cardinality:  "single",
				OutputFields: schemaSelfFields(),
				Errors:       []string{"usage"},
			},
			{
				Name:        "version",
				Description: "Print the version line. The line is the artifact; -o does not reencode it.",
				Effects:     "read_only",
				OutputKind:  "opaque",
				MediaType:   "text/plain",
				Errors:      []string{"usage"},
			},
			{
				Name:        "help",
				Description: "Print the help screen. The screen is the artifact; -o does not reencode it.",
				Effects:     "read_only",
				OutputKind:  "opaque",
				MediaType:   "text/plain",
				Args:        []schemaArg{{Name: "topic", Type: "string", Description: "update, version, completion, schema, snapshot, or dashboard"}},
				Errors:      []string{"usage"},
			},
			{
				Name:        "completion",
				Description: "Print a shell completion script. The script is the artifact; -o does not reencode it.",
				Effects:     "read_only",
				OutputKind:  "opaque",
				MediaType:   "text/plain",
				Args:        []schemaArg{{Name: "shell", Type: "string", Required: true, Enum: []string{"bash", "zsh", "fish"}}},
				Errors:      []string{"usage"},
			},
			{
				Name:        "update",
				Description: "Install the latest release, or with --check print its URL and install nothing. Repeating a run that is already current exits 0 and changes nothing. A failed download can be retried; there is no separate idempotency key because the requested state is the current release.",
				Effects:     "idempotent",
				OutputKind:  "opaque",
				MediaType:   "text/plain",
				Args: []schemaArg{
					{Name: "--check", Type: "boolean", Description: "Print the release URL and install nothing."},
					{Name: "--repo", Type: "string", Description: "GitHub repository owner/name.", Default: "maci0/toktop"},
				},
				Errors: []string{"usage", "failure", "interrupted"},
			},
		},
		Errors: []schemaError{
			{Kind: "usage", ExitCode: exitUsage, Retryable: false, Description: "The invocation was wrong. Nothing was attempted."},
			{Kind: "failure", ExitCode: exitFailure, Retryable: true, Description: "The run could not collect a snapshot or write its result."},
			{Kind: "tty_required", ExitCode: exitTTYRequired, Retryable: false, Description: "The full-screen UI needs a terminal and stdout is not one."},
			{Kind: "interrupted", ExitCode: 130, Retryable: true, Description: "SIGINT or SIGTERM stopped the run."},
		},
	}
}

func schemaSelfFields() []schemaField {
	return []schemaField{
		{Name: "clispec", Type: "string"},
		{Name: "name", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "output", Type: "object"},
		{Name: "global_args", Type: "array", Items: &schemaTypeNode{Type: "object"}},
		{Name: "commands", Type: "array", Items: &schemaTypeNode{Type: "object"}},
		{Name: "errors", Type: "array", Items: &schemaTypeNode{Type: "object"}},
	}
}

func snapshotArgs() []schemaArg {
	return []schemaArg{
		{Name: "--demo", Type: "boolean", Description: "Simulated fleet, no engines and no network."},
		{Name: "--once", Type: "boolean", Description: "Same run as the snapshot command."},
		{Name: "--plain", Type: "boolean", Description: "Linear text report. Ignored when the snapshot is JSON."},
		{Name: "--frames", Type: "integer", Description: "Snapshots to wait for before printing. JSON and plain print the last one."},
		{Name: "--interval", Type: "duration", Description: "Poll interval."},
		{Name: "--add", Type: "url", Description: "OpenAI-compatible endpoint. Repeatable."},
		{Name: "--agents", Type: "boolean"},
		{Name: "--no-ingest", Type: "boolean"},
		{Name: "--probe", Type: "integer"},
		{Name: "--seed", Type: "integer"},
		{Name: "--origin", Type: "string"},
		{Name: "--ssh-key", Type: "path"},
		{Name: "--bearer", Type: "string", Description: "Bearer token for --add. Prefer TOKTOP_BEARER or OMNIROUTE_API_KEY."},
		{Name: "target", Type: "string", Description: "ssh://[user@]host[:port], repeatable. No password in the URL."},
	}
}
