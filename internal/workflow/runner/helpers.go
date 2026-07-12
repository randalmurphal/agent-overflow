package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// NarrativePath returns the absolute system-owned narrative path for one phase
// attempt. The caller owns directory creation.
func NarrativePath(dataRoot, itemID, phaseID string, attempt int) (string, error) {
	if strings.TrimSpace(dataRoot) == "" || itemID == "" || phaseID == "" || attempt < 1 {
		return "", fmt.Errorf("workflow narrative path: data root, item id, phase id, and positive attempt are required")
	}
	path := filepath.Join(dataRoot, "workflow-runs", itemID, fmt.Sprintf("%s.%d", phaseID, attempt), "narrative.md")
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("workflow narrative path: %w", err)
	}
	return absolute, nil
}

// BuildPrompt interpolates an inlined runtime prompt and appends the
// system-owned workflow instructions shared by both providers.
func BuildPrompt(phase def.Phase, vars map[string]any, narrativePath string, feedback *engine.Feedback) (string, error) {
	body, err := def.Interpolate(phase.Prompt, phase.Inputs, vars)
	if err != nil {
		return "", fmt.Errorf("build workflow prompt for phase %q: %w", phase.ID, err)
	}
	suffix, err := PromptSuffix(narrativePath, feedback)
	if err != nil {
		return "", err
	}
	if body == "" {
		return suffix, nil
	}
	return strings.TrimRight(body, "\n") + "\n\n" + suffix, nil
}

// BuildTakeoverFinalizePrompt asks the existing phase session to summarize the
// human-steered result into the normal workflow envelope without replaying the
// phase's original task.
func BuildTakeoverFinalizePrompt(narrativePath string) (string, error) {
	suffix, err := PromptSuffix(narrativePath, nil)
	if err != nil {
		return "", err
	}
	return "Review the work completed during this human takeover. Do not redo the original phase. Validate the current workspace state, update the narrative, and return the phase's final workflow control envelope.\n\n" + suffix, nil
}

// PromptSuffix renders the system-owned instructions appended to every phase
// prompt. Feedback is included only when the request carries it.
func PromptSuffix(narrativePath string, feedback *engine.Feedback) (string, error) {
	if !filepath.IsAbs(narrativePath) {
		return "", fmt.Errorf("workflow prompt suffix: narrative path must be absolute")
	}
	var prompt strings.Builder
	prompt.WriteString("<workflow-system-instructions>\n")
	prompt.WriteString("Write a concise narrative of the work performed, decisions made, and validation results to this file:\n")
	prompt.WriteString(narrativePath)
	prompt.WriteString("\nThe narrative is for human inspection and is not part of the control envelope.\n")
	if feedback != nil {
		values := feedback.Values
		if values == nil {
			values = map[string]any{}
		}
		encoded, err := json.MarshalIndent(values, "", "  ")
		if err != nil {
			return "", fmt.Errorf("workflow prompt suffix: encode feedback values: %w", err)
		}
		note := feedback.Note
		if note == "" {
			note = "(none)"
		}
		prompt.WriteString("<workflow-feedback>\nNote:\n")
		prompt.WriteString(note)
		prompt.WriteString("\nValues:\n```json\n")
		prompt.Write(encoded)
		prompt.WriteString("\n```\n</workflow-feedback>\n")
	}
	prompt.WriteString("Your final message must satisfy the attached schema; status must be done, question, or stuck.\n")
	prompt.WriteString("</workflow-system-instructions>")
	return prompt.String(), nil
}

// OutcomeFromEnvelope maps a previously validated control envelope to its
// engine outcome.
func OutcomeFromEnvelope(payload json.RawMessage) (engine.Outcome, error) {
	var control struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &control); err != nil {
		return engine.Outcome{}, fmt.Errorf("decode validated workflow envelope: %w", err)
	}
	var kind engine.OutcomeKind
	switch control.Status {
	case "done":
		kind = engine.OutcomeDone
	case "question":
		kind = engine.OutcomeQuestion
	case "stuck":
		kind = engine.OutcomeStuck
	default:
		return engine.Outcome{}, fmt.Errorf("validated workflow envelope has unknown status %q", control.Status)
	}
	return engine.Outcome{Kind: kind, Envelope: append(json.RawMessage(nil), payload...)}, nil
}

// RetryMessage renders engine-side envelope validation findings for the one
// feedback retry turn.
func RetryMessage(findings []def.EnvelopeFinding) string {
	var message strings.Builder
	message.WriteString("Your previous final message did not produce a valid workflow control envelope. Correct every finding below, then return a final message satisfying the attached schema:\n")
	if len(findings) == 0 {
		message.WriteString("- $: structured output was absent")
		return message.String()
	}
	for _, finding := range findings {
		message.WriteString("- ")
		message.WriteString(finding.Path)
		message.WriteString(": ")
		message.WriteString(finding.Message)
		message.WriteByte('\n')
	}
	return strings.TrimRight(message.String(), "\n")
}
