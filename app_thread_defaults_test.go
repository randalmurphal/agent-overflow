package main

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
)

func TestGetThreadDefaultsDoesNotLoadColdCodexCatalog(t *testing.T) {
	app := newTestAppWithStore(t)
	calls := 0
	app.codexModelCatalog = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		calls++
		return provider.ModelsForProvider(string(provider.Codex)), nil
	}, time.Now)

	profile := chatmodel.FallbackProfile(string(provider.Codex), "gpt-5.5")
	profile.UpdatedAt = time.Now().UnixMilli()
	if err := app.store.UpsertChatModelProfile(profile); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}

	defaults, err := app.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID})
	if err != nil {
		t.Fatalf("GetThreadDefaults: %v", err)
	}
	if calls != 0 {
		t.Fatalf("Codex catalog loads = %d, want 0 on the new-thread paint path", calls)
	}
	if defaults.Provider != profile.Provider || defaults.Model != profile.Model {
		t.Fatalf("defaults provider/model = %s/%s, want stored %s/%s", defaults.Provider, defaults.Model, profile.Provider, profile.Model)
	}

	if _, err := app.CreateThread(CreateThreadOptions{
		ProjectID: defaultTestProjectID,
		Provider:  defaults.Provider,
		Model:     defaults.Model,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Codex catalog loads after materialization = %d, want 1 authoritative validation", calls)
	}
}

func TestGetThreadDefaultsUsesWarmCodexCatalogWithoutReloading(t *testing.T) {
	app := newTestAppWithStore(t)
	calls := 0
	app.codexModelCatalog = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{
			Slug:     "gpt-5.5",
			Provider: string(provider.Codex),
			ReasoningEfforts: []provider.ReasoningEffortOption{
				{Slug: string(provider.EffortUltra), Default: true},
			},
		}}, nil
	}, time.Now)

	profile := chatmodel.FallbackProfile(string(provider.Codex), "gpt-5.5")
	profile.ReasoningEffort = string(provider.EffortHigh)
	profile.FastMode = true
	profile.UpdatedAt = time.Now().UnixMilli()
	if err := app.store.UpsertChatModelProfile(profile); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	if _, err := app.GetModelsForProvider(string(provider.Codex)); err != nil {
		t.Fatalf("warm Codex catalog: %v", err)
	}

	defaults, err := app.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID})
	if err != nil {
		t.Fatalf("GetThreadDefaults: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Codex catalog loads = %d, want the one explicit warmup", calls)
	}
	if defaults.ReasoningEffort != string(provider.EffortUltra) {
		t.Fatalf("ReasoningEffort = %q, want warm-catalog default %q", defaults.ReasoningEffort, provider.EffortUltra)
	}
	if defaults.FastMode {
		t.Fatal("FastMode = true, want warm catalog without a fast tier to disable it")
	}
}
