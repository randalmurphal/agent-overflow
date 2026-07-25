package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// AttemptDir returns the absolute system-owned directory holding one phase
// attempt's app-managed files. The caller owns directory creation.
func AttemptDir(dataRoot, itemID, phaseID string, attempt int) (string, error) {
	if strings.TrimSpace(dataRoot) == "" || attempt < 1 {
		return "", fmt.Errorf("workflow attempt directory: data root and positive attempt are required")
	}
	if err := pathSegment(itemID, "item id"); err != nil {
		return "", fmt.Errorf("workflow attempt directory: %w", err)
	}
	if err := pathSegment(phaseID, "phase id"); err != nil {
		return "", fmt.Errorf("workflow attempt directory: %w", err)
	}
	path := filepath.Join(dataRoot, "workflow-runs", itemID, fmt.Sprintf("%s.%d", phaseID, attempt))
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("workflow attempt directory: %w", err)
	}
	return absolute, nil
}

// UnitAttemptDir returns the absolute system-owned directory holding one
// fan-out unit try's app-managed files, nested under its phase attempt's
// directory so a run's files stay one tree per attempt. `unitAttempt` is in the
// name because a retried unit reuses its row but must not reuse its narrative:
// the previous try's account of what it did is evidence, not scratch space.
func UnitAttemptDir(dataRoot, itemID, phaseID string, attempt int, unitID string, unitAttempt int) (string, error) {
	dir, err := AttemptDir(dataRoot, itemID, phaseID, attempt)
	if err != nil {
		return "", err
	}
	if err := pathSegment(unitID, "unit id"); err != nil {
		return "", fmt.Errorf("workflow unit directory: %w", err)
	}
	if unitAttempt < 1 {
		return "", fmt.Errorf("workflow unit directory: positive unit attempt is required")
	}
	return filepath.Join(dir, "units", fmt.Sprintf("%s.%d", unitID, unitAttempt)), nil
}

// UnitNarrativePath returns the absolute system-owned narrative path for one
// fan-out unit try. The caller owns directory creation.
func UnitNarrativePath(dataRoot, itemID, phaseID string, attempt int, unitID string, unitAttempt int) (string, error) {
	dir, err := UnitAttemptDir(dataRoot, itemID, phaseID, attempt, unitID, unitAttempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "narrative.md"), nil
}

// UnitEnvelopePath returns the absolute path a tool unit's command may write
// its control envelope to. The runner exports it as AO_ENVELOPE.
func UnitEnvelopePath(dataRoot, itemID, phaseID string, attempt int, unitID string, unitAttempt int) (string, error) {
	dir, err := UnitAttemptDir(dataRoot, itemID, phaseID, attempt, unitID, unitAttempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "envelope.json"), nil
}

// pathSegment refuses any identifier that would not stay one directory level
// down. Ids reaching here are pattern-validated definitions or app-minted
// uuids, so this can only ever fire on a bug — which is exactly why it lives in
// the path builder instead of in every caller.
func pathSegment(value, name string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if value != filepath.Base(value) || value == "." || value == ".." || strings.ContainsRune(value, filepath.Separator) {
		return fmt.Errorf("%s %q is not a single path segment", name, value)
	}
	return nil
}

// NarrativePath returns the absolute system-owned narrative path for one phase
// attempt. Agent phases are told to write it; the runner writes it for tool
// phases. The caller owns directory creation.
func NarrativePath(dataRoot, itemID, phaseID string, attempt int) (string, error) {
	dir, err := AttemptDir(dataRoot, itemID, phaseID, attempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "narrative.md"), nil
}

// EnvelopePath returns the absolute system-provided path a tool phase's command
// may write its control envelope to. The runner exports it as AO_ENVELOPE.
func EnvelopePath(dataRoot, itemID, phaseID string, attempt int) (string, error) {
	dir, err := AttemptDir(dataRoot, itemID, phaseID, attempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "envelope.json"), nil
}

// BuildPrompt interpolates an inlined runtime phase prompt and appends the
// system-owned workflow instructions shared by both providers.
func BuildPrompt(phase def.Phase, vars map[string]any, narrativePath string, feedback *engine.Feedback) (string, error) {
	prompt, err := buildPrompt(phase.Prompt, phase.Inputs, vars, narrativePath, feedback)
	if err != nil {
		return "", fmt.Errorf("build workflow prompt for phase %q: %w", phase.ID, err)
	}
	return prompt, nil
}

// BuildUnitPrompt is BuildPrompt for one fan-out unit. Declarations differ per
// role — a work unit reads the phase's inputs plus its element binding, a join
// reads the phase's inputs plus the reserved `units` results — so the caller
// supplies them from def rather than this package re-deriving them.
func BuildUnitPrompt(unit def.Unit, declarations map[string]def.Variable, vars map[string]any, narrativePath string, feedback *engine.Feedback) (string, error) {
	prompt, err := buildPrompt(unit.Prompt, declarations, vars, narrativePath, feedback)
	if err != nil {
		return "", fmt.Errorf("build workflow prompt for unit %q: %w", unit.ID, err)
	}
	return prompt, nil
}

func buildPrompt(body string, declarations map[string]def.Variable, vars map[string]any, narrativePath string, feedback *engine.Feedback) (string, error) {
	interpolated, err := def.Interpolate(body, declarations, vars)
	if err != nil {
		return "", err
	}
	suffix, err := PromptSuffix(narrativePath, feedback)
	if err != nil {
		return "", err
	}
	if interpolated == "" {
		return suffix, nil
	}
	return strings.TrimRight(interpolated, "\n") + "\n\n" + suffix, nil
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
	// The branch rules below are enforced by def.ValidateEnvelope but cannot be
	// expressed in the schema itself (discriminated unions are not portable
	// across providers — D2a), so they have to be stated. Without them both
	// providers routinely attach a courtesy `reason` to a done envelope, which
	// fails post-validation and burns the single envelope retry on a mistake
	// the phase was never warned about.
	prompt.WriteString("Your final message must satisfy the attached schema; status must be done, question, or stuck.\n")
	prompt.WriteString("Exactly one branch may be populated, and the other two fields must be null:\n")
	prompt.WriteString("- status done: outputs must be non-null; question and reason must both be null.\n")
	prompt.WriteString("- status question: question must be a non-empty string; outputs and reason must both be null.\n")
	prompt.WriteString("- status stuck: reason must be a non-empty string; outputs and question must both be null.\n")
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
