package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil/mockexec"
)

// The ceiling is spawn-only: stamped from Settings on both Claude
// transports, absent on Codex, and pinned to the launch value on
// reconcile, so a settings change waits for the next session start and
// never queues a restart.
func TestCommandTimeoutIsStampedAtSpawnAndPinnedOnReconcile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		providerName string
		model        string
		want         int
	}{
		{string(provider.Claude), "claude-opus-5", 5},
		{string(provider.ClaudeTUI), "claude-opus-5", 5},
		{string(provider.Codex), "gpt-5.6-codex", 0},
	}
	for _, tc := range cases {
		app := newTestAppWithStore(t)
		id, _ := seedPromptOverrideThread(t, app, "thread-timeout-"+tc.providerName, tc.providerName, tc.model)

		launch := promptOverrideOptions(t, app, id)
		if launch.ClaudeMaxCommandTimeoutHours != tc.want {
			t.Fatalf("%s spawn ClaudeMaxCommandTimeoutHours = %d, want %d",
				tc.providerName, launch.ClaudeMaxCommandTimeoutHours, tc.want)
		}

		if _, err := app.settings.Update(map[string]any{"claudeMaxCommandTimeoutHours": 9}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		reconciled := reconcileOptionsFor(t, app, id, launch)
		if reconciled.ClaudeMaxCommandTimeoutHours != tc.want {
			t.Fatalf("%s reconcile ClaudeMaxCommandTimeoutHours = %d, want the launch value %d",
				tc.providerName, reconciled.ClaudeMaxCommandTimeoutHours, tc.want)
		}
		if tc.providerName == string(provider.Claude) {
			if _, ok := claude.PlanLiveUpdate(launch, reconciled); !ok {
				t.Fatal("a command timeout change queued a restart on a running Claude session")
			}
		}

		next := promptOverrideOptions(t, app, id)
		if tc.want != 0 && next.ClaudeMaxCommandTimeoutHours != 9 {
			t.Fatalf("%s next spawn ClaudeMaxCommandTimeoutHours = %d, want 9",
				tc.providerName, next.ClaudeMaxCommandTimeoutHours)
		}
	}
}

// startClaudeTUIRecordingTimeout starts a claude-tui session whose binary
// records the BASH_MAX_TIMEOUT_MS it was spawned with, and returns it.
func startClaudeTUIRecordingTimeout(t *testing.T, app *App, id string) string {
	t.Helper()
	dir := t.TempDir()
	thread := createAppTestThread(t, app, id, string(provider.ClaudeTUI), dir)
	record := filepath.Join(t.TempDir(), "env")
	binary := filepath.Join(t.TempDir(), "mock-claude-tui")
	mockexec.Write(t, binary, "#!/bin/sh\nprintf 'set=%s value=%s' \"${BASH_MAX_TIMEOUT_MS+yes}\" \"$BASH_MAX_TIMEOUT_MS\" > "+
		record+".tmp && mv "+record+".tmp "+record+"\nwhile IFS= read -r _; do :; done\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(record)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("claude-tui mock never recorded its environment: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The claude-tui spawn composes a whole environment rather than an
// override map, so its precedence is proven on the real process: the
// setting outranks a value inherited from the host, and a value in the
// provider's custom environment outranks the setting. Serial: it sets the
// process environment.
func TestClaudeTUIReceivesTheCommandTimeoutCeiling(t *testing.T) {
	t.Setenv("BASH_MAX_TIMEOUT_MS", "60000")

	app := newTestAppWithStore(t)
	if got := startClaudeTUIRecordingTimeout(t, app, "tui-timeout-default"); got != "set=yes value=18000000" {
		t.Fatalf("default spawn recorded %q, want the 5 h ceiling over the inherited value", got)
	}

	app = newTestAppWithStore(t)
	if _, err := app.settings.Update(map[string]any{"claudeMaxCommandTimeoutHours": 3}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := startClaudeTUIRecordingTimeout(t, app, "tui-timeout-3h"); got != "set=yes value=10800000" {
		t.Fatalf("3 h spawn recorded %q, want 10800000", got)
	}

	app = newTestAppWithStore(t)
	if _, err := app.SetProviderCustomEnvVar("claude", "BASH_MAX_TIMEOUT_MS", "99999999", false); err != nil {
		t.Fatalf("SetProviderCustomEnvVar: %v", err)
	}
	if got := startClaudeTUIRecordingTimeout(t, app, "tui-timeout-custom"); got != "set=yes value=99999999" {
		t.Fatalf("custom-env spawn recorded %q, want the custom value", got)
	}
}
