package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// generatedThreadTitle drives the configured text-generation CLI to produce
// a short thread title from one user message. message is passed raw —
// BuildPrompt budgets that section itself. Layer 2 retry is
// handled by runTextGenWithFallback: if the configured provider's CLI fails
// for any reason other than context cancellation, the call retries once with
// the alternate provider provided its binary resolves. Each attempt gets its
// own threadtitle.Timeout budget.
func (a *App) generatedThreadTitle(thread store.Thread, message string, attachments []store.Attachment) (string, error) {
	if a.generateThreadTitleFn != nil {
		raw, err := a.generateThreadTitleFn(thread, message, attachments)
		if err != nil {
			return "", err
		}
		return threadtitle.Sanitize(raw), nil
	}

	// Resolved once for both attempts: the attachment rows don't change
	// between them, and a re-resolve would re-stat every file.
	imagePaths, err := a.threadTitleImagePaths(thread.ID, attachments)
	if err != nil {
		return "", err
	}
	prompt := threadtitle.BuildPrompt(message, attachments)

	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, threadtitle.Timeout, func(cfg textgen.Config, deadline time.Time) (string, error) {
		return a.runThreadTitleOnce(cfg, thread, prompt, imagePaths, deadline)
	})
}

// runThreadTitleOnce dispatches a single thread-title attempt to the
// provider named in cfg. The deadline is the per-attempt budget supplied
// by runTextGenWithFallback; the context parents on a.lifeCtx() so the
// subprocess dies with the app rather than outliving it.
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

	// The CLI helper's timeout arg is only used to format the
	// "timed out after X" error message — actual cancellation rides on
	// ctx. Each attempt owns a full threadtitle.Timeout, so the
	// constant is exactly the budget this attempt got.
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

// applyThreadTitleIfCurrent compare-and-swaps a generated title over the
// caller's expected value and emits the sidebar patch when it lands.
// Reports whether the swap applied: a false is the ordinary outcome of
// losing a race with a user rename or a concurrent generation, never an
// error.
func (a *App) applyThreadTitleIfCurrent(threadID, expected, title string) (bool, error) {
	updated, err := a.store.UpdateTitleIfCurrent(threadID, expected, title)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}

	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
		Action: "patch",
		ID:     threadID,
		Title:  &title,
	})
	// A live Claude session with the peer inbox open advertises itself to
	// other sessions on this machine under the thread's name; the fallback
	// it launched with (`<project>/<short id>`) is exactly what a landed
	// title improves on. No-op for every other thread.
	a.syncPeerSessionNameAsync(threadID)
	return true, nil
}

// threadTitleContextItemLimit bounds the rows the regeneration context
// is built from. FormatThreadContext windows the text itself; this is
// the SQL-side bound so a 40k-item thread doesn't hydrate its whole
// history to render 8k characters. The store reports back whether the
// window excluded anything, which is what marks the formatted context
// truncated even when the rows it kept fit the budget whole.
const threadTitleContextItemLimit = 200

// ThreadTitleGenerationEvent is the completion frame of one title
// generation run (auto, heal, or user-triggered regeneration).
// Error is redacted (textgen.RedactError) and empty on success —
// including the no-op outcomes (nothing to title, model returned
// nothing better, a rename won the CAS).
type ThreadTitleGenerationEvent struct {
	ThreadID string `json:"threadId"`
	Error    string `json:"error"`
}

// RegenerateThreadTitle starts a re-title of an existing thread from its
// conversation so far. It acknowledges as soon as the run is under way;
// the outcome arrives on `thread:title_generation`, and the new title
// itself on `thread:updated` when the swap lands.
//
// Asynchronous on purpose. A generation is up to two provider attempts
// of threadtitle.Timeout each, against a transport client timeout of
// 60s: the synchronous form was guaranteed to be abandoned by the caller
// while the backend kept running, and the abandoned caller re-enabled
// its button and stacked retries on top.
//
// An unknown thread is the only synchronous failure. Everything else —
// an empty thread, a model that produced nothing better, a rename that
// won the compare-and-swap, a provider failure — is reported on the
// completion event. A regeneration asked for while one is already
// running for this thread joins it: the running generation's completion
// event is the answer for both callers.
func (a *App) RegenerateThreadTitle(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("regenerate thread title: %w", err)
	}
	if !a.claimThreadTitleGeneration(threadID) {
		return nil
	}
	go a.runClaimedThreadTitleGeneration(threadID, func() error {
		return a.runThreadTitleRegeneration(thread)
	})
	return nil
}

// regeneratedThreadTitle runs the same fallback-capable CLI path as
// first-turn generation against the regeneration prompt. No images: the
// regeneration context carries attachment NAMES, not bytes.
func (a *App) regeneratedThreadTitle(thread store.Thread, previousTitle, threadContext string) (string, error) {
	if a.regenerateThreadTitleFn != nil {
		raw, err := a.regenerateThreadTitleFn(thread, threadContext)
		if err != nil {
			return "", err
		}
		return threadtitle.Sanitize(raw), nil
	}

	prompt := threadtitle.BuildRegeneratePrompt(previousTitle, threadContext)
	primary := a.resolveTextGenerationConfig()
	return runTextGenWithFallback(a, primary, threadtitle.Timeout, func(cfg textgen.Config, deadline time.Time) (string, error) {
		return a.runThreadTitleOnce(cfg, thread, prompt, nil, deadline)
	})
}

// threadTitleContext renders the thread's top-level user / assistant
// text into the regeneration prompt's thread-contents section.
func (a *App) threadTitleContext(threadID string) (string, error) {
	items, dropped, err := a.store.ThreadTitleContextItems(threadID, threadTitleContextItemLimit)
	if err != nil {
		return "", err
	}
	messages := make([]threadtitle.Message, 0, len(items))
	for _, item := range items {
		message := threadtitle.Message{Role: item.Role, Text: item.Summary}
		if item.Kind == "user_text" {
			// Attachment names are garnish on the context; a row whose
			// meta no longer decodes still contributes its text, so the
			// decode failure is reported and stepped over rather than
			// failing the whole regeneration.
			meta, err := usermessage.FromItem(item)
			if err != nil {
				log.Printf("regenerate thread title: decode user meta for item %s: %v", item.ID, err)
			}
			for _, attachment := range meta.Attachments {
				message.AttachmentNames = append(message.AttachmentNames, attachment.Filename)
			}
		}
		messages = append(messages, message)
	}
	// dropped is the store's report that its row window excluded matching
	// rows; the formatter marks the context truncated on it even when the
	// text it kept fits the character budget whole.
	return threadtitle.FormatThreadContext(messages, dropped), nil
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
