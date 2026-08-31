package app

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

// The thinking axis is the settings-owned axis that CONVERGES on a live
// session, which makes it the odd one out in the spawn/reconcile pair:
// DisabledTools and DisableTodoReminders pin to what the session launched
// with, and this one is re-read on both paths.
//
// These tests exercise the pair through the same entry points
// liveApplySessionConfig uses, so a change that resolved on one side only
// would show up here as either a lost setting or a restart nobody asked for.

func setClaudeThinking(t *testing.T, app *App, thinking map[string]any) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"claudeThinking": thinking}); err != nil {
		t.Fatalf("Update(claudeThinking) error = %v", err)
	}
}

// The spawn half: the stored preference reaches SessionOptions, and from
// there the Claude launch config.
func TestApplySettingsOwnedAxesStampsClaudeThinking(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-thinking-spawn", string(provider.Claude), "claude-opus-5")
	setClaudeThinking(t, app, map[string]any{"mode": "budget", "budgetTokens": 2048, "display": "omitted"})

	opts := promptOverrideOptions(t, app, id)
	want := provider.ClaudeThinking{Mode: "budget", BudgetTokens: 2048, Display: "omitted"}
	if opts.ClaudeThinking != want {
		t.Fatalf("ClaudeThinking = %+v, want %+v", opts.ClaudeThinking, want)
	}
	cfg := claude.ConfigFromOptions(opts)
	if cfg.Thinking.Mode != claude.ThinkingBudget || cfg.Thinking.BudgetTokens != 2048 ||
		cfg.Thinking.Display != claude.ThinkingDisplayOmitted {
		t.Fatalf("Config.Thinking = %+v, want the budget axis", cfg.Thinking)
	}
}

// Codex and claude-tui must read the zero value on BOTH paths. A provider
// that picked the setting up on one side only would diff against itself and
// queue a restart for a setting it can never receive.
func TestClaudeThinkingIsHeadlessClaudeOnlyOnBothPaths(t *testing.T) {
	for _, providerName := range []string{string(provider.Codex), string(provider.ClaudeTUI)} {
		app := newTestAppWithStore(t)
		model := "gpt-5.6-codex"
		if providerName == string(provider.ClaudeTUI) {
			model = "claude-opus-5"
		}
		id, _ := seedPromptOverrideThread(t, app, "thread-thinking-"+providerName, providerName, model)
		setClaudeThinking(t, app, map[string]any{"mode": "off"})

		opts := promptOverrideOptions(t, app, id)
		if opts.ClaudeThinking != (provider.ClaudeThinking{}) {
			t.Fatalf("%s spawn ClaudeThinking = %+v, want zero", providerName, opts.ClaudeThinking)
		}
		reconciled := reconcileOptionsFor(t, app, id, opts)
		if reconciled.ClaudeThinking != (provider.ClaudeThinking{}) {
			t.Fatalf("%s reconcile ClaudeThinking = %+v, want zero", providerName, reconciled.ClaudeThinking)
		}
	}
}

// The reconcile half, end to end through PlanLiveUpdate — the classification
// the whole feature rests on.
func TestReconcileSettingsOwnedAxesConvergesClaudeThinking(t *testing.T) {
	tests := []struct {
		name        string
		launch      map[string]any
		next        map[string]any
		wantLive    bool
		wantRequest claude.ThinkingUpdate
	}{
		{
			name:     "default to budget applies live",
			launch:   map[string]any{},
			next:     map[string]any{"mode": "budget", "budgetTokens": 2048},
			wantLive: true,
			wantRequest: claude.ThinkingUpdate{
				Apply: true, SendBudget: true, Budget: 2048, Display: claude.ThinkingDisplaySummarized,
			},
		},
		{
			name:        "budget to off applies live",
			launch:      map[string]any{"mode": "budget", "budgetTokens": 2048},
			next:        map[string]any{"mode": "off"},
			wantLive:    true,
			wantRequest: claude.ThinkingUpdate{Apply: true, SendBudget: true, Budget: 0},
		},
		{
			name:     "off to budget applies live",
			launch:   map[string]any{"mode": "off"},
			next:     map[string]any{"mode": "budget", "budgetTokens": 4096, "display": "omitted"},
			wantLive: true,
			wantRequest: claude.ThinkingUpdate{
				Apply: true, SendBudget: true, Budget: 4096, Display: claude.ThinkingDisplayOmitted,
			},
		},
		{
			name:        "display only applies live",
			launch:      map[string]any{},
			next:        map[string]any{"display": "omitted"},
			wantLive:    true,
			wantRequest: claude.ThinkingUpdate{Apply: true, Display: claude.ThinkingDisplayOmitted},
		},
		{
			// The one direction with no wire form. Documented as a
			// deferred restart, exactly like turning a prompt override off.
			name:     "budget back to default needs a restart",
			launch:   map[string]any{"mode": "budget", "budgetTokens": 2048},
			next:     map[string]any{},
			wantLive: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			id, _ := seedPromptOverrideThread(t, app, "thread-thinking-reconcile", string(provider.Claude), "claude-opus-5")

			setClaudeThinking(t, app, tc.launch)
			launch := promptOverrideOptions(t, app, id)

			// The user changes the setting while the session is live.
			setClaudeThinking(t, app, tc.next)
			opts := reconcileOptionsFor(t, app, id, launch)

			update, ok := claude.PlanLiveUpdate(launch, opts)
			if ok != tc.wantLive {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v (update %+v)", ok, tc.wantLive, update)
			}
			if !tc.wantLive {
				return
			}
			if update.Thinking != tc.wantRequest {
				t.Fatalf("update.Thinking = %+v, want %+v", update.Thinking, tc.wantRequest)
			}
		})
	}
}

// The hot path: the reconciler runs on every model / effort / runtime-mode
// change, and an unchanged setting must plan nothing. Resolving rather than
// pinning is only safe because "unchanged" is a struct comparison.
func TestReconcileSettingsOwnedAxesPlansNothingForUnchangedThinking(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-thinking-stable", string(provider.Claude), "claude-opus-5")
	setClaudeThinking(t, app, map[string]any{"mode": "budget", "budgetTokens": 2048})

	launch := promptOverrideOptions(t, app, id)
	opts := reconcileOptionsFor(t, app, id, launch)

	update, ok := claude.PlanLiveUpdate(launch, opts)
	if !ok || !update.Empty() {
		t.Fatalf("PlanLiveUpdate = (%+v, %v), want an empty live update", update, ok)
	}
}

// The settings→provider conversion is the one place the two structurally
// identical types meet (internal/settings may not import internal/provider),
// so a field added to one and forgotten here would be a silently dropped
// setting rather than a compile error.
func TestClaudeThinkingOptionCarriesEveryField(t *testing.T) {
	stored := settings.ClaudeThinking{
		Mode:         settings.ClaudeThinkingModeBudget,
		BudgetTokens: 12345,
		Display:      settings.ClaudeThinkingDisplayOmitted,
	}
	got := claudeThinkingOption(stored)
	want := provider.ClaudeThinking{Mode: "budget", BudgetTokens: 12345, Display: "omitted"}
	if got != want {
		t.Fatalf("claudeThinkingOption(%+v) = %+v, want %+v", stored, got, want)
	}
}

// A settings save is the ONLY trigger this axis has — nothing else
// reconciles on one — so the fan-out has to survive being called with no
// live sessions at all rather than depending on a session being registered.
func TestReconcileLiveClaudeSessionsIsSafeWithNoSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	app.reconcileLiveClaudeSessions()
}
