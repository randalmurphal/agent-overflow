package threadapp

import (
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

// TestModelInputsRefuseClaudeAliases pins that every model input the service
// stores refuses a Claude alias instead of guessing what it means, and that
// Codex slugs are left for Codex to judge.
func TestModelInputsRefuseClaudeAliases(t *testing.T) {
	service, database, _ := newServiceFixture(t)

	if _, err := service.Create(CreateOptions{ProjectID: "project", Provider: "claude", Model: "opus"}); !errors.Is(err, provider.ErrModelAlias) {
		t.Fatalf("Create(opus) error = %v, want ErrModelAlias", err)
	}
	if threads, err := database.ListThreads(); err != nil || len(threads) != 0 {
		t.Fatalf("threads after a refused create = %d, %v; want none", len(threads), err)
	}

	thread, err := service.Create(CreateOptions{ProjectID: "project", Provider: "claude", Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("Create(claude-opus-5): %v", err)
	}
	if _, err := service.UpdateModel(thread.ID, "sonnet[1m]"); !errors.Is(err, provider.ErrModelAlias) {
		t.Fatalf("UpdateModel(sonnet[1m]) error = %v, want ErrModelAlias", err)
	}
	if _, err := service.UpdateModelSelection(thread.ID, "claude", "haiku"); !errors.Is(err, provider.ErrModelAlias) {
		t.Fatalf("UpdateModelSelection(claude, haiku) error = %v, want ErrModelAlias", err)
	}
	stored, err := database.GetThread(thread.ID)
	if err != nil || stored.Model != "claude-opus-5" {
		t.Fatalf("stored model = %q, %v; want the refused updates to leave claude-opus-5", stored.Model, err)
	}
	updated, err := service.UpdateModel(thread.ID, "claude-opus-5-5[1m]")
	if err != nil || updated.Thread.Model != "claude-opus-5-5" {
		t.Fatalf("UpdateModel(claude-opus-5-5[1m]) = %q, %v; want claude-opus-5-5", updated.Thread.Model, err)
	}

	service.deps.NewID = func() string { return "codex-thread" }
	codex, err := service.Create(CreateOptions{ProjectID: "project", Provider: "codex", Model: "5.4"})
	if err != nil || codex.Model != "5.4" {
		t.Fatalf("Create(codex, 5.4) = %q, %v; want the slug stored as written", codex.Model, err)
	}
}
