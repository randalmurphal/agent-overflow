package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// An override edited as a file reaches a running Claude session: the
// prompts watcher reloads the texts, announces both keys to attached
// clients, and reconciles, which swaps the live prompt over set_model. The
// app's own saves are suppressed and do not come back as a reload.
func TestAnEditedPromptFileReachesALiveClaudeSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.settings.AttachTierStore(app.store, "client:prompt-files-test")
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

	saved, err := app.UpdateSettings(context.Background(), map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{"enabled": true, "models": []any{"claude-opus-5"}, "prompt": "First house prompt."},
		},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	entry := saved.ClaudePromptOverrides[0]
	if entry.Path == "" || filepath.Dir(entry.Path) != filepath.Join(app.configDir, "prompts") {
		t.Fatalf("saved entry = %+v, want a file under the config dir", entry)
	}

	binary, capture := writeClaudeControlCaptureCLI(t)
	initSeen := make(chan struct{})
	var once sync.Once
	sess, err := claude.NewSession(context.Background(), threadID, claude.Config{
		Binary:  binary,
		Model:   "claude-opus-5",
		WorkDir: workspace,
	}, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			once.Do(func() { close(initSeen) })
		}
	})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	select {
	case <-initSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake CLI never announced its version")
	}
	app.sessionManager().put(threadID, session{
		Provider:      string(provider.Claude),
		Token:         "prompt-file-token",
		Claude:        sess,
		LaunchOptions: promptOverrideOptions(t, app, threadID),
		Liveness:      newSessionLiveness(time.Now()),
	})

	app.initPromptDirectory()
	if app.promptWatcher == nil {
		t.Fatal("the prompts watcher did not start")
	}
	t.Cleanup(func() { _ = app.promptWatcher.Close() })

	// A save in the app rewrites the file, and the watcher must not echo it
	// back as an external edit. Only a reload names the codex key here.
	if _, err := app.UpdateSettings(context.Background(), map[string]any{
		"claudePromptOverrides": []any{
			map[string]any{"id": entry.ID, "enabled": true, "models": []any{"claude-opus-5"}, "prompt": "Saved in the app."},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	select {
	case keys := <-reloads:
		t.Fatalf("the app's own save came back as a reload: %v", keys)
	case <-time.After(time.Second):
	}

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
	body := waitForCaptured(t, capture, edited, "the edited prompt file never reached the live session")
	if !strings.Contains(body, `"set_model"`) {
		t.Fatalf("the edit landed on something other than set_model: %s", body)
	}
}
