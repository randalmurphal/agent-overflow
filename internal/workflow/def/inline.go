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

		phase.FanOut = append([]Unit(nil), phase.FanOut...)
		for unitIndex := range phase.FanOut {
			unit := &phase.FanOut[unitIndex]
			body, err := inlinePrompt(base, unit.Prompt)
			if err != nil {
				return Workflow{}, fmt.Errorf("inline workflow %q phase %q unit %q prompt %q: %w", workflow.ID, phase.ID, unit.ID, unit.Prompt, err)
			}
			unit.Prompt = body
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
