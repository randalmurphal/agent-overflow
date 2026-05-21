package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
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

func (a *App) generatedThreadTitle(thread store.Thread, message string, attachments []store.Attachment) (string, error) {
	if a.generateThreadTitleFn != nil {
		raw, err := a.generateThreadTitleFn(thread, message, attachments)
		if err != nil {
			return "", err
		}
		return threadtitle.Sanitize(raw), nil
	}

	cfg := a.resolveTextGenerationConfig()
	switch cfg.Provider {
	case string(provider.Codex):
		return a.generateCodexThreadTitle(thread, message, attachments, cfg)
	case string(provider.Claude):
		return a.generateClaudeThreadTitle(thread, message, attachments, cfg)
	default:
		return "", fmt.Errorf("generate thread title: unsupported provider %q; expected 'codex' or 'claude'", cfg.Provider)
	}
}

func (a *App) generateCodexThreadTitle(
	thread store.Thread,
	message string,
	attachments []store.Attachment,
	cfg textgen.Config,
) (string, error) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadtitle.Timeout)
	defer cancel()

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

	raw, err := textgen.RunCodex(
		ctx, cfg, workspace, threadtitle.CodexSchemaJSON,
		extra, threadtitle.BuildPrompt(message, attachments), threadtitle.Timeout,
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
	thread store.Thread,
	message string,
	attachments []store.Attachment,
	cfg textgen.Config,
) (string, error) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadtitle.Timeout)
	defer cancel()

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	stdout, err := textgen.RunClaude(
		ctx, cfg, workspace, threadtitle.ClaudeSchemaJSON,
		nil, threadtitle.BuildPrompt(message, attachments), threadtitle.Timeout,
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

	// Only thread:updated is wired on the frontend; the historical
	// provider:event fanout here was consumed by the retired test
	// harness but never by the UI.
	if thread, gerr := a.store.GetThread(threadID); gerr == nil {
		a.emitEvent("thread:updated", thread)
	}
	return nil
}

func (a *App) defaultModelForProvider(providerName string) string {
	return chatmodel.FallbackModelForProvider(providerName)
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
