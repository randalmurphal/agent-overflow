// Package catalog finds, lists, validates and scaffolds the workflow
// definitions under one config root. It owns the on-disk layout (a shared
// `workflows/` directory and one per project) and scope precedence; parsing
// and validation of a single definition belong to internal/workflow/def.
//
// Every function takes the config root and the project slug explicitly. The
// caller decides which project is asking; nothing here reads the environment.
package catalog

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/workflow/def"
)

// Entry is one resolved workflow a project sees, with its validation summary.
type Entry struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Scope         def.Scope         `json:"scope"`
	Path          string            `json:"path"`
	ShadowsShared bool              `json:"shadowsShared"`
	BindingStatus def.BindingStatus `json:"bindingStatus"`
	Findings      []def.Finding     `json:"findings"`
}

// Listing is every workflow a project sees, plus notes about directories
// discovery skipped. The notes describe the state of the config root rather
// than any one workflow, which is why they ride beside the list.
type Listing struct {
	Workflows []Entry  `json:"workflows"`
	Notes     []string `json:"notes,omitempty"`
}

// List resolves the shared and project scopes and validates each definition
// against the project's profile bindings and call graph.
func List(configRoot, projectSlug string) (Listing, error) {
	if err := validateProjectSlug(projectSlug); err != nil {
		return Listing{}, err
	}
	bindings, err := loadProjectBindings(configRoot, projectSlug)
	if err != nil {
		return Listing{}, err
	}
	entries, err := listConfigured(configRoot, projectSlug, bindings)
	if err != nil {
		return Listing{}, err
	}
	notes, err := skippedDirNotes(configRoot, projectSlug)
	if err != nil {
		return Listing{}, err
	}
	return Listing{Workflows: entries, Notes: notes}, nil
}

// ValidateID resolves a workflow by id in the project's scopes and validates
// it.
func ValidateID(configRoot, projectSlug, id string) (def.ValidationResult, error) {
	if err := validateProjectSlug(projectSlug); err != nil {
		return def.ValidationResult{}, err
	}
	workflows, err := ResolveConfigured(configRoot, projectSlug)
	if err != nil {
		return def.ValidationResult{}, err
	}
	for _, workflow := range workflows {
		if workflow.Workflow.ID == id {
			return validate(configRoot, projectSlug, workflow)
		}
	}
	// A skipped directory is the likeliest reason an id a human can see on
	// disk resolves to nothing, so the failure carries the explanation rather
	// than leaving it a directory listing away.
	return def.ValidationResult{}, unresolvedWorkflowError(configRoot, projectSlug, id)
}

// ValidatePath validates one definition file.
//
// The scope is derived from where the file sits (D66): a definition in a
// project's source directory is validated under that project's calls and
// bindings even when projectSlug names another, and a file in the shared
// directory, or outside the config root, is shared-scoped and checked
// against projectSlug, exactly as ValidateID on a shared definition is. That
// is the invariant TestValidateByIDAndByPathAgreeOverEveryStarter pins.
func ValidatePath(configRoot, projectSlug, path string) (def.ValidationResult, error) {
	if err := validateProjectSlug(projectSlug); err != nil {
		return def.ValidationResult{}, err
	}
	workflow, err := def.ParseFile(path)
	if err != nil {
		return def.ValidationResult{}, err
	}
	scope := def.ScopeShared
	if derivedSlug, derivedScope, ok := scopeForPath(configRoot, path); ok {
		scope = derivedScope
		if derivedSlug != "" {
			projectSlug = derivedSlug
		}
	}
	return validate(configRoot, projectSlug, def.ResolvedWorkflow{
		Workflow: workflow, Path: path, Scope: scope,
		HumanGateCount: def.CountHumanGates(workflow),
	})
}

// validate checks one resolved definition against the call graph and the
// bindings a run start in projectSlug would use.
func validate(configRoot, projectSlug string, resolved def.ResolvedWorkflow) (def.ValidationResult, error) {
	bindings, err := loadProjectBindings(configRoot, projectSlug)
	if err != nil {
		return def.ValidationResult{}, err
	}
	calls, err := NewCallResolver(configRoot, projectSlug)
	if err != nil {
		return def.ValidationResult{}, err
	}
	result := def.Validate(resolved, bindings, calls)
	result.Findings = slicesx.OrEmpty(result.Findings)
	return result, nil
}

// unresolvedWorkflowError reports an id that resolved to nothing, carrying the
// skipped-directory notes that explain the most common cause. A note that
// cannot be gathered is appended as itself: the caller's problem is still the
// missing id, and swallowing the second failure would hide a broken config
// root.
func unresolvedWorkflowError(configRoot, projectSlug, id string) error {
	message := fmt.Sprintf("workflow id %q was not found", id)
	notes, err := skippedDirNotes(configRoot, projectSlug)
	if err != nil {
		return errors.Join(errors.New(message), err)
	}
	if len(notes) == 0 {
		return errors.New(message)
	}
	return errors.New(message + "\n" + strings.Join(notes, "\n"))
}
