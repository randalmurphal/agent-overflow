package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/promptoverride"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

// The Settings wire never carries a prompt override's text: it lives in the
// entry's file, which the user edits. An added entry gets an empty file and
// reads as blank; text written to the file is what the next read reports as
// not blank; text a client sends is ignored; a removed entry's file is
// deleted.
func TestPromptOverrideTextStaysOffTheSettingsWire(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	attachPromptFileStore(t, app)
	ctx := context.Background()

	saved, err := app.UpdateSettings(ctx, map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{"enabled": true, "models": []any{"claude-opus-5"}, "prompt": "client text"},
		},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	added := saved.ClaudePromptOverrides[0]
	if added.Prompt != "" || !added.Blank || added.ID == "" {
		t.Fatalf("added entry on the wire = %+v, want no text, blank, and an id", added)
	}
	if filepath.Dir(added.Path) != filepath.Join(app.configDir, "prompts") {
		t.Fatalf("added entry path = %q, want a file under the config dir", added.Path)
	}
	if data, err := os.ReadFile(added.Path); err != nil || len(data) != 0 {
		t.Fatalf("added entry's file = %q (%v), want an empty file", data, err)
	}

	if err := os.WriteFile(added.Path, []byte("Written by hand.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := app.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	entry := read.ClaudePromptOverrides[0]
	if entry.Prompt != "" || entry.Blank {
		t.Fatalf("entry after the file was written = %+v, want not blank and no text", entry)
	}
	raw, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Written by hand") || strings.Contains(string(raw), `"prompt"`) {
		t.Fatalf("GetSettings JSON carries the prompt text: %s", raw)
	}
	if got := app.settings.Get().ClaudePromptOverrides[0].Prompt; got != "Written by hand." {
		t.Fatalf("the service's own read = %q, want the file's text", got)
	}

	// A client that sends text changes nothing in the file.
	if _, err := app.UpdateSettings(ctx, map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{"id": entry.ID, "enabled": false, "models": []any{"claude-opus-5"}, "prompt": "overwrite attempt"},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings toggle: %v", err)
	}
	if data, err := os.ReadFile(entry.Path); err != nil || string(data) != "Written by hand.\n" {
		t.Fatalf("file after a write carrying text = %q (%v), want it untouched", data, err)
	}

	// An oversized file is blank on the wire and names its problem.
	if err := os.WriteFile(entry.Path, []byte(strings.Repeat("x", settings.MaxPromptOverrideLen+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err = app.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got := read.ClaudePromptOverrides[0]; !got.Blank || got.Problem == "" || got.Prompt != "" {
		t.Fatalf("oversized entry on the wire = %+v, want blank with a problem", got)
	}

	if _, err := app.UpdateSettings(ctx, map[string]any{"claudePromptOverrides": []any{}}); err != nil {
		t.Fatalf("UpdateSettings remove: %v", err)
	}
	if _, err := os.Stat(entry.Path); !os.IsNotExist(err) {
		t.Fatalf("removed entry's file: %v, want it deleted", err)
	}
}

// Boot writes the prompts directory README, and restores it when it was
// edited.
func TestBootWritesThePromptsReadme(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.initPromptDirectory()
	t.Cleanup(func() { _ = app.promptWatcher.Close() })

	path := filepath.Join(app.settings.PromptFilesDir(), settings.PromptReadmeName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(data) != promptoverride.ReadmeMarkdown() {
		t.Fatalf("README = %q, want the generated reference", data)
	}
	if err := os.WriteFile(path, []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePromptReadme(app.settings.PromptFilesDir()); err != nil {
		t.Fatalf("writePromptReadme: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != promptoverride.ReadmeMarkdown() {
		t.Fatalf("README after an edit and a boot = %q (%v), want the generated reference", data, err)
	}
}

// A prompt file edited outside the app is announced to attached clients by
// the prompts watcher and flags the live thread whose next start would use
// it, while the running session receives nothing.
func TestAnEditedPromptFileFlagsTheLiveThreadAndNeverReachesIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	threadID, workspace := seedPromptOverrideThread(t, app, "thread-prompt-file", string(provider.Claude), "claude-opus-5")
	// Installed before any save so no emitter races the assignment. A save
	// of the Claude list names only that key; a reload names both.
	reloads := make(chan []string, 16)
	app.emitEventFn = func(name string, data any) {
		if name != string(eventchan.SettingsUpdated) {
			return
		}
		if evt, ok := data.(SettingsUpdatedEvent); ok && slices.Contains(evt.Keys, "codexPromptOverrides") {
			reloads <- evt.Keys
		}
	}
	entry := setPromptOverrides(t, app, "claudePromptOverrides",
		testPromptEntry{Enabled: true, Models: []string{"claude-opus-5"}, Prompt: "First house prompt."})[0]
	<-reloads // the fixture's own reload
	capture := registerLiveClaudeCaptureSession(t, app, threadID, workspace, promptOverrideOptions(t, app, threadID))
	// The reconcile runs on the watcher's goroutine right after the reload
	// is announced, so its event is awaited rather than read at once.
	stale := make(chan providerstatus.Event, 16)
	app.testEmitHook = func(name string, data any) {
		if event, ok := data.(providerstatus.Event); ok && name == string(eventchan.ProviderStatus) && event.Kind == providerstatus.KindPromptStale {
			stale <- event
		}
	}

	app.initPromptDirectory()
	if app.promptWatcher == nil {
		t.Fatal("the prompts watcher did not start")
	}
	t.Cleanup(func() { _ = app.promptWatcher.Close() })

	const edited = "Edited outside the app."
	if err := os.WriteFile(entry.Path, []byte(edited+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case keys := <-reloads:
		if !slices.Equal(keys, []string{"claudePromptOverrides", "codexPromptOverrides"}) {
			t.Fatalf("reload announced %v", keys)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an external edit never announced a reload")
	}
	if got := app.settings.Get().ClaudePromptOverrides[0].Prompt; got != edited {
		t.Fatalf("settings prompt after the edit = %q", got)
	}
	select {
	case event := <-stale:
		expectOnePromptStale(t, []providerstatus.Event{event}, threadID, string(provider.Claude))
	case <-time.After(10 * time.Second):
		t.Fatal("an external edit never flagged the live thread")
	}
	if body, err := os.ReadFile(capture); err == nil && strings.Contains(string(body), `"control_request"`) {
		t.Fatalf("the edited prompt file reached the live session: %s", body)
	}
}
