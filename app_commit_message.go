package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/commitmsg"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
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

	ctx, cancel := context.WithTimeout(context.Background(), commitmsg.Timeout)
	defer cancel()

	summary, patch, branch, err := a.gatherStagedDiffForCommit(thread)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: no uncommitted changes to describe")
	}

	prompt := commitmsg.BuildPrompt(summary, patch, branch)
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
// NOTE ON STAGE-ALL SAFETY: the commit dialog expects "describe
// everything I'm about to commit". Users who want to pick files commit
// through the CommitStep's per-file diff preview after this returns.
// Running `git add -A` here can pick up untracked files the user didn't
// mean to commit; the dialog shows the staged diff before the commit is
// executed, so the user has the last word.
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
	cfg textgen.Config,
	thread store.Thread,
	prompt string,
) (GeneratedCommitMessage, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	raw, err := textgen.RunCodex(ctx, cfg, workspace, commitmsg.CodexSchemaJSON, nil, prompt, commitmsg.Timeout)
	if err != nil {
		return GeneratedCommitMessage{}, err
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
		Subject: commitmsg.SanitizeSubject(parsed.Subject),
		Body:    commitmsg.SanitizeBody(parsed.Body),
	}, nil
}

// generateClaudeCommitMessage drives the Claude CLI via `claude -p`.
// Matches t3-code's ClaudeTextGeneration: structured JSON output comes
// back on stdout inside a `structured_output` envelope.
func (a *App) generateClaudeCommitMessage(
	ctx context.Context,
	cfg textgen.Config,
	thread store.Thread,
	prompt string,
) (GeneratedCommitMessage, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	extra := []string{"--dangerously-skip-permissions"}
	if cfg.Effort != "" {
		extra = append(extra, "--effort", cfg.Effort)
	}

	stdout, err := textgen.RunClaude(ctx, cfg, workspace, commitmsg.ClaudeSchemaJSON, extra, prompt, commitmsg.Timeout)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}

	subject, body, err := commitmsg.DecodeClaude(stdout)
	if err != nil {
		return GeneratedCommitMessage{}, err
	}
	return GeneratedCommitMessage{
		Subject: commitmsg.SanitizeSubject(subject),
		Body:    commitmsg.SanitizeBody(body),
	}, nil
}
