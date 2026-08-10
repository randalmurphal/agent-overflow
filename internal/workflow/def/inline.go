package def

import (
	"fmt"
	"path/filepath"
)

// InlinePrompts converts an authored workflow (prompt fields are sibling-
// relative file paths) into its runtime form (prompt fields are file bodies).
// It is the single transition between those forms; runtime consumers must only
// receive the returned Workflow and must never interpret prompt fields as paths.
func InlinePrompts(resolved ResolvedWorkflow) (Workflow, error) {
	workflow := resolved.Workflow
	workflow.Phases = append([]Phase(nil), resolved.Workflow.Phases...)
	base := filepath.Dir(resolved.Path)

	for phaseIndex := range workflow.Phases {
		phase := &workflow.Phases[phaseIndex]
		body, err := inlinePrompt(base, phase.Prompt)
		if err != nil {
			return Workflow{}, fmt.Errorf("inline workflow %q phase %q prompt %q: %w", workflow.ID, phase.ID, phase.Prompt, err)
		}
		phase.Prompt = body

		// A loop route's `prompt:` override is inlined and frozen exactly like the
		// phase body it replaces, so the run that re-enters a phase through that
		// route renders the file as it was when the definition was resolved — and
		// `--refresh-def` re-reads it with everything else. The route slice is
		// copied first: the phases were copied shallowly, so writing through it
		// would edit the caller's authored workflow.
		phase.Gate.Routes = append([]Route(nil), phase.Gate.Routes...)
		for routeIndex := range phase.Gate.Routes {
			route := &phase.Gate.Routes[routeIndex]
			body, err := inlinePrompt(base, route.Prompt)
			if err != nil {
				return Workflow{}, fmt.Errorf("inline workflow %q phase %q route %d prompt %q: %w", workflow.ID, phase.ID, routeIndex, route.Prompt, err)
			}
			route.Prompt = body
		}

		phase.FanOut = append([]Unit(nil), phase.FanOut...)
		for unitIndex := range phase.FanOut {
			unit := &phase.FanOut[unitIndex]
			body, err := inlinePrompt(base, unit.Prompt)
			if err != nil {
				return Workflow{}, fmt.Errorf("inline workflow %q phase %q unit %q prompt %q: %w", workflow.ID, phase.ID, unit.ID, unit.Prompt, err)
			}
			unit.Prompt = body
		}

		if phase.Unit != nil {
			template := *phase.Unit
			body, err := inlinePrompt(base, template.Prompt)
			if err != nil {
				return Workflow{}, fmt.Errorf("inline workflow %q phase %q unit template %q prompt %q: %w", workflow.ID, phase.ID, template.ID, template.Prompt, err)
			}
			template.Prompt = body
			phase.Unit = &template
		}

		if phase.Join != nil {
			join := *phase.Join
			body, err := inlinePrompt(base, join.Prompt)
			if err != nil {
				return Workflow{}, fmt.Errorf("inline workflow %q phase %q join %q prompt %q: %w", workflow.ID, phase.ID, join.ID, join.Prompt, err)
			}
			join.Prompt = body
			phase.Join = &join
		}
	}
	return workflow, nil
}

func inlinePrompt(base, relative string) (string, error) {
	if relative == "" {
		return "", nil
	}
	path, err := confinedPath(base, relative)
	if err != nil {
		return "", err
	}
	body, err := readLimitedFile(path, "prompt", MaxPromptBytes)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
