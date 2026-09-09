package codexmodels

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestKnownModelSurvivesExpiryRefreshAndFailure(t *testing.T) {
	now := time.Unix(1000, 0)
	next := []provider.ModelInfo{{Slug: "m", Provider: "codex", Capabilities: []string{provider.ModelCapabilityFastMode}, FastModeTier: &provider.FastModeTier{ID: "turbo"}, ReasoningEfforts: []provider.ReasoningEffortOption{{Slug: "high", Default: true}}}}
	var failure error
	calls := 0
	cache := NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) { calls++; return next, failure }, func() time.Time { return now })
	if _, ok := cache.KnownModel("codex", "m"); ok || calls != 0 {
		t.Fatal("cold evidence started a lookup")
	}
	if _, err := cache.Get(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	assertKnown := func() {
		t.Helper()
		model, ok := cache.KnownModel("codex", "m")
		if !ok || model.FastModeTier == nil || model.FastModeTier.ID != "turbo" || !provider.ModelInfoSupportsReasoningEffort(model, "high") {
			t.Fatalf("lost evidence: %+v", model)
		}
		model.FastModeTier.ID = "mutated"
	}
	now = now.Add(2 * time.Minute)
	assertKnown()
	if calls != 1 {
		t.Fatal("expired evidence started a lookup")
	}
	next = []provider.ModelInfo{{Slug: "m", Provider: "codex", ReasoningEfforts: []provider.ReasoningEffortOption{{Slug: "low", Default: true}}}}
	current, err := cache.Get(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if current[0].FastModeTier != nil || provider.ModelInfoSupportsReasoningEffort(current[0], "high") {
		t.Fatal("historical evidence leaked into current catalog")
	}
	assertKnown()
	now = now.Add(2 * time.Minute)
	next = nil
	failure = errors.New("offline")
	if _, err := cache.Get(context.Background(), "codex"); err == nil {
		t.Fatal("expected refresh error")
	}
	assertKnown()
	if _, ok := cache.KnownModel("other-binary", "m"); ok {
		t.Fatal("evidence crossed binary identity")
	}
	cache.Reset()
	if _, ok := cache.KnownModel("codex", "m"); ok {
		t.Fatal("reset retained prior identity")
	}
}

func TestKnownModelRetentionIsBounded(t *testing.T) {
	previous := make([]provider.ModelInfo, 1000)
	for i := range previous {
		previous[i] = provider.ModelInfo{Slug: fmt.Sprint(i)}
	}
	got := rememberModels(previous, []provider.ModelInfo{{Slug: "new"}})
	if len(got) != 1000 || got[0].Slug != "new" {
		t.Fatalf("retained %d models", len(got))
	}
}
