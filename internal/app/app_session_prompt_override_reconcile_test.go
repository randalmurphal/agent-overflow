package app

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
)

// The reconcile half of the settings-owned prompt axis. A prompt change
// never applies to a running session (docs/decisions.md 2026-10-14): on
// every provider, an override edited, enabled, disabled or removed while a
// session runs leaves that session's launch options alone, sends nothing on
// the wire, arms no deferred restart and starts no session. It applies at
// the thread's next session start.

// reconcileOptionsFor rebuilds a thread's options the way liveApplySessionConfig
// does and runs the reconcile-path resolution against a launch bundle.
func reconcileOptionsFor(
	t *testing.T, app *App, threadID string, launch provider.SessionOptions,
) provider.SessionOptions {
	t.Helper()
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	sanitized := app.sanitizeThreadModelSettings(stored)
	opts, err := app.buildSessionOptions(sanitized)
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	app.reconcileSettingsOwnedAxes(sanitized, &opts, launch)
	return opts
}

// promptOverrideTransition is one change a user makes to a provider's
// override list while a session runs: the list the session launched under,
// and the list after the change.
type promptOverrideTransition struct {
	name   string
	before []testPromptEntry
	after  []testPromptEntry
}

func promptOverrideTransitions(model string) []promptOverrideTransition {
	on := func(prompt string) testPromptEntry {
		return testPromptEntry{Enabled: true, Models: []string{model}, Prompt: prompt}
	}
	off := func(prompt string) testPromptEntry {
		return testPromptEntry{Enabled: false, Models: []string{model}, Prompt: prompt}
	}
	return []promptOverrideTransition{
		{name: "edit", before: []testPromptEntry{on("launched prompt")}, after: []testPromptEntry{on("edited prompt, longer")}},
		{name: "enable", before: []testPromptEntry{off("dormant prompt")}, after: []testPromptEntry{on("dormant prompt")}},
		{name: "disable", before: []testPromptEntry{on("launched prompt")}, after: []testPromptEntry{off("launched prompt")}},
		{name: "remove", before: []testPromptEntry{on("launched prompt")}, after: nil},
	}
}

// The reconcile path pins the settings-level prompt to what the session
// launched with on every provider and for every kind of change, so the
// rebuilt launch config equals the live one and nothing is planned.
func TestReconcileSettingsOwnedAxesPinsThePromptOnEveryProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		providerName, model, key string
	}{
		{string(provider.Claude), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.ClaudeTUI), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.Codex), "gpt-5.4", "codexPromptOverrides"},
	} {
		for _, change := range promptOverrideTransitions(tc.model) {
			t.Run(tc.providerName+"/"+change.name, func(t *testing.T) {
				t.Parallel()
				app := newTestAppWithStore(t)
				id, _ := seedPromptOverrideThread(t, app, "thread-pin-"+tc.providerName+"-"+change.name, tc.providerName, tc.model)
				setPromptOverrides(t, app, tc.key, change.before...)
				launch := promptOverrideOptions(t, app, id)

				setPromptOverrides(t, app, tc.key, change.after...)
				opts := reconcileOptionsFor(t, app, id, launch)

				if opts.SystemPrompt != launch.SystemPrompt || opts.SystemPromptOverrideSource != launch.SystemPromptOverrideSource {
					t.Fatalf("reconciled prompt = (%q, %q), want the launched (%q, %q)",
						opts.SystemPrompt, opts.SystemPromptOverrideSource, launch.SystemPrompt, launch.SystemPromptOverrideSource)
				}
				switch tc.providerName {
				case string(provider.Claude):
					if update, ok := claude.PlanLiveUpdate(launch, opts); !ok || !update.Empty() {
						t.Fatalf("claude.PlanLiveUpdate = (%+v, %v), want nothing to apply", update, ok)
					}
				case string(provider.ClaudeTUI):
					if !reflect.DeepEqual(claudetui.ConfigFromOptions(opts), claudetui.ConfigFromOptions(launch)) {
						t.Fatal("the rebuilt claude-tui launch config differs from the live one; a reconcile would restart it")
					}
				case string(provider.Codex):
					// Codex re-sends its per-turn overrides on every turn, so
					// the plan is never empty; ok false would be a restart.
					if _, ok := codex.PlanLiveUpdate(launch, opts); !ok {
						t.Fatal("codex.PlanLiveUpdate refused the rebuilt options; a reconcile would restart the session")
					}
				}
				// The next spawn is where the change lands.
				next := promptOverrideOptions(t, app, id)
				wantNext := ""
				if len(change.after) > 0 && change.after[0].Enabled {
					wantNext = change.after[0].Prompt
				}
				if next.SystemPrompt != wantNext {
					t.Fatalf("next-spawn SystemPrompt = %q, want %q", next.SystemPrompt, wantNext)
				}
			})
		}
	}
}

// The same transitions against running sessions, through the reconciler
// itself: a reconcile after the change converges without a restart, sends no
// control request (the fake Claude CLI records every one), arms no deferred
// restart, starts no session, and leaves the recorded launch options alone.
// claude-tui has no live-update surface, so any reconcile there is a restart;
// it is kept off that path because no settings save reconciles it at all
// (TestSpawnOnlySettingsDoNotTriggerTheSweep) and the rebuilt config is equal
// (above).
func TestPromptOverrideChangeLeavesALiveSessionAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		providerName, model, key string
	}{
		{string(provider.Claude), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.Codex), "gpt-5.4", "codexPromptOverrides"},
	} {
		for _, change := range promptOverrideTransitions(tc.model) {
			t.Run(tc.providerName+"/"+change.name, func(t *testing.T) {
				t.Parallel()
				app := newTestAppWithStore(t)
				id, workspace := seedPromptOverrideThread(t, app, "thread-live-"+tc.providerName+"-"+change.name, tc.providerName, tc.model)
				setPromptOverrides(t, app, tc.key, change.before...)
				launch := promptOverrideOptions(t, app, id)

				capture := ""
				if tc.providerName == string(provider.Claude) {
					capture = registerLiveClaudeCaptureSession(t, app, id, workspace, launch)
				} else {
					registerLiveCodexSession(t, app, id, launch)
				}
				app.startSessionFn = func(threadID string) error {
					return fmt.Errorf("unexpected restart of %s: a prompt override change must not restart a live session", threadID)
				}

				setPromptOverrides(t, app, tc.key, change.after...)
				if !app.liveApplySessionConfig(id, false) {
					t.Fatal("liveApplySessionConfig() = false: a prompt override change fell through to a restart")
				}
				app.reconcileSessionConfig(id, false)

				if app.sessionManager().runtime.PendingConfigReconnect(id) {
					t.Fatal("a prompt override change armed a deferred restart")
				}
				live, ok := app.sessionManager().get(id)
				if !ok {
					t.Fatal("the live session is gone")
				}
				if live.LaunchOptions.SystemPrompt != launch.SystemPrompt ||
					live.LaunchOptions.SystemPromptOverrideSource != launch.SystemPromptOverrideSource {
					t.Fatalf("live launch prompt = (%q, %q), want the launched (%q, %q)",
						live.LaunchOptions.SystemPrompt, live.LaunchOptions.SystemPromptOverrideSource,
						launch.SystemPrompt, launch.SystemPromptOverrideSource)
				}
				if capture != "" {
					if body, err := os.ReadFile(capture); err == nil && strings.Contains(string(body), `"control_request"`) {
						t.Fatalf("a prompt override change sent a control request: %s", body)
					}
				}
			})
		}
	}
}

// registerLiveClaudeCaptureSession spawns the control-request capturing fake
// Claude CLI for the thread, waits for its init so the session knows its
// version, and registers it as the live session with the given launch
// options. Returns the capture file path.
func registerLiveClaudeCaptureSession(t *testing.T, app *App, threadID, workspace string, launch provider.SessionOptions) string {
	t.Helper()
	binary, capture := writeClaudeControlCaptureCLI(t)
	initSeen := make(chan struct{})
	var once sync.Once
	cfg := claude.ConfigFromOptions(launch)
	cfg.Binary = binary
	cfg.WorkDir = workspace
	cfg.SystemPromptDir = t.TempDir()
	sess, err := claude.NewSession(context.Background(), threadID, cfg, func(evt provider.ProviderEvent) {
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
		Token:         "live-token-" + threadID,
		Claude:        sess,
		LaunchOptions: launch,
		Liveness:      newSessionLiveness(time.Now()),
	})
	return capture
}

// A feature-owned prompt (currently discussions) is untouched by the pin:
// buildSessionOptions has already written it, so an edit to it reaches the
// reconciler and converges by restart, on every provider. A pin that
// swallowed it would freeze a feature-owned prompt at its first value.
func TestReconcileSettingsOwnedAxesKeepsFeaturePromptsConverging(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ providerName, model, key string }{
		{string(provider.Claude), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.ClaudeTUI), "claude-opus-5", "claudePromptOverrides"},
		{string(provider.Codex), "gpt-5.4", "codexPromptOverrides"},
	} {
		t.Run(tc.providerName, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			id, _ := seedPromptOverrideThread(t, app, "thread-feature-prompt-"+tc.providerName, tc.providerName, tc.model)
			setPromptOverrides(t, app, tc.key, testPromptEntry{Enabled: true, Models: []string{tc.model}, Prompt: "settings override"})
			app.setThreadSystemPrompt(id, "deliberation prompt v1")
			launch := promptOverrideOptions(t, app, id)
			if launch.SystemPrompt != "deliberation prompt v1" || launch.SystemPromptOverrideSource != "" {
				t.Fatalf("launch prompt = (%q, %q), want the feature-owned prompt and no override",
					launch.SystemPrompt, launch.SystemPromptOverrideSource)
			}

			app.setThreadSystemPrompt(id, "deliberation prompt v2")
			opts := reconcileOptionsFor(t, app, id, launch)
			if opts.SystemPrompt != "deliberation prompt v2" {
				t.Fatalf("SystemPrompt = %q, want the edited feature prompt to survive the reconcile", opts.SystemPrompt)
			}
			if opts.SystemPromptOverrideSource != "" {
				t.Fatalf("SystemPromptOverrideSource = %q, want empty: no settings override is in play", opts.SystemPromptOverrideSource)
			}
		})
	}
}
