package aocli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/workflow/def"
)

// Workflow scope discovery and resolution: where definitions live, which of
// them a given project sees, and how a call edge resolves. Every caller that
// needs "the workflows this project has" — the offline commands, the app's
// composer context, the engine's call resolver — comes through here, so there
// is one answer to scope precedence (project shadows shared) rather than one
// per entry point.

func resolveConfigRoot(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	return appdirs.Root()
}

// workflowProjectScope resolves which project's workflow scope an offline
// command reads. An explicit --project always wins; otherwise AO_PROJECT — the
// slug of the project the calling session works in — supplies it, because the
// offline half has no app to infer a project from and a cold agent inside a
// session has no other way to learn the slug.
//
// An empty answer therefore means neither input was present, which is what lets
// a command with nothing to show say why. A malformed value is named by where it
// came from: the fix for a bad flag is retyping it, and the fix for a bad
// AO_PROJECT is not something the caller typed at all.
func workflowProjectScope(flagValue string, lookupEnv func(string) (string, bool)) (string, error) {
	if slug := strings.TrimSpace(flagValue); slug != "" {
		if err := validateProjectSlug(slug); err != nil {
			return "", err
		}
		return slug, nil
	}
	if lookupEnv == nil {
		return "", nil
	}
	value, _ := lookupEnv(EnvProject)
	slug := strings.TrimSpace(value)
	if slug == "" {
		return "", nil
	}
	if err := validateProjectSlug(slug); err != nil {
		return "", fmt.Errorf("%s: %w", EnvProject, err)
	}
	return slug, nil
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
// chain used by the CLI.
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

// skippedWorkflowDirNotes renders one note per directory discovery ignored that
// looks like an attempt at a workflow. The wording states the layout rather than
// just the fact, because the only useful thing to say to someone who authored
// `<id>/workflow.yaml` is what the flat form is.
func skippedWorkflowDirNotes(configRoot, projectSlug string) ([]string, error) {
	sources, err := configuredSources(configRoot, projectSlug)
	if err != nil {
		return nil, err
	}
	skipped, err := def.SkippedDirs(sources)
	if err != nil {
		return nil, err
	}
	notes := make([]string, 0, len(skipped))
	for _, directory := range skipped {
		notes = append(notes, fmt.Sprintf(
			"note: %s is a directory and was skipped — a workflow is a flat <id>.yaml beside its <id>-*.md prompts",
			directory.Path,
		))
	}
	return notes, nil
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
