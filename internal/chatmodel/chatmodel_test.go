package chatmodel

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestFallbackProvider_EmptyProbeDefaultsToClaude(t *testing.T) {
	// No probe data → Claude (canonical default, consistent error surface).
	if got := FallbackProvider(); got != string(provider.Claude) {
		t.Fatalf("FallbackProvider() = %q, want %q", got, provider.Claude)
	}
	if got := FallbackProvider([]string{}...); got != string(provider.Claude) {
		t.Fatalf("FallbackProvider([]...) = %q, want %q", got, provider.Claude)
	}
}

func TestFallbackProvider_BothAvailablePrefersClaude(t *testing.T) {
	got := FallbackProvider(string(provider.Claude), string(provider.Codex))
	if got != string(provider.Claude) {
		t.Fatalf("FallbackProvider(both) = %q, want %q", got, provider.Claude)
	}
}

func TestFallbackProvider_OnlyCodexAvailableReturnsCodex(t *testing.T) {
	got := FallbackProvider(string(provider.Codex))
	if got != string(provider.Codex) {
		t.Fatalf("FallbackProvider(codex) = %q, want %q", got, provider.Codex)
	}
}

func TestFallbackProvider_OnlyClaudeAvailableReturnsClaude(t *testing.T) {
	got := FallbackProvider(string(provider.Claude))
	if got != string(provider.Claude) {
		t.Fatalf("FallbackProvider(claude) = %q, want %q", got, provider.Claude)
	}
}

func TestFallbackProvider_UnknownProvidersIgnored(t *testing.T) {
	// Probe with only unknown names → defaults to Claude.
	got := FallbackProvider("bogus", "phantom")
	if got != string(provider.Claude) {
		t.Fatalf("FallbackProvider(unknown) = %q, want %q", got, provider.Claude)
	}
}

func TestFallbackProvider_MixedKnownAndUnknown(t *testing.T) {
	// Unknown names mixed with a known Codex → still picks Codex (Claude
	// isn't in the list, the unknowns can't substitute).
	if got := FallbackProvider("bogus", string(provider.Codex)); got != string(provider.Codex) {
		t.Fatalf("FallbackProvider(bogus, codex) = %q, want %q", got, provider.Codex)
	}
	// Same with Claude in the list — Claude wins regardless of position.
	if got := FallbackProvider("bogus", string(provider.Claude)); got != string(provider.Claude) {
		t.Fatalf("FallbackProvider(bogus, claude) = %q, want %q", got, provider.Claude)
	}
}

func TestFallbackModelForProvider(t *testing.T) {
	if got := FallbackModelForProvider(string(provider.Claude)); got == "" {
		t.Fatalf("expected non-empty Claude model")
	}
	if got := FallbackModelForProvider(string(provider.Codex)); got == "" {
		t.Fatalf("expected non-empty Codex model")
	}
	if got := FallbackModelForProvider("bogus-provider"); got != "" {
		t.Fatalf("FallbackModelForProvider(bogus) = %q, want empty", got)
	}
}

func TestFallbackProfile_FillsWhenInputsEmpty(t *testing.T) {
	got := FallbackProfile("", "")
	if got.Provider == "" {
		t.Fatalf("Provider not filled: %#v", got)
	}
	if got.Model == "" {
		t.Fatalf("Model not filled: %#v", got)
	}
	if got.RuntimeMode != string(provider.DefaultRuntimeMode) {
		t.Fatalf("RuntimeMode = %q, want default", got.RuntimeMode)
	}
	if got.FastMode {
		t.Fatalf("FastMode should default to false")
	}
}

func TestFallbackProfile_PreservesExplicitInputs(t *testing.T) {
	got := FallbackProfile(string(provider.Claude), "claude-haiku-4-5")
	if got.Provider != string(provider.Claude) {
		t.Fatalf("Provider = %q", got.Provider)
	}
	if got.Model != "claude-haiku-4-5" {
		t.Fatalf("Model = %q", got.Model)
	}
}

func TestFallbackProfile_RoutesBlankProviderViaAvailability(t *testing.T) {
	// Only Codex available, no provider preference → seed picks Codex.
	got := FallbackProfile("", "", string(provider.Codex))
	if got.Provider != string(provider.Codex) {
		t.Fatalf("FallbackProfile(blank, [codex]) provider = %q, want %q", got.Provider, provider.Codex)
	}
	if got.Model == "" {
		t.Fatalf("model should be filled from Codex registry, got empty")
	}
}

func TestProfileFromThread_DropsFastModeWhenUnsupported(t *testing.T) {
	thread := store.Thread{
		Provider: string(provider.Claude),
		Model:    "claude-haiku-4-5",
		FastMode: true,
	}
	got := ProfileFromThread(thread)
	if got.FastMode {
		t.Fatalf("Haiku doesn't advertise fast-mode; expected FastMode=false, got true")
	}
}

func TestProfileFromThread_KeepsFastModeWhenSupported(t *testing.T) {
	thread := store.Thread{
		Provider: string(provider.Claude),
		Model:    "claude-opus-4-7",
		FastMode: true,
	}
	got := ProfileFromThread(thread)
	if !got.FastMode {
		t.Fatalf("Opus 4.7 advertises fast-mode; expected FastMode=true")
	}
}

func TestSameProfile_NormalizesRuntimeMode(t *testing.T) {
	a := store.ChatModelProfile{Provider: "claude", Model: "x", RuntimeMode: "  approval_required  "}
	b := store.ChatModelProfile{Provider: "claude", Model: "x", RuntimeMode: "approval_required"}
	if !SameProfile(a, b) {
		t.Fatalf("expected equal under runtime-mode normalization (a=%q b=%q)", a.RuntimeMode, b.RuntimeMode)
	}
}

func TestSameProfile_RejectsContextWindowMismatch(t *testing.T) {
	a := store.ChatModelProfile{ContextWindow: 100_000}
	b := store.ChatModelProfile{ContextWindow: 200_000}
	if SameProfile(a, b) {
		t.Fatalf("differing ContextWindow should make profiles unequal")
	}
}

func TestSanitizeProfile_DropsUnsupportedFastMode(t *testing.T) {
	in := store.ChatModelProfile{
		Provider: string(provider.Claude),
		Model:    "claude-haiku-4-5",
		FastMode: true,
	}
	got := SanitizeProfile(in)
	if got.FastMode {
		t.Fatalf("SanitizeProfile should clear FastMode for non-fast-mode model")
	}
}

func TestSanitizeProfile_ClampsContextWindow(t *testing.T) {
	in := store.ChatModelProfile{
		Provider:      string(provider.Claude),
		Model:         "claude-opus-4-7",
		ContextWindow: 1, // nonsense — registry will reject
	}
	got := SanitizeProfile(in)
	if got.ContextWindow == 1 {
		t.Fatalf("SanitizeProfile left bogus ContextWindow untouched")
	}
}

func TestSanitizeContextWindow_PassesThroughWhenRegistryEmpty(t *testing.T) {
	// Unknown provider/model — registry has nothing; positive tokens pass.
	if got := SanitizeContextWindow("bogus", "bogus", 50_000); got != 50_000 {
		t.Fatalf("SanitizeContextWindow on unknown registry returned %d, want pass-through", got)
	}
}

func TestSanitizeContextWindow_FallsBackToDefaultWhenZeroAndRegistryEmpty(t *testing.T) {
	// Zero tokens, unknown registry — falls through to provider.DefaultContextWindowForModel
	// which lands on ClaudeStandardContextWindow for unknown providers.
	if got := SanitizeContextWindow("bogus", "bogus", 0); got <= 0 {
		t.Fatalf("SanitizeContextWindow on unknown registry/zero returned non-positive %d", got)
	}
}

func TestSanitizeThreadClampsModelFields(t *testing.T) {
	// Bogus stored thread: an obviously-wrong context window and a
	// fast-mode flag on a non-fast-mode model.
	in := store.Thread{
		Provider:        string(provider.Claude),
		Model:           "claude-haiku-4-5",
		ContextWindow:   1, // registry will reject
		FastMode:        true,
		ReasoningEffort: "high",
	}
	got := SanitizeThread(in)
	if got.ContextWindow == 1 {
		t.Fatalf("SanitizeThread left bogus ContextWindow untouched: %+v", got)
	}
	if got.FastMode {
		t.Fatalf("SanitizeThread did not clear FastMode on non-fast-mode model: %+v", got)
	}
}

func TestSameModelFieldsMatchesAcrossUnrelatedFields(t *testing.T) {
	a := store.Thread{
		ID:              "a",
		Title:           "thread A",
		Model:           "claude-opus-4-7",
		ContextWindow:   provider.ClaudeStandardContextWindow,
		ReasoningEffort: "high",
		FastMode:        false,
	}
	b := store.Thread{
		ID:              "b",
		Title:           "thread B",
		Model:           "claude-opus-4-7",
		ContextWindow:   provider.ClaudeStandardContextWindow,
		ReasoningEffort: "high",
		FastMode:        false,
	}
	if !SameModelFields(a, b) {
		t.Fatal("SameModelFields = false, want true (unrelated fields differ)")
	}
}

func TestSameModelFieldsRejectsModelFieldDifference(t *testing.T) {
	base := store.Thread{
		Model:           "claude-opus-4-7",
		ContextWindow:   provider.ClaudeStandardContextWindow,
		ReasoningEffort: "high",
	}
	flips := []store.Thread{
		{Model: "claude-opus-4-6", ContextWindow: provider.ClaudeStandardContextWindow, ReasoningEffort: "high"},
		{Model: "claude-opus-4-7", ContextWindow: provider.ClaudeExtendedContextWindow, ReasoningEffort: "high"},
		{Model: "claude-opus-4-7", ContextWindow: provider.ClaudeStandardContextWindow, ReasoningEffort: "low"},
		{Model: "claude-opus-4-7", ContextWindow: provider.ClaudeStandardContextWindow, ReasoningEffort: "high", FastMode: true},
	}
	for i, b := range flips {
		if SameModelFields(base, b) {
			t.Errorf("SameModelFields(case %d) = true, want false; b=%+v", i, b)
		}
	}
}

func TestValidateContextUpdate_AcceptsKnownPair(t *testing.T) {
	provName, model, err := ValidateContextUpdate(
		"  "+string(provider.Claude)+"  ",
		"  claude-opus-4-7  ",
		provider.ClaudeStandardContextWindow,
		0, 0,
	)
	if err != nil {
		t.Fatalf("ValidateContextUpdate err = %v", err)
	}
	if provName != string(provider.Claude) || model != "claude-opus-4-7" {
		t.Fatalf("trimmed (%q, %q), want (claude, claude-opus-4-7)", provName, model)
	}
}

func TestValidateContextUpdate_RejectsEmptyProviderOrModel(t *testing.T) {
	if _, _, err := ValidateContextUpdate("", "model", 200000, 0, 0); err == nil {
		t.Fatal("ValidateContextUpdate(empty provider) = nil, want error")
	}
	if _, _, err := ValidateContextUpdate("claude", "", 200000, 0, 0); err == nil {
		t.Fatal("ValidateContextUpdate(empty model) = nil, want error")
	}
	if _, _, err := ValidateContextUpdate("   ", "   ", 200000, 0, 0); err == nil {
		t.Fatal("ValidateContextUpdate(whitespace pair) = nil, want error")
	}
}

func TestValidateContextUpdate_RejectsUnknownPair(t *testing.T) {
	_, _, err := ValidateContextUpdate("bogus", "bogus", 200000, 0, 0)
	if err == nil {
		t.Fatal("ValidateContextUpdate(unknown pair) = nil, want error")
	}
}

func TestValidateContextUpdate_RejectsUnsupportedContextWindow(t *testing.T) {
	_, _, err := ValidateContextUpdate(string(provider.Claude), "claude-opus-4-7", 12345, 0, 0)
	if err == nil {
		t.Fatal("ValidateContextUpdate(unsupported window) = nil, want error")
	}
}

func TestValidateContextUpdate_RejectsOutOfRangeAutoCompactPercent(t *testing.T) {
	if _, _, err := ValidateContextUpdate(string(provider.Claude), "claude-opus-4-7", provider.ClaudeStandardContextWindow, -1, 0); err == nil {
		t.Fatal("ValidateContextUpdate(negative standard) = nil, want error")
	}
	if _, _, err := ValidateContextUpdate(string(provider.Claude), "claude-opus-4-7", provider.ClaudeStandardContextWindow, 0, 91); err == nil {
		t.Fatal("ValidateContextUpdate(extended > 90) = nil, want error")
	}
}

func TestContextWindowSupported(t *testing.T) {
	options := []provider.ContextWindowOption{
		{Tokens: 100_000},
		{Tokens: 200_000},
	}
	if !ContextWindowSupported(options, 100_000) {
		t.Fatalf("expected 100_000 to be supported")
	}
	if ContextWindowSupported(options, 150_000) {
		t.Fatalf("150_000 should not be supported (registry mismatch)")
	}
	if ContextWindowSupported(nil, 100_000) {
		t.Fatalf("nil options should reject everything")
	}
}

func TestIsValidContextWindow(t *testing.T) {
	if !IsValidContextWindow(1) {
		t.Fatalf("positive should be valid")
	}
	if IsValidContextWindow(0) {
		t.Fatalf("zero should be invalid")
	}
	if IsValidContextWindow(-100) {
		t.Fatalf("negative should be invalid")
	}
}

func TestSupportsStoredFastMode_RegistryHit(t *testing.T) {
	if !SupportsStoredFastMode(string(provider.Claude), "claude-opus-4-7") {
		t.Fatalf("Opus 4.7 advertises fast-mode")
	}
	if SupportsStoredFastMode(string(provider.Claude), "claude-haiku-4-5") {
		t.Fatalf("Haiku 4.5 does not advertise fast-mode")
	}
}

func TestSupportsStoredFastMode_CodexUnknownModelPermissive(t *testing.T) {
	// Codex's live model catalog (not the static registry) is the source
	// of truth for Codex fast-mode. An unknown Codex model returns true
	// so a remembered-favorite slug isn't silently dropped on startup
	// before the live catalog has loaded.
	if !SupportsStoredFastMode(string(provider.Codex), "gpt-future-model") {
		t.Fatalf("unknown Codex model should be permissive")
	}
}

func TestSupportsStoredFastMode_CodexEmptyModelRejected(t *testing.T) {
	if SupportsStoredFastMode(string(provider.Codex), "  ") {
		t.Fatalf("empty Codex model should not be permissive")
	}
}

func TestSupportsStoredFastMode_UnknownProviderRejected(t *testing.T) {
	if SupportsStoredFastMode("bogus", "something") {
		t.Fatalf("unknown provider should be rejected")
	}
}

func TestHasCapability(t *testing.T) {
	m := provider.ModelInfo{Capabilities: []string{"a", "b"}}
	if !HasCapability(m, "a") {
		t.Fatalf("a should be present")
	}
	if HasCapability(m, "c") {
		t.Fatalf("c should be absent")
	}
	if HasCapability(provider.ModelInfo{}, "x") {
		t.Fatalf("empty model has no capabilities")
	}
}

func TestDefaultContextWindow(t *testing.T) {
	if got := DefaultContextWindow(string(provider.Claude), "claude-opus-4-7", 0); got <= 0 {
		t.Fatalf("expected positive default for known model, got %d", got)
	}
	if got := DefaultContextWindow("bogus", "bogus", 12345); got != 12345 {
		t.Fatalf("unknown model should return fallback (12345), got %d", got)
	}
}

// TestStoredContextWindowSurvivesLargeModelDefaultFlip is the persisted-thread
// guard for the 1M-by-default change: the new default applies at thread
// creation / new-session seeding only. An existing thread that stored 200k on
// a model whose default is now 1M must come back out of every sanitizer with
// 200k intact.
func TestStoredContextWindowSurvivesLargeModelDefaultFlip(t *testing.T) {
	const model = "claude-opus-5"
	if got := provider.DefaultContextWindowForModel(string(provider.Claude), model, 0); got != provider.ClaudeExtendedContextWindow {
		t.Fatalf("precondition: %s default = %d, want the extended tier", model, got)
	}

	t.Run("SanitizeContextWindow", func(t *testing.T) {
		if got := SanitizeContextWindow(string(provider.Claude), model, provider.ClaudeStandardContextWindow); got != provider.ClaudeStandardContextWindow {
			t.Fatalf("stored 200k rewritten to %d", got)
		}
	})

	t.Run("SanitizeProfile", func(t *testing.T) {
		got := SanitizeProfile(store.ChatModelProfile{
			Provider:      string(provider.Claude),
			Model:         model,
			ContextWindow: provider.ClaudeStandardContextWindow,
		})
		if got.ContextWindow != provider.ClaudeStandardContextWindow {
			t.Fatalf("stored 200k rewritten to %d", got.ContextWindow)
		}
	})

	t.Run("SanitizeThread", func(t *testing.T) {
		got := SanitizeThread(store.Thread{
			Provider:        string(provider.Claude),
			Model:           model,
			ContextWindow:   provider.ClaudeStandardContextWindow,
			ReasoningEffort: "xhigh",
		})
		if got.ContextWindow != provider.ClaudeStandardContextWindow {
			t.Fatalf("stored 200k rewritten to %d", got.ContextWindow)
		}
	})

	t.Run("ProfileFromThread", func(t *testing.T) {
		got := ProfileFromThread(store.Thread{
			Provider:        string(provider.Claude),
			Model:           model,
			ContextWindow:   provider.ClaudeStandardContextWindow,
			ReasoningEffort: "xhigh",
		})
		if got.ContextWindow != provider.ClaudeStandardContextWindow {
			t.Fatalf("stored 200k rewritten to %d", got.ContextWindow)
		}
	})

	// The flip does reach a brand-new profile, which has no stored choice.
	t.Run("FallbackProfileTakesTheNewDefault", func(t *testing.T) {
		got := FallbackProfile(string(provider.Claude), model)
		if got.ContextWindow != provider.ClaudeExtendedContextWindow {
			t.Fatalf("new profile ContextWindow = %d, want the extended tier", got.ContextWindow)
		}
	})

	// Sonnet keeps 1M opt-in, so a new Sonnet profile still starts at 200k.
	t.Run("SonnetNewProfileStaysStandard", func(t *testing.T) {
		got := FallbackProfile(string(provider.Claude), "claude-sonnet-5")
		if got.ContextWindow != provider.ClaudeStandardContextWindow {
			t.Fatalf("new sonnet profile ContextWindow = %d, want the standard tier", got.ContextWindow)
		}
	})
}
