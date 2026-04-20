package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
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
	if hasPriorItems || thread.Provider != string(provider.Claude) {
		return
	}
	if strings.TrimSpace(thread.Title) != defaultGeneratedThreadTitle {
		return
	}
	if strings.TrimSpace(content) == "" {
		return
	}

	go func() {
		title, err := a.generatedThreadTitle(thread, content)
		if err != nil {
			log.Printf("send message: generate thread title: %v", err)
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

func (a *App) generatedThreadTitle(thread store.Thread, message string) (string, error) {
	if a.generateThreadTitleFn != nil {
		raw, err := a.generateThreadTitleFn(thread, message)
		if err != nil {
			return "", err
		}
		return sanitizeGeneratedThreadTitle(raw), nil
	}

	switch thread.Provider {
	case string(provider.Claude):
		return a.generateClaudeThreadTitle(thread, message)
	default:
		return "", nil
	}
}

func (a *App) generateClaudeThreadTitle(thread store.Thread, message string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), threadTitleTimeout)
	defer cancel()

	model := firstNonEmpty(thread.Model, a.defaultModelForProvider(thread.Provider))
	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`,
		"--dangerously-skip-permissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, a.providerBinaryPath(thread.Provider), args...)
	cmd.Dir = thread.WorkspacePath
	cmd.Stdin = strings.NewReader(buildThreadTitlePrompt(message))

	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", fmt.Errorf("claude CLI command failed: %s", stderr)
			}
		}
		return "", err
	}

	title, err := decodeClaudeThreadTitle(stdout)
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

	meta, err := json.Marshal(map[string]string{"newTitle": title})
	if err != nil {
		return fmt.Errorf("marshal thread rename meta: %w", err)
	}

	a.emitProviderEvent(provider.ProviderEvent{
		Kind:      provider.EventThreadRenamed,
		ThreadID:  threadID,
		Content:   title,
		Meta:      meta,
		Timestamp: time.Now(),
	})
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

func buildThreadTitlePrompt(message string) string {
	return strings.TrimSpace(`You write concise thread titles for coding conversations.
Return a JSON object with key: title.
Rules:
- Title should summarize the user's request, not restate it verbatim.
- Keep it short and specific (3-8 words).
- Avoid quotes, filler, prefixes, and trailing punctuation.

User message:
` + message)
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
