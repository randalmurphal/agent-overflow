package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/threadtitleapp"
	"agent-overflow/internal/triage"
)

// threadTitleApplication composes title policy with provider generation ports.
func (a *App) threadTitleApplication() *threadtitleapp.Service {
	a.threadTitleAppOnce.Do(func() {
		generator := a.threadTitleGenerator
		if generator == nil && (a.generateThreadTitleFn != nil || a.regenerateThreadTitleFn != nil) {
			generator = a.legacyThreadTitleTestGenerator
		}
		if generator == nil {
			generator = a.generateThreadTitlePrompt
		}
		a.threadTitleApp = threadtitleapp.New(threadtitleapp.Config{
			Store:       a.store,
			Attachments: a.attachments,
			Generate:    generator,
			Applied: func(applied threadtitleapp.Applied) {
				a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
					Action: triage.ThreadActionPatch,
					ID:     applied.ThreadID,
					Title:  &applied.Title,
				})
				// A live Claude session with the peer inbox open advertises
				// itself under the thread's title. No-op for other threads.
				a.syncPeerSessionNameAsync(applied.ThreadID)
			},
			Completed: func(completion threadtitleapp.Completion) {
				a.emitEvent(eventchan.ThreadTitleGeneration, ThreadTitleGenerationEvent{
					ThreadID: completion.ThreadID,
					Error:    completion.Error,
				})
			},
		})
	})
	return a.threadTitleApp
}

// maybeGenerateThreadTitleWithAttachments forwards the send-path trigger to
// the application service. It stays at root because app_send.go is the caller
// that knows whether conversation rows preceded this send.
func (a *App) maybeGenerateThreadTitleWithAttachments(
	thread store.Thread,
	content string,
	attachments []store.Attachment,
	hasPriorItems bool,
) {
	a.threadTitleApplication().Auto(thread, content, attachments, hasPriorItems)
}

func (a *App) legacyThreadTitleTestGenerator(thread store.Thread, prompt string, _ []string) (string, error) {
	const contextMarker = "\n\nThread contents:\n"
	if strings.HasPrefix(prompt, "Regenerate the title for an existing thread") {
		if a.regenerateThreadTitleFn == nil {
			return "", fmt.Errorf("regenerate thread title: test generator unavailable")
		}
		_, threadContext, ok := strings.Cut(prompt, contextMarker)
		if !ok {
			return "", fmt.Errorf("regenerate thread title: malformed test prompt")
		}
		return a.regenerateThreadTitleFn(thread, threadContext)
	}
	if a.generateThreadTitleFn == nil {
		return "", fmt.Errorf("generate thread title: test generator unavailable")
	}
	_, message, ok := strings.Cut(prompt, "\nUser message:\n")
	if !ok {
		return "", fmt.Errorf("generate thread title: malformed test prompt")
	}
	message, _, _ = strings.Cut(message, "\n\nAttachment metadata:\n")
	return a.generateThreadTitleFn(thread, message, nil)
}

// generateThreadTitlePrompt drives the configured text-generation CLI for one
// application-service-built prompt. Layer 2 retries once with the alternate
// provider for non-cancellation failures when its binary resolves. Each attempt
// receives its own threadtitle.Timeout budget.
func (a *App) generateThreadTitlePrompt(thread store.Thread, prompt string, imagePaths []string) (string, error) {
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, threadtitle.Timeout, func(cfg textgen.Config, deadline time.Time) (string, error) {
		return a.runThreadTitleOnce(cfg, thread, prompt, imagePaths, deadline)
	})
}

// runThreadTitleOnce dispatches a single thread-title attempt to the provider
// named in cfg. The application lifetime parents the per-attempt deadline so a
// provider subprocess cannot outlive the app.
func (a *App) runThreadTitleOnce(
	cfg textgen.Config,
	thread store.Thread,
	prompt string,
	imagePaths []string,
	deadline time.Time,
) (string, error) {
	ctx, cancel := context.WithDeadline(a.lifeCtx(), deadline)
	defer cancel()

	switch cfg.Provider {
	case string(provider.Codex):
		return a.generateCodexThreadTitle(ctx, cfg, thread, prompt, imagePaths)
	case string(provider.Claude):
		return a.generateClaudeThreadTitle(ctx, cfg, thread, prompt)
	default:
		return "", fmt.Errorf("generate thread title: unsupported provider %q; expected 'codex' or 'claude'", cfg.Provider)
	}
}

func (a *App) generateCodexThreadTitle(
	ctx context.Context,
	cfg textgen.Config,
	thread store.Thread,
	prompt string,
	imagePaths []string,
) (string, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	var extra []string
	for _, imagePath := range imagePaths {
		extra = append(extra, "--image", imagePath)
	}
	raw, err := textgen.RunCodex(
		ctx, cfg, workspace, threadtitle.CodexSchemaJSON,
		extra, prompt, threadtitle.Timeout,
	)
	if err != nil {
		return "", err
	}

	var parsed struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("codex: decode structured output: %w", err)
	}
	return threadtitle.Sanitize(parsed.Title), nil
}

func (a *App) generateClaudeThreadTitle(
	ctx context.Context,
	cfg textgen.Config,
	thread store.Thread,
	prompt string,
) (string, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	stdout, err := textgen.RunClaude(
		ctx, cfg, workspace, threadtitle.ClaudeSchemaJSON,
		nil, prompt, threadtitle.Timeout,
	)
	if err != nil {
		return "", err
	}
	title, err := threadtitle.DecodeClaude(stdout)
	if err != nil {
		return "", err
	}
	return threadtitle.Sanitize(title), nil
}
