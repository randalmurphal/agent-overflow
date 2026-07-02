package main

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestSeedChatModelProfileSkipsHiddenLastUsedModel(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	// Hide the catalog head; the next catalog entry is the expected seed.
	hiddenSlug := provider.ClaudeModels[0].Slug
	firstVisible := provider.ClaudeModels[1].Slug
	if _, err := app.settings.Update(map[string]any{
		"claudeHiddenModels": []any{hiddenSlug},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "claude",
		Model:           hiddenSlug,
		ReasoningEffort: "xhigh",
		ContextWindow:   provider.ClaudeExtendedContextWindow,
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}

	// Last-used model is hidden → seed the provider's first visible
	// catalog model instead.
	seed := app.seedChatModelProfile("", "")
	if seed.Provider != "claude" || seed.Model != firstVisible {
		t.Fatalf("seed = %s/%s, want claude/%s (first visible model)", seed.Provider, seed.Model, firstVisible)
	}

	// The provider-scoped branch applies the same guard.
	seed = app.seedChatModelProfile("claude", "")
	if seed.Model != firstVisible {
		t.Fatalf("provider-scoped seed model = %q, want %q", seed.Model, firstVisible)
	}

	// Explicit model requests bypass the hide-list: hiding is a picker
	// preference, not a hard ban.
	seed = app.seedChatModelProfile("claude", hiddenSlug)
	if seed.Model != hiddenSlug {
		t.Fatalf("explicit seed model = %q, want %q", seed.Model, hiddenSlug)
	}
}

func TestSeedChatModelProfileSkipsHiddenCodexModel(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	hiddenSlug := provider.CodexModels[0].Slug
	firstVisible := provider.CodexModels[1].Slug
	if _, err := app.settings.Update(map[string]any{
		"codexHiddenModels": []any{hiddenSlug},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "codex",
		Model:           hiddenSlug,
		ReasoningEffort: "medium",
		ContextWindow:   provider.CodexStandardContextWindow,
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}

	seed := app.seedChatModelProfile("codex", "")
	if seed.Provider != "codex" || seed.Model != firstVisible {
		t.Fatalf("seed = %s/%s, want codex/%s (first visible model)", seed.Provider, seed.Model, firstVisible)
	}
}

func TestSeedChatModelProfileKeepsVisibleLastUsedModel(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"claudeHiddenModels": []any{"claude-haiku-4-5"},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "claude",
		Model:           "claude-opus-4-5",
		ReasoningEffort: "high",
		ContextWindow:   provider.ClaudeStandardContextWindow,
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}

	seed := app.seedChatModelProfile("", "")
	if seed.Model != "claude-opus-4-5" {
		t.Fatalf("seed model = %q, want claude-opus-4-5 (visible last-used stays)", seed.Model)
	}
}

func TestFirstVisibleModelFallsBackWhenAllHidden(t *testing.T) {
	hidden := make([]string, 0, len(provider.ClaudeModels))
	for _, model := range provider.ClaudeModels {
		hidden = append(hidden, model.Slug)
	}

	// The settings UI prevents hiding everything; a hand-mangled file
	// must still yield a usable model — the catalog head.
	if got := firstVisibleModel("claude", hidden); got != provider.ClaudeModels[0].Slug {
		t.Fatalf("firstVisibleModel(all hidden) = %q, want catalog head %q", got, provider.ClaudeModels[0].Slug)
	}
}
