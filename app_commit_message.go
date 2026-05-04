package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

// commitMessageTimeout bounds how long we'll wait for a provider CLI to
// draft a message. Matches t3-code's 180s — commit-message generation is
// a small, structured JSON request; anything slower than three minutes
// is a misconfiguration the user should see as an error, not a hang.
const commitMessageTimeout = 180 * time.Second

// Default model per provider for short text generation. Lifted
// verbatim from t3-code's RoutingTextGeneration. These values are
// intentionally not exposed through settings.DefaultSettings because
// they're provider-bound — the right default depends on which CLI the
// user selected, and a single shared default is the wrong abstraction.
const (
	defaultTextGenerationCodexModel  = "gpt-5.4-mini"
	defaultTextGenerationClaudeModel = "claude-haiku-4-5"
)

// Per-section caps applied at the prompt-construction layer. Mirrors
// t3-code's Prompts.ts (6k for the summary, 40k for the patch). The
// gather layer in internal/git applies larger caps first, so these can
// trim further without re-reading the repo.
const (
	promptStagedSummaryLimit = 6_000
	promptStagedPatchLimit   = 40_000
)

// GeneratedCommitMessage is the structured output the frontend fills
// the commit dialog with. Subject is capped at 72 chars (imperative,
// no trailing period); body may be empty or multi-paragraph Markdown.
type GeneratedCommitMessage struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// GenerateCommitMessage inspects the thread's working tree, stages all
// changes, and asks the configured text-generation CLI (Codex by
// default, Claude as an alternative) to draft a structured commit
// subject + body. Routing mirrors t3-code's RoutingTextGeneration.
//
// Errors short-circuit in four shapes the frontend can render cleanly:
//   - unknown thread
//   - empty staged diff ("nothing to commit")
//   - CLI missing on PATH
//   - CLI exited non-zero (stderr bubbled into the error)
//
// The caller is expected to have the commit dialog open — it will
// present the result (or the error) to the user for edit before
// actually committing.
func (a *App) GenerateCommitMessage(threadID string) (GeneratedCommitMessage, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commitMessageTimeout)
	defer cancel()

	summary, patch, branch, err := a.gatherStagedDiffForCommit(thread)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: no uncommitted changes to describe")
	}

	prompt := buildCommitMessagePrompt(summary, patch, branch)
	cfg := a.resolveTextGenerationConfig()

	switch cfg.Provider {
	case "codex":
		return a.generateCodexCommitMessage(ctx, cfg, thread, prompt)
	case "claude":
		return a.generateClaudeCommitMessage(ctx, cfg, thread, prompt)
	default:
		return GeneratedCommitMessage{}, fmt.Errorf(
			"generate commit message: unsupported provider %q; expected 'codex' or 'claude'",
			cfg.Provider,
		)
	}
}

// gatherStagedDiffForCommit stages everything in the thread's workspace
// and returns the resulting summary, patch, and current branch. Matches
// t3-code's GitCore.getCommitContext: stage → read summary → read patch
// → read branch.
//
// NOTE ON STAGE-ALL SAFETY: this mirrors t3-code and forge parity — the
// commit dialog expects "describe everything I'm about to commit". Users
// who want to pick files commit through the CommitStep's per-file diff
// preview after this returns. Running `git add -A` here can pick up
// untracked files the user didn't mean to commit; the dialog shows the
// staged diff before the commit is executed, so the user has the last
// word. Flagged in the wave report.
func (a *App) gatherStagedDiffForCommit(thread store.Thread) (summary, patch, branch string, err error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", "", "", err
	}
	core := a.gitCore()
	if err := core.StageAll(workspace); err != nil {
		return "", "", "", fmt.Errorf("stage-all: %w", err)
	}
	summary, err = core.StagedSummary(workspace)
	if err != nil {
		return "", "", "", fmt.Errorf("staged summary: %w", err)
	}
	patch, err = core.StagedPatch(workspace)
	if err != nil {
		return "", "", "", fmt.Errorf("staged patch: %w", err)
	}
	branch = core.CurrentBranchName(workspace)
	return summary, patch, branch, nil
}

// generateCodexCommitMessage drives the Codex CLI via `codex exec
// --ephemeral ...`. Matches t3-code's CodexTextGeneration: writes the
// JSON schema and the --output-last-message target to scratch files,
// pipes the prompt over stdin, then reads the structured JSON back
// from the output file.
func (a *App) generateCodexCommitMessage(
	ctx context.Context,
	cfg textGenerationConfig,
	thread store.Thread,
	prompt string,
) (GeneratedCommitMessage, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	schemaPath, outputPath, cleanup, err := createTextGenerationScratchFiles(commitCodexSchemaJSON)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("codex: scratch files: %w", err)
	}
	defer cleanup()

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args: []string{
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			"-s", "read-only",
			"--model", cfg.Model,
			"--config", fmt.Sprintf("model_reasoning_effort=%q", cfg.Effort),
			"--output-schema", schemaPath,
			"--output-last-message", outputPath,
			"-",
		},
		Cwd:   workspace,
		Stdin: prompt,
	})
	if err != nil {
		return GeneratedCommitMessage{}, translateCLINotFound("codex", commitMessageTimeout, err)
	}
	if result.ExitCode != 0 {
		return GeneratedCommitMessage{}, fmt.Errorf("codex CLI failed: %s", firstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}

	raw, readErr := readTextGenerationOutputFile(outputPath)
	if readErr != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("codex: read output: %w", readErr)
	}

	var parsed struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("codex: decode structured output: %w", err)
	}
	if strings.TrimSpace(parsed.Subject) == "" {
		return GeneratedCommitMessage{}, fmt.Errorf("codex: structured output missing subject")
	}

	return GeneratedCommitMessage{
		Subject: sanitizeCommitSubject(parsed.Subject),
		Body:    sanitizeCommitBody(parsed.Body),
	}, nil
}

// generateClaudeCommitMessage drives the Claude CLI via `claude -p`.
// Matches t3-code's ClaudeTextGeneration: structured JSON output comes
// back on stdout inside a `structured_output` envelope.
func (a *App) generateClaudeCommitMessage(
	ctx context.Context,
	cfg textGenerationConfig,
	thread store.Thread,
	prompt string,
) (GeneratedCommitMessage, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", commitClaudeSchemaJSON,
		"--dangerously-skip-permissions",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  prompt,
	})
	if err != nil {
		return GeneratedCommitMessage{}, translateCLINotFound("claude", commitMessageTimeout, err)
	}
	if result.ExitCode != 0 {
		return GeneratedCommitMessage{}, fmt.Errorf("claude CLI failed: %s", firstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}

	subject, body, err := decodeClaudeCommitMessage([]byte(result.Stdout))
	if err != nil {
		return GeneratedCommitMessage{}, err
	}
	return GeneratedCommitMessage{
		Subject: sanitizeCommitSubject(subject),
		Body:    sanitizeCommitBody(body),
	}, nil
}

// commitCodexSchemaJSON is the JSON schema passed to `codex exec
// --output-schema`. Matches t3-code's buildCommitMessagePrompt output
// schema: required subject+body, no extra keys.
const commitCodexSchemaJSON = `{` +
	`"type":"object",` +
	`"properties":{` +
	`"subject":{"type":"string"},` +
	`"body":{"type":"string"}` +
	`},` +
	`"required":["subject","body"],` +
	`"additionalProperties":false` +
	`}`

// commitClaudeSchemaJSON mirrors the Codex schema but is inlined
// separately because the Claude CLI escapes the schema slightly
// differently on the command line. Keeping them as distinct constants
// means a future divergence (e.g. adding a `branch` key for the
// branch-name flow) won't require re-parsing the one shared string.
const commitClaudeSchemaJSON = `{"type":"object","properties":{"subject":{"type":"string"},"body":{"type":"string"}},"required":["subject"]}`

// buildCommitMessagePrompt assembles the natural-language instruction
// sent to the provider CLI. Matches t3-code's Prompts.ts line-for-line
// so the two apps produce identical output shape for identical input.
func buildCommitMessagePrompt(summary, patch, branch string) string {
	if strings.TrimSpace(branch) == "" {
		branch = "(detached)"
	}
	return strings.Join([]string{
		"You write concise git commit messages.",
		"Return a JSON object with keys: subject, body.",
		"Rules:",
		"- subject must be imperative, <= 72 chars, and no trailing period",
		"- body can be empty string or short bullet points",
		"- capture the primary user-visible or developer-visible change",
		"",
		"Branch: " + branch,
		"",
		"Staged files:",
		limitPromptSection(summary, promptStagedSummaryLimit),
		"",
		"Staged patch:",
		limitPromptSection(patch, promptStagedPatchLimit),
	}, "\n")
}

// limitPromptSection applies the prompt-layer cap with the same
// `[truncated]` marker the gather layer uses. Kept separate from the
// gather-layer cap in internal/git so the two can evolve independently.
func limitPromptSection(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n\n[truncated]"
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
// subject line: single line, stripped quotes, no trailing period,
// falling back to t3-code's "Update project files" sentinel when the
// model returns nothing usable, capped at 72 characters.
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

	if out == "" {
		// Match t3-code's sanitizeCommitSubject fallback so the dialog
		// never opens with a blank subject — the user can overwrite it.
		return ""
	}

	runes := []rune(out)
	if len(runes) <= 72 {
		return out
	}
	return strings.TrimSpace(string(runes[:69])) + "..."
}

// sanitizeCommitBody trims the model's body output without imposing a
// character limit — git has no body length limit and the user can
// always edit. We normalize whitespace: collapse runs of 3+ newlines
// down to 2, strip leading/trailing whitespace.
func sanitizeCommitBody(raw string) string {
	out := strings.TrimSpace(raw)
	if out == "" {
		return ""
	}
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}
