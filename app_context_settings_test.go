package main

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestGetContextSettingsReturnsProviderModelOptions(t *testing.T) {
	app := newTestAppWithStore(t)

	profile, err := app.GetContextSettings("codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("GetContextSettings(codex/gpt-5.5): %v", err)
	}
	if profile.ContextWindow != 272000 {
		t.Fatalf("ContextWindow = %d, want default 272000", profile.ContextWindow)
	}
	if len(profile.ContextWindows) != 2 {
		t.Fatalf("ContextWindows len = %d, want 2", len(profile.ContextWindows))
	}
	if profile.ContextWindows[0].Tokens != 272000 || profile.ContextWindows[1].Tokens != 1050000 {
		t.Fatalf("ContextWindows = %+v, want 272000 + 1050000", profile.ContextWindows)
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
		ContextWindow:              1050000,
		AutoCompactStandardPercent: 80,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported context window") {
		t.Fatalf("UpdateContextSettingsProfile unsupported window error = %v, want unsupported context window", err)
	}
}

func TestUpdateThreadContextSettingsPersistsAndRemembersProfile(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/context-settings", "gpt-5.5", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		Provider:                   "ignored",
		Model:                      "ignored",
		ContextWindow:              1050000,
		AutoCompactStandardPercent: 75,
		AutoCompactExtendedPercent: 88,
	})
	if err != nil {
		t.Fatalf("UpdateThreadContextSettings: %v", err)
	}
	if updated.ContextWindow != 1050000 {
		t.Fatalf("ContextWindow = %d, want 1050000", updated.ContextWindow)
	}
	if updated.AutoCompactStandardPercent != 75 || updated.AutoCompactExtendedPercent != 88 {
		t.Fatalf("auto compact = %d/%d, want 75/88", updated.AutoCompactStandardPercent, updated.AutoCompactExtendedPercent)
	}

	profile, err := app.store.GetChatModelProfile("codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("GetChatModelProfile: %v", err)
	}
	if profile.ContextWindow != 1050000 ||
		profile.AutoCompactStandardPercent != 75 ||
		profile.AutoCompactExtendedPercent != 88 {
		t.Fatalf("stored profile = %+v, want context 1050000 and compact 75/88", profile)
	}
}

func TestUpdateThreadContextSettingsRestartsActiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/context-settings-active", "gpt-5.5", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "active-context-token"}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{
		Provider:                   "codex",
		Model:                      "gpt-5.5",
		ContextWindow:              1050000,
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
