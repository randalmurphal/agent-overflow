package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// TestContextSettingsResolveWireOnlyClaudeModels is the claude-fable-5-1
// regression (2026-09-01): a model the CLI ships before the static registry
// lists it exists only in the probe-merged catalog, so the context-settings
// surface hard-failed with "unknown provider/model" while the picker happily
// listed the model. The merged entry inherits the fable-5 family windows, so
// the 200k/1M pair must resolve with the 1M default and both settings paths
// must accept either option.
func TestContextSettingsResolveWireOnlyClaudeModels(t *testing.T) {
	app := newTestAppWithStore(t)
	seedWireOnlyFable51(t, app)

	profile, err := app.GetContextSettings("claude", "claude-fable-5-1")
	if err != nil {
		t.Fatalf("GetContextSettings(claude/claude-fable-5-1): %v", err)
	}
	if len(profile.ContextWindows) != 2 {
		t.Fatalf("ContextWindows = %+v, want 200k + 1M", profile.ContextWindows)
	}
	if profile.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("ContextWindow = %d, want the 1M family default %d", profile.ContextWindow, provider.ClaudeExtendedContextWindow)
	}

	updated, err := app.UpdateContextSettingsProfile(ContextSettingsUpdate{
		Provider:      "claude",
		Model:         "claude-fable-5-1",
		ContextWindow: provider.ClaudeStandardContextWindow,
	})
	if err != nil {
		t.Fatalf("UpdateContextSettingsProfile(claude-fable-5-1 → 200k): %v", err)
	}
	if updated.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("updated ContextWindow = %d, want %d", updated.ContextWindow, provider.ClaudeStandardContextWindow)
	}
}

// seedWireOnlyFable51 stages the probe-merged catalog the way the zero-token
// account probe does: the CLI lists claude-fable-5-1, the static registry does
// not, so the merged entry is wire-only with fable-5 family windows (1M default).
func seedWireOnlyFable51(t *testing.T, app *App) {
	t.Helper()
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	capture := claudecatalog.ModelCapture{}
	capture.Capture([]claude.WireModel{{
		Value:         "fable",
		ResolvedModel: "claude-fable-5-1",
		DisplayName:   "Fable",
	}}, nil)
	capture.Store(app.claudeProbeModelKey())
}

// The new-thread paint path and thread materialization seed a wire-only model
// from the catalog default too. The static fallback's 200k IS a supported
// option, so without the fallback re-stamp a fresh fable-5-1 thread would
// silently start at 200k while the settings modal advertised 1M.
func TestThreadDefaultsSeedWireOnlyClaudeModelFromCatalogDefault(t *testing.T) {
	app := newTestAppWithStore(t)
	seedWireOnlyFable51(t, app)

	defaults, err := app.GetThreadDefaults(CreateThreadOptions{
		ProjectID: defaultTestProjectID, Provider: "claude", Model: "claude-fable-5-1",
	})
	if err != nil {
		t.Fatalf("GetThreadDefaults(claude-fable-5-1): %v", err)
	}
	if defaults.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("defaults ContextWindow = %d, want 1M %d", defaults.ContextWindow, provider.ClaudeExtendedContextWindow)
	}

	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID: defaultTestProjectID, Provider: "claude", Model: "claude-fable-5-1",
	})
	if err != nil {
		t.Fatalf("CreateThread(claude-fable-5-1): %v", err)
	}
	if thread.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("created ContextWindow = %d, want 1M %d", thread.ContextWindow, provider.ClaudeExtendedContextWindow)
	}

	// An explicit choice is still the user's: 200k stays 200k.
	standard, err := app.CreateThread(CreateThreadOptions{
		ProjectID: defaultTestProjectID, Provider: "claude", Model: "claude-fable-5-1",
		ContextWindow: provider.ClaudeStandardContextWindow,
	})
	if err != nil {
		t.Fatalf("CreateThread(claude-fable-5-1, 200k): %v", err)
	}
	if standard.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("explicit ContextWindow = %d, want %d", standard.ContextWindow, provider.ClaudeStandardContextWindow)
	}

	// Per-thread context settings validate against the same merged options.
	switched, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		ContextWindow: provider.ClaudeStandardContextWindow,
	})
	if err != nil {
		t.Fatalf("UpdateThreadContextSettings(claude-fable-5-1 → 200k): %v", err)
	}
	if switched.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("thread ContextWindow after update = %d, want %d", switched.ContextWindow, provider.ClaudeStandardContextWindow)
	}

	// So do the project's new-thread defaults, in both directions.
	updated, err := app.UpdateNewThreadDefaults(NewThreadDefaultsUpdate{
		ProjectID: defaultTestProjectID, Provider: "claude", Model: "claude-fable-5-1",
		ContextWindow: provider.ClaudeExtendedContextWindow,
	})
	if err != nil {
		t.Fatalf("UpdateNewThreadDefaults(claude-fable-5-1, 1M): %v", err)
	}
	if updated.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("new-thread default ContextWindow = %d, want 1M", updated.ContextWindow)
	}
	if _, err := app.UpdateNewThreadDefaults(NewThreadDefaultsUpdate{
		ProjectID: defaultTestProjectID, Provider: "claude", Model: "claude-fable-5-1",
		ContextWindow: 12345,
	}); err == nil {
		t.Fatal("UpdateNewThreadDefaults accepted a window the merged catalog does not offer")
	}
}

func TestGetContextSettingsReturnsProviderModelOptions(t *testing.T) {
	app := newTestAppWithStore(t)

	profile, err := app.GetContextSettings("codex", "gpt-5.4")
	if err != nil {
		t.Fatalf("GetContextSettings(codex/gpt-5.4): %v", err)
	}
	if profile.ContextWindow != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindow = %d, want default %d", profile.ContextWindow, provider.CodexStandardContextWindow)
	}
	if len(profile.ContextWindows) != 2 {
		t.Fatalf("ContextWindows len = %d, want 2", len(profile.ContextWindows))
	}
	if profile.ContextWindows[0].Tokens != provider.CodexStandardContextWindow ||
		profile.ContextWindows[1].Tokens != provider.CodexExtendedContextWindow {
		t.Fatalf("ContextWindows = %+v, want codex standard + extended", profile.ContextWindows)
	}
}

func TestGetContextSettingsReturnsStandardOnlyForCodexGPT55(t *testing.T) {
	app := newTestAppWithStore(t)

	profile, err := app.GetContextSettings("codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("GetContextSettings(codex/gpt-5.5): %v", err)
	}
	if profile.ContextWindow != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", profile.ContextWindow, provider.CodexStandardContextWindow)
	}
	if len(profile.ContextWindows) != 1 || profile.ContextWindows[0].Tokens != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindows = %+v, want standard-only", profile.ContextWindows)
	}
}

func TestGetContextSettingsReturnsGPT56Window(t *testing.T) {
	app := newTestAppWithStore(t)

	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		t.Run(model, func(t *testing.T) {
			profile, err := app.GetContextSettings("codex", model)
			if err != nil {
				t.Fatalf("GetContextSettings(codex/%s): %v", model, err)
			}
			if profile.ContextWindow != provider.Codex56ContextWindow {
				t.Fatalf("ContextWindow = %d, want %d", profile.ContextWindow, provider.Codex56ContextWindow)
			}
			if len(profile.ContextWindows) != 1 || profile.ContextWindows[0].Label != "372k" {
				t.Fatalf("ContextWindows = %+v, want one 372k option", profile.ContextWindows)
			}
		})
	}
}

func TestGetContextSettingsClampsStaleCodexGPT55Profile(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:      "codex",
		Model:         "gpt-5.5",
		ContextWindow: provider.CodexExtendedContextWindow,
		RuntimeMode:   "default",
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}

	profile, err := app.GetContextSettings("codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("GetContextSettings(codex/gpt-5.5): %v", err)
	}
	if profile.ContextWindow != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", profile.ContextWindow, provider.CodexStandardContextWindow)
	}
	if len(profile.ContextWindows) != 1 || profile.ContextWindows[0].Tokens != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindows = %+v, want standard-only", profile.ContextWindows)
	}
}

func TestGetContextSettingsReturnsSingleWindowCodexDefaults(t *testing.T) {
	app := newTestAppWithStore(t)

	profile, err := app.GetContextSettings("codex", "gpt-5.2")
	if err != nil {
		t.Fatalf("GetContextSettings(codex/gpt-5.2): %v", err)
	}
	if profile.ContextWindow != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", profile.ContextWindow, provider.CodexStandardContextWindow)
	}
	if len(profile.ContextWindows) != 1 || profile.ContextWindows[0].Tokens != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindows = %+v, want standard-only", profile.ContextWindows)
	}
}

func TestUpdateContextSettingsProfileValidatesPercentAndWindow(t *testing.T) {
	app := newTestAppWithStore(t)

	_, err := app.UpdateContextSettingsProfile(ContextSettingsUpdate{
		Provider:                   "claude",
		Model:                      "claude-sonnet-4-6",
		ContextWindow:              1000000,
		AutoCompactStandardPercent: 91,
	})
	if err == nil || !strings.Contains(err.Error(), "between 0 and 90") {
		t.Fatalf("UpdateContextSettingsProfile percent error = %v, want range error", err)
	}

	_, err = app.UpdateContextSettingsProfile(ContextSettingsUpdate{
		Provider:                   "codex",
		Model:                      "gpt-5.4-mini",
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactStandardPercent: 80,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported context window") {
		t.Fatalf("UpdateContextSettingsProfile unsupported window error = %v, want unsupported context window", err)
	}
}

func TestUpdateThreadContextSettingsPersistsAndRemembersProfile(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/context-settings", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		Provider:                   "ignored",
		Model:                      "ignored",
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactStandardPercent: 75,
		AutoCompactExtendedPercent: 88,
	})
	if err != nil {
		t.Fatalf("UpdateThreadContextSettings: %v", err)
	}
	if updated.ContextWindow != provider.CodexExtendedContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", updated.ContextWindow, provider.CodexExtendedContextWindow)
	}
	if updated.AutoCompactStandardPercent != 75 || updated.AutoCompactExtendedPercent != 88 {
		t.Fatalf("auto compact = %d/%d, want 75/88", updated.AutoCompactStandardPercent, updated.AutoCompactExtendedPercent)
	}

	profile, err := app.store.GetChatModelProfile("codex", "gpt-5.4")
	if err != nil {
		t.Fatalf("GetChatModelProfile: %v", err)
	}
	if profile.ContextWindow != provider.CodexExtendedContextWindow ||
		profile.AutoCompactStandardPercent != 75 ||
		profile.AutoCompactExtendedPercent != 88 {
		t.Fatalf("stored profile = %+v, want extended context and compact 75/88", profile)
	}
}

// TestUpdateThreadContextSettingsClearsLastTokenUsageAndEmitsReset
// pins Fix 6: changing context settings clears the persisted token-usage
// snapshot AND emits a `provider:usage` reset so the meter doesn't
// render stale `usedTokens` against the new max during the brief
// restart gap.
func TestUpdateThreadContextSettingsClearsLastTokenUsageAndEmitsReset(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/context-settings-reset", "gpt-5.5", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if err := app.store.UpdateLastTokenUsage(thread.ID, `{"usedTokens":180000,"maxTokens":1000000}`); err != nil {
		t.Fatalf("seed last token usage: %v", err)
	}

	var (
		emittedAction   string
		emittedThreadID string
	)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		evt, ok := data.(provider.UsageEvent)
		if !ok {
			t.Errorf("usage payload type = %T", data)
			return
		}
		emittedAction = evt.Action
		emittedThreadID = evt.ThreadID
	}

	if _, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		Provider:                   "codex",
		Model:                      "gpt-5.5",
		ContextWindow:              provider.CodexStandardContextWindow,
		AutoCompactStandardPercent: 75,
		AutoCompactExtendedPercent: 80,
	}); err != nil {
		t.Fatalf("UpdateThreadContextSettings: %v", err)
	}

	if emittedAction != "reset" || emittedThreadID != thread.ID {
		t.Fatalf("provider:usage reset = (%q, %q), want (reset, %q)", emittedAction, emittedThreadID, thread.ID)
	}
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.LastTokenUsage != "" {
		t.Fatalf("LastTokenUsage = %q, want empty", got.LastTokenUsage)
	}
}

func TestUpdateThreadContextSettingsRestartsActiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/context-settings-active", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "active-context-token"})

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		Provider:                   "codex",
		Model:                      "gpt-5.4",
		ContextWindow:              provider.CodexExtendedContextWindow,
		AutoCompactStandardPercent: 75,
		AutoCompactExtendedPercent: 88,
	}); err != nil {
		t.Fatalf("UpdateThreadContextSettings: %v", err)
	}

	select {
	case got := <-started:
		if got != thread.ID {
			t.Fatalf("restart threadID = %q, want %q", got, thread.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("UpdateThreadContextSettings did not restart active session")
	}
}
