package aocli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/workflow/def"
)

const (
	exitOK       = 0
	exitFindings = 1
	exitError    = 2
)

// Run executes one ao command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	root := flag.NewFlagSet("ao", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	configRoot := root.String("config-root", "", "override the Agent Overflow config root")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeRootUsage(stdout); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "ao: %v\n", err)
		_ = writeRootUsage(stderr)
		return exitError
	}

	rest := root.Args()
	if len(rest) == 0 {
		_ = writeRootUsage(stderr)
		return exitError
	}
	if rest[0] != "workflow" {
		fmt.Fprintf(stderr, "ao: unknown command %q\n", rest[0])
		_ = writeRootUsage(stderr)
		return exitError
	}
	return runWorkflow(rest[1:], *configRoot, stdout, stderr)
}

func runWorkflow(args []string, configRoot string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeWorkflowUsage(stderr)
		return exitError
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if err := writeWorkflowUsage(stdout); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], configRoot, stdout, stderr)
	case "list":
		return runList(args[1:], configRoot, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ao workflow: unknown command %q\n", args[0])
		_ = writeWorkflowUsage(stderr)
		return exitError
	}
}

func runValidate(args []string, inheritedConfigRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ao workflow validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configRoot := flags.String("config-root", inheritedConfigRoot, "override the Agent Overflow config root")
	id := flags.String("id", "", "resolve and validate a workflow by id")
	projectSlug := flags.String("project", "", "include workflows for the project slug")
	jsonOutput := flags.Bool("json", false, "write the typed validation result as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeValidateUsage(stdout); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "ao workflow validate: %v\n", err)
		_ = writeValidateUsage(stderr)
		return exitError
	}

	paths := flags.Args()
	if (*id == "" && len(paths) != 1) || (*id != "" && len(paths) != 0) {
		fmt.Fprintln(stderr, "ao workflow validate: provide exactly one path or --id <id>")
		return exitError
	}
	if *id == "" && *projectSlug != "" {
		fmt.Fprintln(stderr, "ao workflow validate: --project requires --id")
		return exitError
	}
	if err := validateProjectSlug(*projectSlug); err != nil {
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
		root, err := resolveConfigRoot(*configRoot)
		if err != nil {
			return operationalError(stderr, err)
		}
		workflows, err := resolveConfigured(root, *projectSlug)
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

	result := def.Validate(resolved, nil)
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
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Scope         def.Scope `json:"scope"`
	Path          string    `json:"path"`
	ShadowsShared bool      `json:"shadowsShared"`
}

func runList(args []string, inheritedConfigRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ao workflow list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configRoot := flags.String("config-root", inheritedConfigRoot, "override the Agent Overflow config root")
	projectSlug := flags.String("project", "", "include workflows for the project slug")
	jsonOutput := flags.Bool("json", false, "write the resolved workflow list as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeListUsage(stdout); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "ao workflow list: %v\n", err)
		_ = writeListUsage(stderr)
		return exitError
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ao workflow list: unexpected positional arguments")
		return exitError
	}
	if err := validateProjectSlug(*projectSlug); err != nil {
		return operationalError(stderr, err)
	}

	root, err := resolveConfigRoot(*configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}
	entries, err := listConfigured(root, *projectSlug)
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
		fmt.Fprintf(&output, "id=%s\tname=%q\tscope=%s\tpath=%q\tshadows-shared=%t\n", entry.ID, entry.Name, entry.Scope, entry.Path, entry.ShadowsShared)
	}
	if err := writeOutput(stdout, output.String()); err != nil {
		return operationalError(stderr, err)
	}
	return exitOK
}

func resolveConfigRoot(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	return appdirs.Root()
}

func validateProjectSlug(slug string) error {
	if slug == "" {
		return nil
	}
	if len(slug) > 64 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return fmt.Errorf("invalid project slug %q (want 1-64 lowercase letters, digits, or single hyphens)", slug)
	}
	lastWasHyphen := false
	for _, character := range slug {
		isHyphen := character == '-'
		if !isHyphen && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return fmt.Errorf("invalid project slug %q (want 1-64 lowercase letters, digits, or single hyphens)", slug)
		}
		if isHyphen && lastWasHyphen {
			return fmt.Errorf("invalid project slug %q (want 1-64 lowercase letters, digits, or single hyphens)", slug)
		}
		lastWasHyphen = isHyphen
	}
	return nil
}

func configuredSources(configRoot, projectSlug string) ([]def.Source, error) {
	sources := make([]def.Source, 0, 2)
	candidates := []def.Source{{Dir: filepath.Join(configRoot, "workflows"), Scope: def.ScopeShared}}
	if projectSlug != "" {
		candidates = append(candidates, def.Source{Dir: filepath.Join(project.ConfigDir(configRoot, projectSlug), "workflows"), Scope: def.ScopeProject})
	}
	for _, source := range candidates {
		info, err := os.Stat(source.Dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect workflow source %q: %w", source.Dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workflow source %q is not a directory", source.Dir)
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func resolveConfigured(configRoot, projectSlug string) ([]def.ResolvedWorkflow, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	return def.Resolve(sources)
}

func listConfigured(configRoot, projectSlug string) ([]listEntry, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	resolved, err := def.Resolve(sources)
	if err != nil {
		return nil, err
	}
	shared, err := resolveScope(sources, def.ScopeShared)
	if err != nil {
		return nil, err
	}
	sharedIDs := make(map[string]struct{}, len(shared))
	for _, workflow := range shared {
		sharedIDs[workflow.Workflow.ID] = struct{}{}
	}
	entries := make([]listEntry, 0, len(resolved))
	for _, workflow := range resolved {
		_, shadows := sharedIDs[workflow.Workflow.ID]
		entries = append(entries, listEntry{
			ID:            workflow.Workflow.ID,
			Name:          workflow.Workflow.Name,
			Scope:         workflow.Scope,
			Path:          workflow.Path,
			ShadowsShared: shadows && workflow.Scope == def.ScopeProject,
		})
	}
	if entries == nil {
		entries = []listEntry{}
	}
	return entries, nil
}

func resolveScope(sources []def.Source, scope def.Scope) ([]def.ResolvedWorkflow, error) {
	filtered := make([]def.Source, 0, 1)
	for _, source := range sources {
		if source.Scope == scope {
			filtered = append(filtered, source)
		}
	}
	return def.Resolve(filtered)
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
	fmt.Fprintf(stderr, "ao: %v\n", err)
	return exitError
}

func writeRootUsage(output io.Writer) error {
	return writeOutput(output, "Usage: ao [--config-root <path>] workflow <command> [options]\n\nCommands:\n  workflow validate  Validate a workflow definition\n  workflow list      List resolved workflow definitions\n")
}

func writeWorkflowUsage(output io.Writer) error {
	return writeOutput(output, "Usage: ao workflow <command> [options]\n\nCommands:\n  validate  Validate a workflow definition\n  list      List resolved workflow definitions\n")
}

func writeValidateUsage(output io.Writer) error {
	return writeOutput(output, "Usage: ao workflow validate [options] <path>\n       ao workflow validate [options] --id <id>\n\nOptions:\n  --config-root <path>  override the Agent Overflow config root\n  --id <id>             resolve and validate a workflow by id\n  --json                write the typed validation result as JSON\n  --project <slug>      include workflows for the project slug\n")
}

func writeListUsage(output io.Writer) error {
	return writeOutput(output, "Usage: ao workflow list [options]\n\nOptions:\n  --config-root <path>  override the Agent Overflow config root\n  --json                write the resolved workflow list as JSON\n  --project <slug>      include workflows for the project slug\n")
}
