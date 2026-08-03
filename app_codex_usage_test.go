package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

func codexUsagePtr(v int64) *int64 { return &v }

// codexAccountUsageKey rebuilds the cache key GetCodexAccountUsage uses so a
// test can seed the cache instead of spawning a real app-server.
func codexAccountUsageKey(app *App) string {
	selection := app.captureProviderAccountSelection(string(provider.Codex))
	return app.providerBinaryPath(string(provider.Codex)) + "\x00" + selection.AccountID
}

func seedCodexAccountUsage(t *testing.T, app *App, usage codex.AccountUsage, err error) {
	t.Helper()
	_, seedErr := app.codexAccountUsage().Get(
		context.Background(),
		codexAccountUsageKey(app),
		func(context.Context) (codex.AccountUsage, error) { return usage, err },
	)
	if err == nil && seedErr != nil {
		t.Fatalf("seed cache: %v", seedErr)
	}
}

func TestGetCodexAccountUsageProjectsTheReport(t *testing.T) {
	app := newTestAppWithStore(t)
	seedCodexAccountUsage(t, app, codex.AccountUsage{
		LifetimeTokens:    codexUsagePtr(11776335004),
		CurrentStreakDays: codexUsagePtr(8),
		DailyBuckets: []codex.AccountUsageDailyBucket{
			{StartDate: "2026-08-01", Tokens: 725458670},
		},
	}, nil)

	usage, err := app.GetCodexAccountUsage()
	if err != nil {
		t.Fatalf("GetCodexAccountUsage: %v", err)
	}
	if usage == nil {
		t.Fatal("want a report")
	}
	if usage.LifetimeTokens == nil || *usage.LifetimeTokens != 11776335004 {
		t.Errorf("lifetime tokens = %v", usage.LifetimeTokens)
	}
	if usage.PeakDailyTokens != nil {
		t.Errorf("an unreported field must stay absent, got %v", *usage.PeakDailyTokens)
	}
	if len(usage.DailyBuckets) != 1 || usage.DailyBuckets[0].StartDate != "2026-08-01" {
		t.Errorf("buckets = %+v", usage.DailyBuckets)
	}
}

// TestGetCodexAccountUsageAbsenceIsNotAnError pins the state-vs-failure
// split: "this codex/account has nothing to report" renders no section and
// is not a failure, while a real failure stays visible.
func TestGetCodexAccountUsageAbsenceIsNotAnError(t *testing.T) {
	t.Run("unavailable is nil, nil", func(t *testing.T) {
		app := newTestAppWithStore(t)
		seedCodexAccountUsage(t, app, codex.AccountUsage{},
			fmt.Errorf("%w: this codex build has no account/usage/read", codex.ErrAccountUsageUnavailable))

		usage, err := app.GetCodexAccountUsage()
		if err != nil {
			t.Fatalf("an unavailable report must not surface as an error: %v", err)
		}
		if usage != nil {
			t.Fatalf("want no report, got %+v", usage)
		}
	})

	t.Run("an empty report is also absence", func(t *testing.T) {
		app := newTestAppWithStore(t)
		seedCodexAccountUsage(t, app, codex.AccountUsage{}, nil)

		usage, err := app.GetCodexAccountUsage()
		if err != nil {
			t.Fatalf("GetCodexAccountUsage: %v", err)
		}
		if usage != nil {
			t.Fatalf("an empty profile must not render as zeros, got %+v", usage)
		}
	})

	t.Run("a real failure surfaces", func(t *testing.T) {
		app := newTestAppWithStore(t)
		wantErr := errors.New("codex account usage: spawn: no such file")
		seedCodexAccountUsage(t, app, codex.AccountUsage{}, wantErr)

		if _, err := app.GetCodexAccountUsage(); err == nil {
			t.Fatal("a transport failure must not be reported as absence")
		}
	})
}

// TestThreadTurnInFlightTransitions covers the apply-when-idle gate as
// transitions rather than states: the same thread must read idle, then busy,
// then idle again as its turn opens and closes through both signals the gate
// consults.
func TestThreadTurnInFlightTransitions(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-settings-push-gate")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if app.threadTurnInFlight(thread.ID) {
		t.Fatal("a thread with no session and no turn must read idle")
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "tok",
		liveness: newSessionLiveness(time.Now()),
	}
	if app.threadTurnInFlight(thread.ID) {
		t.Fatal("a live session with no turn must read idle")
	}

	// The session-liveness counter alone is enough to block a push.
	app.sessions[thread.ID].liveness.activeTurns.Add(1)
	if !app.threadTurnInFlight(thread.ID) {
		t.Fatal("an active turn on the session must read busy")
	}
	decrementActiveTurnsClamped(&app.sessions[thread.ID].liveness.activeTurns)
	if app.threadTurnInFlight(thread.ID) {
		t.Fatal("the gate must reopen once the turn count drains")
	}

	// So is triage's own view, which opens before the counter on some paths.
	handle := func(event provider.ProviderEvent) {
		t.Helper()
		event.ThreadID = thread.ID
		event.TurnID = "turn-1"
		event.Timestamp = time.Now()
		if err := app.triage.Handle(event); err != nil {
			t.Fatalf("triage handle %v: %v", event.Kind, err)
		}
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
	if !app.threadTurnInFlight(thread.ID) {
		t.Fatal("a turn triage knows about must read busy")
	}
	handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	if app.threadTurnInFlight(thread.ID) {
		t.Fatal("the gate must reopen after the turn completes")
	}
}

// TestPushCodexThreadSettingsIgnoresEmptyWork guards the two cheap
// short-circuits ahead of any wire work.
func TestPushCodexThreadSettingsIgnoresEmptyWork(t *testing.T) {
	app := newTestAppWithStore(t)
	// A nil session and an empty plan must both be no-ops rather than panics;
	// liveApplySessionConfig reaches here for every Codex config change,
	// including ones that touch no pushable axis.
	app.pushCodexThreadSettings("thread-x", nil, codex.ThreadSettingsPush{Model: true})
	app.pushCodexThreadSettings("thread-x", nil, codex.ThreadSettingsPush{})
}
