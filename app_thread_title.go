package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/triage"
)

func (a *App) maybeGenerateThreadTitle(thread store.Thread, content string, hasPriorItems bool) {
	a.maybeGenerateThreadTitleWithAttachments(thread, content, hasPriorItems, nil)
}

func (a *App) maybeGenerateThreadTitleWithAttachments(thread store.Thread, content string, hasPriorItems bool, attachments []store.Attachment) {
	if hasPriorItems {
		return
	}
	if strings.TrimSpace(thread.Title) != threadtitle.Default {
		return
	}
	if strings.TrimSpace(content) == "" {
		return
	}
	titleMessage := textgen.LimitPromptSection(content, 8_000)

	go func() {
		title, err := a.generatedThreadTitle(thread, titleMessage, attachments)
		if err != nil {
			log.Printf("send message: generate thread title: %s", threadtitle.RedactError(err))
			return
		}
		if title == "" || title == threadtitle.Default {
			return
		}
		if err := a.applyGeneratedThreadTitle(thread.ID, title); err != nil {
			log.Printf("send message: apply generated thread title: %v", err)
		}
	}()
}

// generatedThreadTitle drives the configured text-generation CLI to produce
// a short thread title. Layer 2 retry is handled by runTextGenWithFallback:
// if the configured provider's CLI fails for any reason other than context
// cancellation, the call retries once with the alternate provider provided
// its binary resolves and there's time left in the shared deadline.
//
// The deadline is shared across both attempts so total wall-clock budget
// stays at threadtitle.Timeout regardless of how many providers we try.
func (a *App) generatedThreadTitle(thread store.Thread, message string, attachments []store.Attachment) (string, error) {
	if a.generateThreadTitleFn != nil {
		raw, err := a.generateThreadTitleFn(thread, message, attachments)
		if err != nil {
			return "", err
		}
		return threadtitle.Sanitize(raw), nil
	}

	deadline := time.Now().Add(threadtitle.Timeout)
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, deadline, func(cfg textgen.Config) (string, error) {
		return a.runThreadTitleOnce(cfg, thread, message, attachments, deadline)
	})
}

// runThreadTitleOnce dispatches a single thread-title attempt to the
// provider named in cfg, deriving the per-attempt context from the shared
// deadline so two attempts together stay within threadtitle.Timeout.
func (a *App) runThreadTitleOnce(
	cfg textgen.Config,
	thread store.Thread,
	message string,
	attachments []store.Attachment,
	deadline time.Time,
) (string, error) {
	ctx, cancel := context.WithDeadline(a.lifeCtx(), deadline)
	defer cancel()

	switch cfg.Provider {
	case string(provider.Codex):
		return a.generateCodexThreadTitle(ctx, cfg, thread, message, attachments)
	case string(provider.Claude):
		return a.generateClaudeThreadTitle(ctx, cfg, thread, message, attachments)
	default:
		return "", fmt.Errorf("generate thread title: unsupported provider %q; expected 'codex' or 'claude'", cfg.Provider)
	}
}

func (a *App) generateCodexThreadTitle(
	ctx context.Context,
	cfg textgen.Config,
	thread store.Thread,
	message string,
	attachments []store.Attachment,
) (string, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	imagePaths, err := a.threadTitleImagePaths(thread.ID, attachments)
	if err != nil {
		return "", err
	}

	var extra []string
	for _, imagePath := range imagePaths {
		extra = append(extra, "--image", imagePath)
	}

	// The CLI helper's timeout arg is only used to format the
	// "timed out after X" error message — actual cancellation rides
	// on ctx. Use time-until-deadline so a Layer 2 retry's misreport
	// stays honest about the budget the alternate actually got.
	raw, err := textgen.RunCodex(
		ctx, cfg, workspace, threadtitle.CodexSchemaJSON,
		extra, threadtitle.BuildPrompt(message, attachments), remainingBudget(ctx, threadtitle.Timeout),
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
	message string,
	attachments []store.Attachment,
) (string, error) {
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	stdout, err := textgen.RunClaude(
		ctx, cfg, workspace, threadtitle.ClaudeSchemaJSON,
		nil, threadtitle.BuildPrompt(message, attachments), remainingBudget(ctx, threadtitle.Timeout),
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

func (a *App) applyGeneratedThreadTitle(threadID, title string) error {
	updated, err := a.store.UpdateTitleIfCurrent(threadID, threadtitle.Default, title)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}

	a.emitEvent("thread:updated", triage.ThreadUpdateEvent{
		Action: "patch",
		ID:     threadID,
		Title:  &title,
	})
	return nil
}

func (a *App) threadTitleImagePaths(threadID string, attachments []store.Attachment) ([]string, error) {
	if len(attachments) == 0 || a.attachments == nil {
		return nil, nil
	}
	paths := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment.MimeType, "image/") {
			continue
		}
		record, path, ok, err := a.attachments.Get(attachment.ID)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", attachment.ID, err)
		}
		if !ok || record.ThreadID != threadID {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}
