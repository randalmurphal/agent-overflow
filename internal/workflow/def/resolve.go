package def

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var idPattern = regexp.MustCompile("^[a-z0-9-]+$")

// Source is one workflow directory and its identity scope.
type Source struct {
	Dir   string
	Scope Scope
}

// SkippedDir is one directory inside a workflow source that discovery ignored
// and that looks like an attempt at authoring a workflow: it holds at least one
// YAML file. Discovery is flat by design — a workflow is `<id>.yaml` beside its
// `<id>-*.md` prompts — so a hand-authored `<id>/workflow.yaml` resolves to
// nothing at all, with no error and no row. Reporting the directory is what keeps
// that silence from being invisible.
type SkippedDir struct {
	Path  string
	Scope Scope
}

// SkippedDirs reports those directories, in source order and sorted by name
// within each source. It is a separate read rather than a second return value of
// Resolve because the engine resolves definitions on every run start and has
// nothing to do with the answer, while the two CLI surfaces that render it pay
// one extra directory listing only when a human is asking.
//
// Pure: it returns what it found and never writes to a log or a stream, so the
// caller decides how loud a skipped directory is.
func SkippedDirs(sources []Source) ([]SkippedDir, error) {
	var skipped []SkippedDir
	for _, source := range sources {
		if source.Scope != ScopeProject && source.Scope != ScopeShared {
			return nil, fmt.Errorf("resolve source %q: invalid scope %q", source.Dir, source.Scope)
		}
		entries, err := os.ReadDir(source.Dir)
		if err != nil {
			return nil, fmt.Errorf("read workflow source %q: %w", source.Dir, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path, err := confinedPath(source.Dir, entry.Name())
			if err != nil {
				return nil, fmt.Errorf("resolve workflow directory %q: %w", filepath.Join(source.Dir, entry.Name()), err)
			}
			holdsYAML, err := containsYAML(path)
			if err != nil {
				return nil, err
			}
			if holdsYAML {
				skipped = append(skipped, SkippedDir{Path: path, Scope: source.Scope})
			}
		}
	}
	return skipped, nil
}

// containsYAML reports whether a directory holds a YAML file directly — the one
// signal that separates "someone tried to author a workflow in here" from an
// unrelated directory that happens to sit beside the definitions.
func containsYAML(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read workflow directory %q: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if extension := filepath.Ext(entry.Name()); extension == ".yaml" || extension == ".yml" {
			return true, nil
		}
	}
	return false, nil
}

// Resolve loads sources, rejects duplicate IDs within a scope, and applies
// project-over-shared precedence regardless of filename.
func Resolve(sources []Source) ([]ResolvedWorkflow, error) {
	byScope := map[Scope]map[string]ResolvedWorkflow{
		ScopeProject: {},
		ScopeShared:  {},
	}
	for _, source := range sources {
		if source.Scope != ScopeProject && source.Scope != ScopeShared {
			return nil, fmt.Errorf("resolve source %q: invalid scope %q", source.Dir, source.Scope)
		}
		entries, err := os.ReadDir(source.Dir)
		if err != nil {
			return nil, fmt.Errorf("read workflow source %q: %w", source.Dir, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" && filepath.Ext(entry.Name()) != ".yml") {
				continue
			}
			path, err := confinedPath(source.Dir, entry.Name())
			if err != nil {
				return nil, fmt.Errorf("resolve workflow file %q: %w", filepath.Join(source.Dir, entry.Name()), err)
			}
			workflow, err := ParseFile(path)
			if err != nil {
				return nil, err
			}
			if !idPattern.MatchString(workflow.ID) {
				return nil, fmt.Errorf("workflow file %q declares invalid id %q (want [a-z0-9-]+)", path, workflow.ID)
			}
			if prior, exists := byScope[source.Scope][workflow.ID]; exists {
				return nil, fmt.Errorf("workflow id %q is duplicated in %s scope: %q and %q", workflow.ID, source.Scope, prior.Path, path)
			}
			byScope[source.Scope][workflow.ID] = ResolvedWorkflow{
				Workflow: workflow, Scope: source.Scope, Path: path,
				HumanGateCount: CountHumanGates(workflow),
			}
		}
	}
	resolved := make(map[string]ResolvedWorkflow, len(byScope[ScopeProject])+len(byScope[ScopeShared]))
	for id, workflow := range byScope[ScopeShared] {
		resolved[id] = workflow
	}
	for id, workflow := range byScope[ScopeProject] {
		resolved[id] = workflow
	}
	ids := make([]string, 0, len(resolved))
	for id := range resolved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]ResolvedWorkflow, 0, len(ids))
	for _, id := range ids {
		result = append(result, resolved[id])
	}
	return result, nil
}
