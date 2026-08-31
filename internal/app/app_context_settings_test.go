package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

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
