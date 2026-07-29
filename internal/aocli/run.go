package aocli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/workflow/def"
)

const (
	exitOK       = 0
	exitFindings = 1
	exitError    = 2
)

// topLevelCommand is one row of the root dispatch table. Every top-level verb
// carries the same signature whether or not it uses every argument, so the
// table — not a switch — is what Run walks.
type topLevelCommand struct {
	name string
	run  func(args []string, configRoot string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int
}

// topLevelCommands is the single source of truth for what a top-level command
// is. Run dispatches through it and Commands reports it, so the app binary's
// verb dispatch (main.go, D30) cannot drift from the set this package handles:
// adding a row here is what makes `agent-overflow <verb>` reach it.
var topLevelCommands = []topLevelCommand{
	{name: "help", run: func(_ []string, _ string, _ func(string) (string, bool), stdout, stderr io.Writer) int {
		if err := writeOutput(stdout, rootUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}},
	{name: "workflow", run: func(args []string, configRoot string, _ func(string) (string, bool), stdout, stderr io.Writer) int {
		return runWorkflow(args, configRoot, stdout, stderr)
	}},
	{name: "run", run: func(args []string, _ string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
		return runCommand(args, lookupEnv, stdout, stderr)
	}},
	{name: "notes", run: func(args []string, _ string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
		return notesCommand(args, lookupEnv, stdout, stderr)
	}},
	{name: "schedule", run: func(args []string, _ string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
		return scheduleCommand.run(args, lookupEnv, stdout, stderr)
	}},
}

// Commands names every top-level command, in the order the dispatch table
// declares them. The app binary uses it to decide whether an argv is a CLI
// invocation or a boot, which is the one place a second, hand-maintained copy
// of this list would silently rot.
func Commands() []string {
	names := make([]string, 0, len(topLevelCommands))
	for _, command := range topLevelCommands {
		names = append(names, command.name)
	}
	return names
}

// IsCommand reports whether name is a top-level command this package handles.
func IsCommand(name string) bool {
	for _, command := range topLevelCommands {
		if command.name == name {
			return true
		}
	}
	return false
}

// Usage is the root help text, exported so the app binary can print it when it
// refuses an in-session invocation that names no command.
func Usage() string { return rootUsage }

// Run executes one CLI command and returns its process exit code. Environment
// lookup is injected so the execution commands are testable without mutating
// process state; os.LookupEnv is the production reader.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithEnv(args, os.LookupEnv, stdout, stderr)
}

// RunWithEnv is Run against a supplied environment.
func RunWithEnv(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	root := flag.NewFlagSet("agent-overflow", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	configRoot := root.String("config-root", "", "override the Agent Overflow config root")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, rootUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "agent-overflow: %v\n", err)
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}

	rest := root.Args()
	if len(rest) == 0 {
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}
	for _, command := range topLevelCommands {
		if command.name != rest[0] {
			continue
		}
		return command.run(rest[1:], *configRoot, lookupEnv, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agent-overflow: unknown command %q\n", rest[0])
	_ = writeOutput(stderr, rootUsage)
	return exitError
}

func runWorkflow(args []string, configRoot string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeOutput(stderr, workflowUsage)
		return exitError
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if err := writeOutput(stdout, workflowUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}
	switch args[0] {
	case "new":
		return runNew(args[1:], configRoot, stdout, stderr)
	case "validate":
		return runValidate(args[1:], configRoot, stdout, stderr)
	case "list":
		return runList(args[1:], configRoot, stdout, stderr)
	case "schema":
		return runSchema(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-overflow workflow: unknown command %q\n", args[0])
		_ = writeOutput(stderr, workflowUsage)
		return exitError
	}
}

// runSchema prints the embedded authoring schema. It takes no flags and no
// config root: the schema is a property of the binary, not of an installation.
func runSchema(args []string, stdout, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "help" || arg == "--help" || arg == "-h" {
			if err := writeOutput(stdout, schemaUsage); err != nil {
				return operationalError(stderr, err)
			}
			return exitOK
		}
	}
	if len(args) != 0 {
		fmt.Fprintln(stderr, "agent-overflow workflow schema: unexpected arguments")
		_ = writeOutput(stderr, schemaUsage)
		return exitError
	}
	schema := def.AuthoringSchema()
	if len(schema) == 0 || schema[len(schema)-1] != '\n' {
		schema = append(schema, '\n')
	}
	if err := writeOutput(stdout, string(schema)); err != nil {
		return operationalError(stderr, err)
	}
	return exitOK
}

func runValidate(args []string, inheritedConfigRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow workflow validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configRoot := flags.String("config-root", inheritedConfigRoot, "override the Agent Overflow config root")
	id := flags.String("id", "", "resolve and validate a workflow by id")
	projectSlug := flags.String("project", "", "include workflows for the project slug")
	jsonOutput := flags.Bool("json", false, "write the typed validation result as JSON")
	paths, err := parsePermuted(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, validateUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "agent-overflow workflow validate: %v\n", err)
		_ = writeOutput(stderr, validateUsage)
		return exitError
	}

	if (*id == "" && len(paths) != 1) || (*id != "" && len(paths) != 0) {
		fmt.Fprintln(stderr, "agent-overflow workflow validate: provide exactly one path or --id <id>")
		return exitError
	}
	if *id == "" && *projectSlug != "" {
		fmt.Fprintln(stderr, "agent-overflow workflow validate: --project requires --id")
		return exitError
	}
	if err := validateProjectSlug(*projectSlug); err != nil {
		return operationalError(stderr, err)
	}

	root, err := resolveConfigRoot(*configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}
	var resolved def.ResolvedWorkflow
	if *id == "" {
		workflow, err := def.ParseFile(paths[0])
		if err != nil {
			return operationalError(stderr, err)
		}
		resolved = def.ResolvedWorkflow{Workflow: workflow, Path: paths[0], Scope: def.ScopeShared}
	} else {
		workflows, err := ResolveConfigured(root, *projectSlug)
		if err != nil {
			return operationalError(stderr, err)
		}
		var found bool
		for _, workflow := range workflows {
			if workflow.Workflow.ID == *id {
				resolved = workflow
				found = true
				break
			}
		}
		if !found {
			return operationalError(stderr, fmt.Errorf("workflow id %q was not found", *id))
		}
	}

	bindings, err := loadProjectBindings(root, *projectSlug)
	if err != nil {
		return operationalError(stderr, err)
	}
	// A call edge is resolved against the same configured scopes a run start
	// uses, so `agent-overflow workflow validate` sees the call graph the engine
	// will.
	calls, err := NewCallResolver(root, *projectSlug)
	if err != nil {
		return operationalError(stderr, err)
	}
	result := def.Validate(resolved, bindings, calls)
	result.Findings = slicesx.OrEmpty(result.Findings)
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			return operationalError(stderr, err)
		}
	} else {
		var output strings.Builder
		fmt.Fprintf(&output, "bindings: %s\n", result.BindingStatus)
		for _, finding := range result.Findings {
			fmt.Fprintf(&output, "%s: %s: %s\n", finding.Element, finding.Code, finding.Message)
		}
		// Reports never fail a validation — they describe what the run will do,
		// not what is wrong with it — so they print under their own marker and
		// leave the exit code alone.
		for _, report := range result.Reports {
			fmt.Fprintf(&output, "note: %s: %s: %s\n", report.Element, report.Code, report.Message)
		}
		if err := writeOutput(stdout, output.String()); err != nil {
			return operationalError(stderr, err)
		}
	}
	if result.Valid() {
		return exitOK
	}
	return exitFindings
}

type listEntry struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Scope         def.Scope         `json:"scope"`
	Path          string            `json:"path"`
	ShadowsShared bool              `json:"shadowsShared"`
	BindingStatus def.BindingStatus `json:"bindingStatus"`
	Findings      []def.Finding     `json:"findings"`
}

func runList(args []string, inheritedConfigRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow workflow list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configRoot := flags.String("config-root", inheritedConfigRoot, "override the Agent Overflow config root")
	projectSlug := flags.String("project", "", "include workflows for the project slug")
	jsonOutput := flags.Bool("json", false, "write the resolved workflow list as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, listUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "agent-overflow workflow list: %v\n", err)
		_ = writeOutput(stderr, listUsage)
		return exitError
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "agent-overflow workflow list: unexpected positional arguments")
		return exitError
	}
	if err := validateProjectSlug(*projectSlug); err != nil {
		return operationalError(stderr, err)
	}

	root, err := resolveConfigRoot(*configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}
	bindings, err := loadProjectBindings(root, *projectSlug)
	if err != nil {
		return operationalError(stderr, err)
	}
	entries, err := listConfigured(root, *projectSlug, bindings)
	if err != nil {
		return operationalError(stderr, err)
	}
	if *jsonOutput {
		if err := writeJSON(stdout, entries); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}
	var output strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&output, "id=%s\tname=%q\tscope=%s\tpath=%q\tshadows-shared=%t\tbindings=%s\tfindings=%d\n", entry.ID, entry.Name, entry.Scope, entry.Path, entry.ShadowsShared, entry.BindingStatus, len(entry.Findings))
	}
	if err := writeOutput(stdout, output.String()); err != nil {
		return operationalError(stderr, err)
	}
	return exitOK
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

func writeOutput(output io.Writer, value string) error {
	if _, err := io.WriteString(output, value); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}

func operationalError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "agent-overflow: %v\n", err)
	return exitError
}
