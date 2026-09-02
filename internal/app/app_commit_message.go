package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/commitmsg"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/textgen"
)

// GeneratedCommitMessage is the structured output the frontend fills
// the commit dialog with. Subject is capped at 72 chars (imperative,
// no trailing period); body may be empty or multi-paragraph Markdown.
type GeneratedCommitMessage struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// GenerateCommitMessage inspects the referenced workspace, stages all
// changes, and asks the configured text-generation CLI (Codex by
// default, Claude as an alternative) to draft a structured commit
// subject + body. Routing mirrors t3-code's RoutingTextGeneration.
//
// Layer 2 fallback is handled by runTextGenWithFallback: if the configured
// provider's CLI fails for any reason other than context cancellation,
// the call retries once with the alternate provider provided its binary
// resolves, on its own fresh commitmsg.Timeout budget. The user sees
// Codex's structured `{subject, body}` even if Codex is down — silently —
// as long as Claude is installed.
//
// Errors short-circuit in four shapes the frontend can render cleanly:
//   - unresolvable workspace
//   - empty staged diff ("nothing to commit")
//   - CLI missing on PATH
//   - CLI exited non-zero (stderr bubbled into the error)
//
// The caller is expected to have the commit dialog open — it will
// present the result (or the error) to the user for edit before
// actually committing.
func (a *App) GenerateCommitMessage(ws WorkspaceRef) (GeneratedCommitMessage, error) {
	_, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}

	summary, patch, branch, err := a.gatherStagedDiffForCommit(workspace)
	if err != nil {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return GeneratedCommitMessage{}, fmt.Errorf("generate commit message: no uncommitted changes to describe")
	}

	prompt := commitmsg.BuildPrompt(summary, patch, branch, a.commitMessageStyleGuidance(workspace))
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, commitmsg.Timeout, func(cfg textgen.Config, deadline time.Time) (GeneratedCommitMessage, error) {
		return a.runCommitMessageOnce(cfg, workspace, prompt, deadline)
	})
}

// commitMessageStyleGuidance resolves the user's configured writing
// style into the guidance BuildPrompt embeds. The repo style samples
// recent commit subjects from the workspace best-effort — a fresh repo
// with no history falls back to the conventional guidance inside
// commitmsg.
func (a *App) commitMessageStyleGuidance(workspace string) commitmsg.StyleGuidance {
	s := a.currentSettings()
	guidance := commitmsg.StyleGuidance{
		Kind:   s.CommitMessageStyle,
		Custom: s.CommitMessageStyleCustom,
	}
	if guidance.Kind == commitmsg.StyleRepo {
		guidance.RecentSubjects = a.gitCore().RecentCommitSubjects(workspace, commitmsg.RepoStyleSubjectCount)
	}
	return guidance
}

// runCommitMessageOnce dispatches a single commit-message attempt to the
// provider named in cfg. The deadline is the per-attempt budget supplied by
// runTextGenWithFallback. Parents on a.lifeCtx() so commit-message
// subprocesses cancel on app shutdown instead of orphaning past the binding
// return — matches runThreadTitleOnce.
func (a *App) runCommitMessageOnce(
	cfg textgen.Config,
	workspace string,
	prompt string,
	deadline time.Time,
) (GeneratedCommitMessage, error) {
	ctx, cancel := context.WithDeadline(a.lifeCtx(), deadline)
	defer cancel()

	switch cfg.Provider {
	case string(provider.Codex):
		return a.generateCodexCommitMessage(ctx, cfg, workspace, prompt)
	case string(provider.Claude):
		return a.generateClaudeCommitMessage(ctx, cfg, workspace, prompt)
	default:
		return GeneratedCommitMessage{}, fmt.Errorf(
			"generate commit message: unsupported provider %q; expected 'codex' or 'claude'",
			cfg.Provider,
		)
	}
}

// gatherStagedDiffForCommit stages everything in the workspace and
// returns the resulting summary, patch, and current branch. Matches
// t3-code's GitCore.getCommitContext: stage → read summary → read patch
// → read branch.
//
// NOTE ON STAGE-ALL SAFETY: the commit dialog expects "describe
// everything I'm about to commit". Users who want to pick files commit
// through the CommitStep's per-file diff preview after this returns.
// Running `git add -A` here can pick up untracked files the user didn't
// mean to commit; the dialog shows the staged diff before the commit is
// executed, so the user has the last word.
func (a *App) gatherStagedDiffForCommit(workspace string) (summary, patch, branch string, err error) {
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
	branch = core.CurrentBranch(workspace)
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
	workspace string,
	prompt string,
) (GeneratedCommitMessage, error) {
	// The timeout arg only formats the "timed out after X" message;
	// cancellation rides on ctx. Each attempt owns a full commitmsg.Timeout,
	// so the constant is exactly the budget this attempt got.
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
	workspace string,
	prompt string,
) (GeneratedCommitMessage, error) {
	// Effort is not passed here: RunClaude renders cfg.Effort itself, so
	// thread-title and workflow-digest generation get the same flag.
	extra := []string{"--dangerously-skip-permissions"}

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
