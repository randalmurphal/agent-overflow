package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const (
	commitMessageTimeout = 2 * time.Minute
	// diffContextLimit caps the bytes of diff we hand to the model. Large
	// diffs rarely improve the message quality past this budget and we'd
	// rather land a generated message than a timeout.
	diffContextLimit = 20_000
)

// GeneratedCommitMessage is the structured output the frontend fills
// the commit dialog with. Subject is capped at 72 chars (imperative,
// no trailing period); body is optional Markdown paragraphs.
type GeneratedCommitMessage struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// GenerateCommitMessage inspects the thread's working-tree diff and asks the
// provider CLI to draft a commit subject + body. Short-circuits with an
// error when there's nothing to commit so the UI can show a clean
// "nothing to describe" message instead of a synthesized placeholder.
//
// Only Claude is currently wired — it's the provider with a stable
// --json-schema flag. For Codex / other providers, callers should fall
// back to manual entry (the dialog already supports this).
func (a *App) GenerateCommitMessage(threadID string) (GeneratedCommitMessage, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}
	if thread.Provider != string(provider.Claude) {
		return GeneratedCommitMessage{}, fmt.Errorf(
			"generate commit message: provider %q is not supported for automatic generation; write a message manually",
			thread.Provider,
		)
	}

	diff, err := a.GetWorkingTreeDiff(threadID)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: get diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: no uncommitted changes to describe")
	}

	return a.generateClaudeCommitMessage(thread, truncateDiffForPrompt(diff, diffContextLimit))
}

func (a *App) generateClaudeCommitMessage(thread store.Thread, diff string) (GeneratedCommitMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commitMessageTimeout)
	defer cancel()

	model := firstNonEmpty(thread.Model, a.defaultModelForProvider(thread.Provider))
	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", `{"type":"object","properties":{"subject":{"type":"string"},"body":{"type":"string"}},"required":["subject"]}`,
		"--dangerously-skip-permissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, a.providerBinaryPath(thread.Provider), args...)
	cmd.Dir = thread.WorkspacePath
	cmd.Stdin = strings.NewReader(buildCommitMessagePrompt(diff))

	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return GeneratedCommitMessage{}, fmt.Errorf("claude CLI failed: %s", stderr)
			}
		}
		return GeneratedCommitMessage{}, err
	}

	subject, body, err := decodeClaudeCommitMessage(stdout)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}
	return GeneratedCommitMessage{
		Subject: sanitizeCommitSubject(subject),
		Body:    sanitizeCommitBody(body),
	}, nil
}

// truncateDiffForPrompt cuts a diff to a byte budget, preserving a head
// section and optionally a tail section so the model sees both where the
// changes start and where they end. On small diffs this is a no-op.
func truncateDiffForPrompt(diff string, maxBytes int) string {
	if len(diff) <= maxBytes {
		return diff
	}
	if maxBytes < 200 {
		return diff[:maxBytes]
	}
	// Reserve ~20% of the budget for a tail so the model sees the end of
	// the diff as well as the beginning. Join with an explicit marker so
	// the model knows bytes were dropped.
	headLen := (maxBytes * 4) / 5
	tailLen := maxBytes - headLen - len("\n\n[...diff truncated...]\n\n")
	if tailLen <= 0 {
		return diff[:maxBytes]
	}
	return diff[:headLen] + "\n\n[...diff truncated...]\n\n" + diff[len(diff)-tailLen:]
}

// buildCommitMessagePrompt assembles the natural-language instruction sent
// to Claude. Kept in its own function so the prompt is easy to unit-test
// and update without crawling through generate-message internals.
func buildCommitMessagePrompt(diff string) string {
	return strings.TrimSpace(`You write concise git commit messages for coding changes.
Return a JSON object with keys: subject (required), body (optional).

Rules for subject:
- Imperative mood ("Add X", not "Added X" or "Adds X").
- No prefixes like "feat:" or "fix:" unless the repo clearly uses conventional commits.
- Max 72 characters.
- No trailing period.

Rules for body:
- Omit unless the change needs context the diff doesn't make obvious.
- Explain the WHY, not the WHAT — the diff already shows the what.
- Keep paragraphs short; blank lines separate them.
- Do not include the subject again.

Diff:
`) + "\n" + diff
}

// decodeClaudeCommitMessage pulls the structured output out of a Claude
// JSON response. Mirrors decodeClaudeThreadTitle's tail-line + envelope
// shape — Claude's -p JSON mode emits the structured result as the last
// line with a known envelope key.
func decodeClaudeCommitMessage(stdout []byte) (string, string, error) {
	line := strings.TrimSpace(string(stdout))
	if line == "" {
		return "", "", fmt.Errorf("claude returned empty output")
	}

	lines := strings.Split(line, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if candidate != "" {
			last = candidate
			break
		}
	}
	if last == "" {
		return "", "", fmt.Errorf("claude returned no JSON output")
	}

	var envelope struct {
		StructuredOutput struct {
			Subject string `json:"subject"`
			Body    string `json:"body"`
		} `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(last), &envelope); err != nil {
		return "", "", fmt.Errorf("decode claude structured output: %w", err)
	}
	if strings.TrimSpace(envelope.StructuredOutput.Subject) == "" {
		return "", "", fmt.Errorf("claude returned an empty subject")
	}
	return envelope.StructuredOutput.Subject, envelope.StructuredOutput.Body, nil
}

// sanitizeCommitSubject trims the model's output into a well-formed
// subject line: single line, stripped quotes, no trailing period, capped
// at 72 characters with an ellipsis if truncation kicks in.
func sanitizeCommitSubject(raw string) string {
	out := raw
	if line, _, ok := strings.Cut(out, "\n"); ok {
		out = line
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, `'"`+"`")
	out = strings.TrimSpace(out)
	out = strings.TrimSuffix(out, ".")
	out = strings.Join(strings.Fields(out), " ")

	runes := []rune(out)
	if len(runes) <= 72 {
		return out
	}
	return strings.TrimSpace(string(runes[:69])) + "..."
}

// sanitizeCommitBody trims the model's body output without imposing a
// character limit — git has no body length limit and the user can always
// edit. We just normalize whitespace: collapse runs of blank lines,
// strip leading/trailing whitespace.
func sanitizeCommitBody(raw string) string {
	out := strings.TrimSpace(raw)
	if out == "" {
		return ""
	}
	// Collapse 3+ consecutive newlines down to 2 so paragraph spacing is
	// consistent across different model outputs.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}
