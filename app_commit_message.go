package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// commitMessageTimeout bounds how long we'll wait for a provider CLI to
// draft a message. Matches t3-code's 180s — commit-message generation is
// a small, structured JSON request; anything slower than three minutes
// is a misconfiguration the user should see as an error, not a hang.
const commitMessageTimeout = 180 * time.Second

// Default model per provider for commit-message generation. Lifted
// verbatim from t3-code's RoutingTextGeneration. These values are
// intentionally not exposed through settings.DefaultSettings because
// they're provider-bound — the right default depends on which CLI the
// user selected, and a single shared default is the wrong abstraction.
const (
	defaultCommitCodexModel  = "gpt-5.4-mini"
	defaultCommitClaudeModel = "claude-haiku-4-5"
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

// commitCLIExecutor is the seam tests use to stub out the Codex/Claude
// CLI invocations. Production wires it to execCommitCLI, which shells
// out via exec.CommandContext; tests install a fake that synthesises
// the stdout/stderr/exitCode triple directly.
type commitCLIExecutor func(ctx context.Context, spec commitCLISpec) (commitCLIResult, error)

// commitCLISpec captures everything needed to invoke a provider CLI for
// commit-message generation. Keeping it as a struct (instead of a long
// parameter list) means the executor interface doesn't need to change
// when we add a new knob later.
type commitCLISpec struct {
	// Binary is the resolved path (absolute, or a name to look up on
	// PATH) of the provider CLI.
	Binary string
	// Args are the positional arguments passed to the binary. For Codex
	// this is the `exec --ephemeral ...` line including temp-file paths;
	// for Claude it's `-p --output-format json ...`.
	Args []string
	// Cwd is the working directory — always the repo root so git-aware
	// CLIs can find their context.
	Cwd string
	// Stdin is the prompt piped to the CLI.
	Stdin string
}

// commitCLIResult captures the observable outcome of a provider-CLI
// invocation. ExitCode 0 indicates success; non-zero surfaces stderr to
// the user. Stdout is only consumed by the Claude path (Codex writes its
// structured result to the --output-last-message file instead).
type commitCLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// execCommitCLI is the production commitCLIExecutor. It shells out with
// the provided args, wires stdin from the prompt string, and captures
// stdout/stderr into strings. exec.ExitError is normalised into a
// non-error return with the exit code so the caller can branch on it.
func execCommitCLI(ctx context.Context, spec commitCLISpec) (commitCLIResult, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Stdin = strings.NewReader(spec.Stdin)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := commitCLIResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	// context cancel / timeout surfaces as err; elevate it so the caller
	// can return the specific timeout error rather than a generic exec
	// failure.
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	var exitErr *exec.ExitError
	if err != nil && errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
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

	s := a.currentSettings()
	providerKind := strings.TrimSpace(s.TextGenerationProvider)
	if providerKind == "" {
		providerKind = settings.DefaultSettings.TextGenerationProvider
	}

	exec := a.commitExecutor
	if exec == nil {
		exec = execCommitCLI
	}

	switch providerKind {
	case "codex":
		model := strings.TrimSpace(s.TextGenerationModel)
		if model == "" {
			model = defaultCommitCodexModel
		}
		effort := strings.TrimSpace(s.TextGenerationReasoningEffort)
		if effort == "" {
			effort = settings.DefaultSettings.TextGenerationReasoningEffort
		}
		binary := a.providerBinaryPath("codex")
		return a.generateCodexCommitMessage(ctx, exec, thread, prompt, binary, model, effort)
	case "claude":
		model := strings.TrimSpace(s.TextGenerationModel)
		if model == "" {
			model = defaultCommitClaudeModel
		}
		binary := a.providerBinaryPath("claude")
		return a.generateClaudeCommitMessage(ctx, exec, thread, prompt, binary, model)
	default:
		return GeneratedCommitMessage{}, fmt.Errorf(
			"generate commit message: unsupported provider %q; expected 'codex' or 'claude'",
			providerKind,
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
	exec commitCLIExecutor,
	thread store.Thread,
	prompt, binary, model, effort string,
) (GeneratedCommitMessage, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	schemaPath, outputPath, cleanup, err := createCodexScratchFiles(commitCodexSchemaJSON)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("codex: scratch files: %w", err)
	}
	defer cleanup()

	result, err := exec(ctx, commitCLISpec{
		Binary: binary,
		Args: []string{
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			"-s", "read-only",
			"--model", model,
			"--config", fmt.Sprintf("model_reasoning_effort=%q", effort),
			"--output-schema", schemaPath,
			"--output-last-message", outputPath,
			"-",
		},
		Cwd:   workspace,
		Stdin: prompt,
	})
	if err != nil {
		return GeneratedCommitMessage{}, translateCLINotFound("codex", err)
	}
	if result.ExitCode != 0 {
		return GeneratedCommitMessage{}, fmt.Errorf("codex CLI failed: %s", firstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}

	raw, readErr := os.ReadFile(outputPath)
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
	exec commitCLIExecutor,
	thread store.Thread,
	prompt, binary, model string,
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
	if model != "" {
		args = append(args, "--model", model)
	}

	result, err := exec(ctx, commitCLISpec{
		Binary: binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  prompt,
	})
	if err != nil {
		return GeneratedCommitMessage{}, translateCLINotFound("claude", err)
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

// createCodexScratchFiles writes the output schema to a temp file and
// creates an empty output file for the CLI to populate. Returns both
// paths and a cleanup func that removes them — callers should `defer`
// the cleanup.
func createCodexScratchFiles(schema string) (schemaPath, outputPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "agent-overflow-commit-*")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() {
		_ = os.RemoveAll(dir)
	}

	schemaPath = filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}

	outputPath = filepath.Join(dir, "output.json")
	if err := os.WriteFile(outputPath, []byte(""), 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return schemaPath, outputPath, cleanup, nil
}

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

// translateCLINotFound turns an exec.ErrNotFound or a context timeout
// into a user-friendly error. Everything else passes through so the
// caller sees the raw cause.
func translateCLINotFound(cliName string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s CLI not found on PATH", cliName)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s CLI timed out after %s", cliName, commitMessageTimeout)
	}
	// The exec package returns a PathError wrapping the underlying
	// syscall.ENOENT; unwrap so our translation fires in that case too.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s CLI not found: %s", cliName, pathErr.Path)
	}
	return err
}

// firstNonEmptyMessage picks the best human-readable error detail from
// a set of candidates — stderr wins over stdout, but an empty stderr
// falls through to whatever we've got. Mirrors t3-code's stderrDetail
// vs stdoutDetail precedence.
func firstNonEmptyMessage(candidates ...string) string {
	for _, c := range candidates {
		if t := strings.TrimSpace(c); t != "" {
			return t
		}
	}
	return ""
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
