package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/textgen"
)

// fakeLookPath returns a function that resolves only the names in `available`.
// Everything else returns exec.ErrNotFound — matching the real exec.LookPath
// signature so the helpers can't tell production from test.
func fakeLookPath(available ...string) func(string) error {
	set := make(map[string]struct{}, len(available))
	for _, name := range available {
		set[name] = struct{}{}
	}
	return func(bin string) error {
		if _, ok := set[bin]; ok {
			return nil
		}
		return exec.ErrNotFound
	}
}

// TestResolveTextGenerationConfig_Layer1FallbackCodexMissing covers the
// canonical bug: default settings prefer Codex, but the user has only
// Claude installed. The resolver must substitute Claude with its own
// default model (claude-haiku-4-5) rather than handing the call site a
// Codex config that will fail on every exec.LookPath retry.
func TestResolveTextGenerationConfig_Layer1FallbackCodexMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude") // Codex not installed.

	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want %q (fallback when codex missing)", cfg.Provider, provider.Claude)
	}
	if cfg.Model != textgen.DefaultClaudeModel {
		t.Fatalf("model = %q, want %q (claude default after substitution)", cfg.Model, textgen.DefaultClaudeModel)
	}
	if cfg.Binary != "claude" {
		t.Fatalf("binary = %q, want claude", cfg.Binary)
	}
}

func TestResolveTextGenerationConfig_Layer1FallbackClaudeMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "claude",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex") // Claude not installed.

	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("provider = %q, want %q (fallback when claude missing)", cfg.Provider, provider.Codex)
	}
	if cfg.Model != textgen.DefaultCodexModel {
		t.Fatalf("model = %q, want %q", cfg.Model, textgen.DefaultCodexModel)
	}
}

func TestResolveTextGenerationConfig_BothAvailablePrefersConfigured(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")

	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("default codex should win when both available: got %q", cfg.Provider)
	}
}

func TestResolveTextGenerationConfig_NeitherAvailableReturnsConfigured(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath() // nothing installed.

	cfg := app.resolveTextGenerationConfig()
	// Caller must surface the "codex CLI not found" error; we don't
	// invent a fake provider just to make the resolve step succeed.
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("neither installed: provider = %q, want %q (preserve user preference)", cfg.Provider, provider.Codex)
	}
}

// TestResolveTextGenerationConfig_DoesNotCarryModelAcrossProviders is the
// critical invariant: a Codex-named model in the user's settings must
// NOT be passed to Claude after Layer 1 substitution — Claude would
// reject the slug. The substituted provider always uses its own default.
func TestResolveTextGenerationConfig_DoesNotCarryModelAcrossProviders(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "codex",
		"textGenerationModel":    "gpt-5.4-pro", // user-customized Codex model
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("claude") // only Claude installed.

	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want claude", cfg.Provider)
	}
	if cfg.Model == "gpt-5.4-pro" {
		t.Fatalf("Codex model %q must NOT carry across to Claude", cfg.Model)
	}
	if cfg.Model != textgen.DefaultClaudeModel {
		t.Fatalf("model = %q, want %q (claude default)", cfg.Model, textgen.DefaultClaudeModel)
	}
}

// TestResolveTextGenerationConfig_KeepsUserModelWhenProviderUnchanged
// is the negative of the above: when the configured provider IS available,
// the user's explicit model setting is honored. Substitution is what
// triggers the reset; happy-path keeps user choice.
func TestResolveTextGenerationConfig_KeepsUserModelWhenProviderUnchanged(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "codex",
		"textGenerationModel":    "gpt-5.4-pro",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex") // configured provider available.

	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("provider = %q, want codex", cfg.Provider)
	}
	if cfg.Model != "gpt-5.4-pro" {
		t.Fatalf("user model dropped: got %q, want gpt-5.4-pro", cfg.Model)
	}
}

func TestResolveTextGenerationConfig_CoercesEffortForKnownCodexModel(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationModel":           "gpt-5.5",
		"textGenerationReasoningEffort": "ultra",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex")

	cfg := app.resolveTextGenerationConfig()
	if cfg.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", cfg.Model)
	}
	if cfg.Effort != string(provider.EffortMedium) {
		t.Fatalf("effort = %q, want medium (gpt-5.5 default)", cfg.Effort)
	}
}

func TestResolveTextGenerationConfig_PreservesUltraForSol(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationModel":           "gpt-5.6-sol",
		"textGenerationReasoningEffort": "ultra",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	app.lookPathFn = fakeLookPath("codex")

	cfg := app.resolveTextGenerationConfig()
	if cfg.Model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", cfg.Model)
	}
	if cfg.Effort != string(provider.EffortUltra) {
		t.Fatalf("effort = %q, want ultra", cfg.Effort)
	}
}

func TestResolveTextGenerationConfigFor_ReturnsFalseWhenBinaryMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude")

	if _, ok := app.resolveTextGenerationConfigFor(string(provider.Codex)); ok {
		t.Fatalf("resolveTextGenerationConfigFor(codex) ok = true, want false (binary missing)")
	}
	cfg, ok := app.resolveTextGenerationConfigFor(string(provider.Claude))
	if !ok {
		t.Fatalf("resolveTextGenerationConfigFor(claude) ok = false; expected true (binary present)")
	}
	if cfg.Model != textgen.DefaultClaudeModel {
		t.Fatalf("model = %q, want %q (always default for alternate)", cfg.Model, textgen.DefaultClaudeModel)
	}
}

// TestResolveTextGenerationConfigFor_SymmetricClaudeMissing mirrors the above
// for the other axis: only Codex installed → Codex resolves, Claude returns
// ok=false. Catches regressions where one branch loses its lookPath check.
func TestResolveTextGenerationConfigFor_SymmetricClaudeMissing(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("codex")

	if _, ok := app.resolveTextGenerationConfigFor(string(provider.Claude)); ok {
		t.Fatalf("resolveTextGenerationConfigFor(claude) ok = true, want false (binary missing)")
	}
	cfg, ok := app.resolveTextGenerationConfigFor(string(provider.Codex))
	if !ok {
		t.Fatalf("resolveTextGenerationConfigFor(codex) ok = false; expected true (binary present)")
	}
	if cfg.Model != textgen.DefaultCodexModel {
		t.Fatalf("model = %q, want %q (always default for alternate)", cfg.Model, textgen.DefaultCodexModel)
	}
}

func TestResolveTextGenerationConfigFor_UnknownProviderReturnsFalse(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")
	if _, ok := app.resolveTextGenerationConfigFor("phantom"); ok {
		t.Fatalf("unknown provider should return ok=false")
	}
}

// TestRunTextGenWithFallback_PerAttemptBudgets is the incident fix
// (2026-08-16): a primary that fails by TIMING OUT must still reach the
// alternate, and the alternate must get its own full budget rather than
// the primary's exhausted remainder.
func TestRunTextGenWithFallback_PerAttemptBudgets(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")

	const budget = 90 * time.Second
	var deadlines []time.Time
	var providers []string

	start := time.Now()
	got, err := runTextGenWithFallback(app, app.resolveTextGenerationConfig(), budget,
		func(cfg textgen.Config, deadline time.Time) (string, error) {
			providers = append(providers, cfg.Provider)
			deadlines = append(deadlines, deadline)
			if len(providers) == 1 {
				return "", fmt.Errorf("primary: %w", context.DeadlineExceeded)
			}
			return "alternate result", nil
		},
	)
	if err != nil {
		t.Fatalf("runTextGenWithFallback() error = %v", err)
	}
	if got != "alternate result" {
		t.Fatalf("result = %q, want the alternate's", got)
	}
	if len(providers) != 2 {
		t.Fatalf("attempts = %v, want primary + alternate (DeadlineExceeded must not short-circuit)", providers)
	}
	if providers[0] == providers[1] {
		t.Fatalf("both attempts used %q", providers[0])
	}
	// Each attempt's deadline is ~now+budget. The second must be a FRESH
	// budget, not the first's remainder — and the tolerance is tight
	// enough to notice a shared one: two in-process attempts take
	// microseconds, so seconds of slack is all the wall clock needs.
	for i, deadline := range deadlines {
		if delta := deadline.Sub(start); delta < budget || delta > budget+5*time.Second {
			t.Fatalf("attempt %d deadline is %s past start, want ~%s", i+1, delta, budget)
		}
	}
	if !deadlines[1].After(deadlines[0]) {
		t.Fatal("alternate deadline is not later than the primary's — the budget was shared, not fresh")
	}
}

// TestRunTextGenWithFallback_AlternateFailureIsLoggedAndPrimarySurfaces
// pins the other half of the incident: the fallback's failure used to be
// discarded silently, leaving one opaque log line about the primary.
func TestRunTextGenWithFallback_AlternateFailureIsLoggedAndPrimarySurfaces(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	attempts := 0
	_, err := runTextGenWithFallback(app, app.resolveTextGenerationConfig(), 30*time.Second,
		func(cfg textgen.Config, _ time.Time) (string, error) {
			attempts++
			if attempts == 1 {
				return "", errors.New("primary exploded")
			}
			// What a real CLI timeout looks like after textgen translates
			// it: the message names the CLI and the budget it had.
			return "", textgen.TranslateCLINotFound(cfg.Provider, 30*time.Second, context.DeadlineExceeded)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "primary exploded") {
		t.Fatalf("err = %v, want the primary's error surfaced", err)
	}
	line := logged.String()
	if !strings.Contains(line, "textgen: fallback") {
		t.Fatalf("fallback failure not logged: %q", line)
	}
	if !strings.Contains(line, "CLI timed out after 30s") {
		t.Fatalf("log line should report the alternate's timeout: %q", line)
	}
}

// TestRunTextGenWithFallback_NoAlternateSpawnDuringShutdown: a CLI
// killed by teardown reports its own failure rather than
// context.Canceled, and starting a second provider subprocess into a
// shutdown is how orphans are made.
func TestRunTextGenWithFallback_NoAlternateSpawnDuringShutdown(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex")

	attempts := 0
	_, err := runTextGenWithFallback(app, app.resolveTextGenerationConfig(), time.Minute,
		func(textgen.Config, time.Time) (string, error) {
			attempts++
			app.appCancel()
			return "", errors.New("codex CLI failed: signal: killed")
		},
	)
	if err == nil {
		t.Fatal("expected the primary error")
	}
	if attempts != 1 {
		t.Fatalf("executor called %d times, want 1 (no fallback spawn during shutdown)", attempts)
	}
}

func TestOtherProvider(t *testing.T) {
	if got := otherProvider(string(provider.Claude)); got != string(provider.Codex) {
		t.Fatalf("otherProvider(claude) = %q, want codex", got)
	}
	if got := otherProvider(string(provider.Codex)); got != string(provider.Claude) {
		t.Fatalf("otherProvider(codex) = %q, want claude", got)
	}
	if got := otherProvider("bogus"); got != "" {
		t.Fatalf("otherProvider(bogus) = %q, want empty", got)
	}
}

func TestAvailableTextGenerationProviders(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)

	// Order matters: chatmodel.FallbackProvider prefers the first known
	// entry it sees, and the caller (seedChatModelProfile) feeds this
	// list straight through. Claude must come before Codex so that the
	// "both installed" case keeps the historical Claude-first default.
	app.lookPathFn = fakeLookPath("claude", "codex")
	got := app.availableTextGenerationProviders()
	if len(got) != 2 || got[0] != string(provider.Claude) || got[1] != string(provider.Codex) {
		t.Fatalf("both installed: got %v, want [claude codex] in order", got)
	}

	app.lookPathFn = fakeLookPath("claude")
	got = app.availableTextGenerationProviders()
	if len(got) != 1 || got[0] != string(provider.Claude) {
		t.Fatalf("only claude installed: got %v, want [claude]", got)
	}

	app.lookPathFn = fakeLookPath("codex")
	got = app.availableTextGenerationProviders()
	if len(got) != 1 || got[0] != string(provider.Codex) {
		t.Fatalf("only codex installed: got %v, want [codex]", got)
	}

	app.lookPathFn = fakeLookPath()
	got = app.availableTextGenerationProviders()
	if len(got) != 0 {
		t.Fatalf("neither installed: got %v, want []", got)
	}
}
