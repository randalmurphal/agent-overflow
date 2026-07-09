package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestModelCapabilitiesUseSuccessfulCodexCatalogAsAuthoritative(t *testing.T) {
	app := appWithCodexModelCatalog(t, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{
			Slug:     "gpt-5.5",
			Provider: string(provider.Codex),
			ReasoningEfforts: []provider.ReasoningEffortOption{
				{Slug: "ultra", Default: true},
				{Slug: "future-effort"},
			},
		}}, nil
	})

	if app.supportsFastModeForModel(string(provider.Codex), "gpt-5.5") {
		t.Fatal("live model without priority service tier should override static fast-mode support")
	}
	if !app.reasoningEffortSupportedForModel(string(provider.Codex), "gpt-5.5", "ultra") {
		t.Fatal("live model should support its advertised reasoning effort")
	}
	if app.reasoningEffortSupportedForModel(string(provider.Codex), "gpt-5.5", "high") {
		t.Fatal("live model should reject a statically supported but unadvertised effort")
	}
	if app.reasoningEffortSupportedForModel(string(provider.Codex), "gpt-5.5", "future-effort") {
		t.Fatal("live model should reject effort slugs the app cannot preserve")
	}
	if app.supportsFastModeForModel(string(provider.Codex), "gpt-5.4") {
		t.Fatal("model omitted by successful live catalog should not use static fast-mode fallback")
	}
	if app.reasoningEffortSupportedForModel(string(provider.Codex), "gpt-5.4", "high") {
		t.Fatal("model omitted by successful live catalog should not use static effort fallback")
	}

	profile := app.sanitizeChatModelProfile(store.ChatModelProfile{
		Provider:        string(provider.Codex),
		Model:           "gpt-5.5",
		ReasoningEffort: "high",
		FastMode:        true,
	})
	if profile.ReasoningEffort != "ultra" || profile.FastMode {
		t.Fatalf("sanitized profile = %+v, want live default ultra with fast mode disabled", profile)
	}
}

func TestModelCapabilitiesFallBackWhenCodexCatalogFails(t *testing.T) {
	app := appWithCodexModelCatalog(t, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		return nil, errors.New("codex unavailable")
	})

	if !app.supportsFastModeForModel(string(provider.Codex), "gpt-5.5") {
		t.Fatal("catalog failure should preserve static fast-mode support")
	}
	if !app.reasoningEffortSupportedForModel(string(provider.Codex), "gpt-5.5", "high") {
		t.Fatal("catalog failure should preserve static reasoning efforts")
	}
}

func appWithCodexModelCatalog(t *testing.T, list codexmodels.Lister) *App {
	t.Helper()
	app := &App{}
	app.codexModelCatalogOnce.Do(func() {
		app.codexModelCatalog = codexmodels.NewWith(time.Minute, list, time.Now)
	})
	return app
}
