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
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

const (
	defaultGeneratedThreadTitle = "New Thread"
	threadTitleTimeout          = 3 * time.Minute
)

func (a *App) maybeGenerateThreadTitle(thread store.Thread, content string, hasPriorItems bool) {
	a.maybeGenerateThreadTitleWithAttachments(thread, content, hasPriorItems, nil)
}

func (a *App) maybeGenerateThreadTitleWithAttachments(thread store.Thread, content string, hasPriorItems bool, attachments []store.Attachment) {
	if hasPriorItems {
		return
	}
	if strings.TrimSpace(thread.Title) != defaultGeneratedThreadTitle {
		return
	}
	if strings.TrimSpace(content) == "" {
		return
	}
	titleMessage := limitPromptSection(content, 8_000)

	go func() {
		title, err := a.generatedThreadTitle(thread, titleMessage, attachments)
		if err != nil {
			log.Printf("send message: generate thread title: %s", redactTitleGenerationError(err))
			return
		}
		if title == "" || title == defaultGeneratedThreadTitle {
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
		return sanitizeGeneratedThreadTitle(raw), nil
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
	cfg textGenerationConfig,
) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), threadTitleTimeout)
	defer cancel()

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	schemaPath, outputPath, cleanup, err := createTextGenerationScratchFiles(threadTitleCodexSchemaJSON)
	if err != nil {
		return "", fmt.Errorf("codex: scratch files: %w", err)
	}
	defer cleanup()

	imagePaths, err := a.threadTitleImagePaths(thread.ID, attachments)
	if err != nil {
		return "", err
	}

	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"-s", "read-only",
		"--model", cfg.Model,
		"--config", fmt.Sprintf("model_reasoning_effort=%q", cfg.Effort),
		"--output-schema", schemaPath,
		"--output-last-message", outputPath,
	}
	for _, imagePath := range imagePaths {
		args = append(args, "--image", imagePath)
	}
	args = append(args, "-")

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  buildThreadTitlePrompt(message, attachments),
	})
	if err != nil {
		return "", translateCLINotFound("codex", threadTitleTimeout, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("codex CLI failed with exit code %d", result.ExitCode)
	}

	raw, err := readTextGenerationOutputFile(outputPath)
	if err != nil {
		return "", fmt.Errorf("codex: read output: %w", err)
	}

	var parsed struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("codex: decode structured output: %w", err)
	}
	return sanitizeGeneratedThreadTitle(parsed.Title), nil
}

func (a *App) generateClaudeThreadTitle(
	thread store.Thread,
	message string,
	attachments []store.Attachment,
	cfg textGenerationConfig,
) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), threadTitleTimeout)
	defer cancel()

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", threadTitleClaudeSchemaJSON,
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  buildThreadTitlePrompt(message, attachments),
	})
	if err != nil {
		return "", translateCLINotFound("claude", threadTitleTimeout, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("claude CLI failed with exit code %d", result.ExitCode)
	}

	title, err := decodeClaudeThreadTitle([]byte(result.Stdout))
	if err != nil {
		return "", err
	}
	return sanitizeGeneratedThreadTitle(title), nil
}

func (a *App) applyGeneratedThreadTitle(threadID, title string) error {
	updated, err := a.store.UpdateTitleIfCurrent(threadID, defaultGeneratedThreadTitle, title)
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
	cfg := a.currentSettings()
	switch providerName {
	case string(provider.Claude):
		return firstNonEmpty(cfg.DefaultModelClaude, settings.DefaultSettings.DefaultModelClaude)
	case string(provider.Codex):
		return firstNonEmpty(cfg.DefaultModelCodex, settings.DefaultSettings.DefaultModelCodex)
	default:
		return ""
	}
}

const threadTitleCodexSchemaJSON = `{"type":"object","properties":{"title":{"type":"string","maxLength":50}},"required":["title"],"additionalProperties":false}`

const threadTitleClaudeSchemaJSON = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`

func buildThreadTitlePrompt(message string, attachments []store.Attachment) string {
	prompt := []string{
		"You write concise thread titles for coding conversations.",
		"Return a JSON object with key: title.",
		"Rules:",
		"- Title should summarize the user's request, not restate it verbatim.",
		"- Keep it short and specific (3-8 words).",
		"- Avoid quotes, filler, prefixes, and trailing punctuation.",
		"- If images are attached, use them as primary context for visual/UI issues.",
		"",
		"User message:",
		limitPromptSection(message, 8_000),
	}

	if len(attachments) > 0 {
		prompt = append(prompt, "", "Attachment metadata:", limitPromptSection(formatThreadTitleAttachments(attachments), 4_000))
	}

	return strings.Join(prompt, "\n")
}

func formatThreadTitleAttachments(attachments []store.Attachment) string {
	lines := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", attachment.Filename, attachment.MimeType, attachment.Size))
	}
	return strings.Join(lines, "\n")
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

func redactTitleGenerationError(err error) string {
	message := err.Error()
	if strings.Contains(message, "CLI failed") {
		return "provider CLI failed"
	}
	return message
}

func decodeClaudeThreadTitle(stdout []byte) (string, error) {
	line := strings.TrimSpace(string(stdout))
	if line == "" {
		return "", fmt.Errorf("claude returned empty output")
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
		return "", fmt.Errorf("claude returned no JSON output")
	}

	var envelope struct {
		StructuredOutput struct {
			Title string `json:"title"`
		} `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(last), &envelope); err != nil {
		return "", fmt.Errorf("decode claude structured output: %w", err)
	}
	return envelope.StructuredOutput.Title, nil
}

func sanitizeGeneratedThreadTitle(raw string) string {
	normalized := raw
	if line, _, ok := strings.Cut(normalized, "\n"); ok {
		normalized = line
	}
	normalized = strings.TrimSpace(normalized)
	normalized = strings.Trim(normalized, `'"`+"`")
	normalized = strings.TrimSpace(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return defaultGeneratedThreadTitle
	}

	runes := []rune(normalized)
	if len(runes) <= 50 {
		return normalized
	}
	return strings.TrimSpace(string(runes[:47])) + "..."
}
