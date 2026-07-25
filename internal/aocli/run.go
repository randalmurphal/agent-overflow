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

// Run executes one ao command and returns its process exit code. Environment
// lookup is injected so the execution commands are testable without mutating
// process state; os.LookupEnv is the production reader.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithEnv(args, os.LookupEnv, stdout, stderr)
}

// RunWithEnv is Run against a supplied environment.
func RunWithEnv(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	root := flag.NewFlagSet("ao", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	configRoot := root.String("config-root", "", "override the Agent Overflow config root")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, rootUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "ao: %v\n", err)
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}

	rest := root.Args()
	if len(rest) == 0 {
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}
	switch rest[0] {
	case "help":
		if err := writeOutput(stdout, rootUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	case "workflow":
		return runWorkflow(rest[1:], *configRoot, stdout, stderr)
	case "run":
		return runCommand(rest[1:], lookupEnv, stdout, stderr)
	case "notes":
		return notesCommand(rest[1:], lookupEnv, stdout, stderr)
	case "schedule":
		return scheduleCommand.run(rest[1:], lookupEnv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ao: unknown command %q\n", rest[0])
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}
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
		fmt.Fprintf(stderr, "ao workflow: unknown command %q\n", args[0])
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
		fmt.Fprintln(stderr, "ao workflow schema: unexpected arguments")
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
	flags := flag.NewFlagSet("ao workflow validate", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "ao workflow validate: %v\n", err)
		_ = writeOutput(stderr, validateUsage)
		return exitError
	}

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
	// uses, so `ao workflow validate` sees the call graph the engine will.
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
	flags := flag.NewFlagSet("ao workflow list", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "ao workflow list: %v\n", err)
		_ = writeOutput(stderr, listUsage)
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

// WorkflowSourceDirs names the two directories workflow definitions are read
// from, whether or not they exist yet. Callers that only want to tell a human
// where definitions go use this; callers that want the definitions themselves
// use ResolveConfigured, which skips absent directories. The project directory
// is empty when there is no project.
func WorkflowSourceDirs(configRoot, projectSlug string) (shared, projectDir string) {
	shared = filepath.Join(configRoot, "workflows")
	if projectSlug != "" {
		projectDir = filepath.Join(project.ConfigDir(configRoot, projectSlug), "workflows")
	}
	return shared, projectDir
}

// ResolveConfigured returns workflows from the same shared/project discovery
// chain used by the ao CLI.
func ResolveConfigured(configRoot, projectSlug string) ([]def.ResolvedWorkflow, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	return def.Resolve(sources)
}

// callResolver answers a call phase's static target from one snapshot of the
// configured directories. Resolution is `def.Resolve`'s, so a call edge lands on
// exactly the definition a run start would pick: project scope wins over shared.
type callResolver struct {
	byID map[string]def.ResolvedWorkflow
}

func (r callResolver) ResolveCall(id string) (def.ResolvedWorkflow, error) {
	workflow, ok := r.byID[id]
	if !ok {
		return def.ResolvedWorkflow{}, fmt.Errorf("workflow id %q was not found in this project's shared or project scope", id)
	}
	return workflow, nil
}

// NewCallResolver builds the dry-run's view of the call graph. The directory is
// read once per resolver, so one validation sees one consistent set of
// definitions no matter how many edges it walks.
func NewCallResolver(configRoot, projectSlug string) (def.CallResolver, error) {
	resolved, err := ResolveConfigured(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	return CallResolverFor(resolved), nil
}

// CallResolverFor builds a call resolver over an already-resolved set. Callers
// that resolved the directory themselves use this so one request sees exactly
// one snapshot of the definitions — validating a listing against a re-read of
// the directory could report call edges that disagree with the rows it renders.
func CallResolverFor(resolved []def.ResolvedWorkflow) def.CallResolver {
	byID := make(map[string]def.ResolvedWorkflow, len(resolved))
	for _, workflow := range resolved {
		byID[workflow.Workflow.ID] = workflow
	}
	return callResolver{byID: byID}
}

// ResolveWorkflow resolves one workflow by its explicit persisted scope.
func ResolveWorkflow(configRoot, projectSlug, workflowID string, scope def.Scope) (def.ResolvedWorkflow, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	if scope != def.ScopeShared && scope != def.ScopeProject {
		return def.ResolvedWorkflow{}, fmt.Errorf("workflow %q has invalid scope %q", workflowID, scope)
	}
	resolved, err := resolveScope(sources, scope)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	for _, workflow := range resolved {
		if workflow.Workflow.ID == workflowID {
			return workflow, nil
		}
	}
	return def.ResolvedWorkflow{}, fmt.Errorf("workflow id %q was not found in %s scope", workflowID, scope)
}

func listConfigured(configRoot, projectSlug string, bindings def.Bindings) ([]listEntry, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	resolved, err := def.Resolve(sources)
	if err != nil {
		return nil, err
	}
	calls := CallResolverFor(resolved)
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
		validation := def.Validate(workflow, bindings, calls)
		entries = append(entries, listEntry{
			ID:            workflow.Workflow.ID,
			Name:          workflow.Workflow.Name,
			Scope:         workflow.Scope,
			Path:          workflow.Path,
			ShadowsShared: shadows && workflow.Scope == def.ScopeProject,
			BindingStatus: validation.BindingStatus,
			Findings:      slicesx.OrEmpty(validation.Findings),
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
