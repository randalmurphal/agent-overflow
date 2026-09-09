package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestModelControlsTrustExpiredCatalogAndRememberedSelection(t *testing.T) {
	app := newTestAppWithStore(t)
	now := time.Now()
	calls := 0
	catalog := provider.ModelsForProvider("codex")
	app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) { calls++; return catalog, nil }, func() time.Time { return now })
	if _, err := app.GetModelsForProvider("codex"); err != nil {
		t.Fatal(err)
	}
	thread := testThread("cached-controls")
	thread.Provider = "codex"
	thread.Model = "gpt-5.4"
	thread.ReasoningEffort = "high"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "low"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateThreadFastMode(thread.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("selection refreshed catalog %d times", calls-1)
	}
	// A later catalog can remove the model without changing persisted choices.
	catalog = nil
	if _, err := app.GetModelsForProvider("codex"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "max", FastMode: true}); err != nil {
		t.Fatal(err)
	}
	updated, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	updated, err = app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ReasoningEffort != "max" || !updated.FastMode {
		t.Fatalf("selection changed: %+v", updated)
	}
	opts, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(updated))
	if err != nil {
		t.Fatal(err)
	}
	if opts.ReasoningEffort != provider.EffortMax || !opts.FastMode {
		t.Fatal("send projection changed selection")
	}
}
