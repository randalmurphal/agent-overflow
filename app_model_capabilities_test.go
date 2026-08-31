package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
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

// TestFastModeTierIDForModelReadsTheCatalogTier pins the resolver every session
// build asks. Empty is the "no catalog opinion" answer for both an unresolved
// slug and a provider with no tier concept — the Codex translator turns that
// into the legacy `priority` id rather than dropping fast mode.
func TestFastModeTierIDForModelReadsTheCatalogTier(t *testing.T) {
	app := appWithCodexModelCatalog(t, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{
			{
				Slug:         "gpt-fast",
				Provider:     string(provider.Codex),
				Capabilities: []string{provider.ModelCapabilityFastMode},
				FastModeTier: &provider.FastModeTier{ID: "turbo", Name: "Turbo"},
			},
			{Slug: "gpt-plain", Provider: string(provider.Codex)},
		}, nil
	})

	if got := app.fastModeTierIDForModel(string(provider.Codex), "gpt-fast"); got != "turbo" {
		t.Errorf("fastModeTierIDForModel(gpt-fast) = %q, want turbo", got)
	}
	if got := app.fastModeTierIDForModel(string(provider.Codex), "gpt-plain"); got != "" {
		t.Errorf("fastModeTierIDForModel(gpt-plain) = %q, want empty", got)
	}
	if got := app.fastModeTierIDForModel(string(provider.Codex), "gpt-unknown"); got != "" {
		t.Errorf("fastModeTierIDForModel(gpt-unknown) = %q, want empty", got)
	}
	// Claude declares no tiers at all; the capability marker is its whole
	// fast-mode answer and this resolver must stay silent for it.
	if got := app.fastModeTierIDForModel(string(provider.Claude), "claude-opus-5"); got != "" {
		t.Errorf("fastModeTierIDForModel(claude-opus-5) = %q, want empty", got)
	}
}

// TestBuildSessionOptionsStampsTheFastModeTier proves the resolver is actually
// wired into the ONE SessionOptions construction site. Both the spawn path and
// the live-config reconciler go through buildSessionOptions, so if it ever
// stopped stamping the tier, a reconcile would diff a resolved id against an
// unresolved one and flap the session's serviceTier on every pass.
func TestBuildSessionOptionsStampsTheFastModeTier(t *testing.T) {
	app := newTestAppWithStore(t)
	// newTestAppWithStore already consumed codexModelCatalogOnce (it installs a
	// deliberately-failing lister), so the catalog is swapped by assignment
	// rather than through the Once — which would silently no-op here and leave
	// this test asserting against the static fallback.
	app.codexModelCatalog = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{
			Slug:         "gpt-fast",
			Provider:     string(provider.Codex),
			Capabilities: []string{provider.ModelCapabilityFastMode},
			FastModeTier: &provider.FastModeTier{ID: "turbo", Name: "Turbo"},
			ReasoningEfforts: []provider.ReasoningEffortOption{
				{Slug: string(provider.EffortHigh), Default: true},
			},
		}}, nil
	}, time.Now)

	thread := testThread("thread-fast-mode-tier")
	thread.Provider = string(provider.Codex)
	thread.Model = "gpt-fast"
	thread.ReasoningEffort = string(provider.EffortHigh)
	thread.FastMode = true
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}

	opts, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(stored))
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	if !opts.FastMode {
		t.Fatalf("FastMode = false, want the stored flag preserved")
	}
	if opts.FastModeTierID != "turbo" {
		t.Fatalf("FastModeTierID = %q, want turbo", opts.FastModeTierID)
	}
	if got := codex.ConfigFromOptions(opts).ServiceTier; got != "turbo" {
		t.Fatalf("ServiceTier = %q, want turbo on the wire", got)
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
