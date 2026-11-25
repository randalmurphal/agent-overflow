package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func TestSessionRateLimitsStayAttributedToOriginalAccount(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.sessionManager().put("thread", session{
		Token:               "old-process",
		CredentialAccountID: "first",
	})
	meta, err := json.Marshal(provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits: []provider.RateLimitEntry{{
			LimitID: "codex", WindowMins: 300,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	event := app.providerLifecycleService().AttributeRateLimits(provider.ProviderEvent{
		Kind: provider.EventRateLimits,
		Meta: meta,
	}, "thread", "old-process")

	var got provider.RateLimitsSnapshot
	if err := json.Unmarshal(event.Meta, &got); err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "first" {
		t.Fatalf("AccountID = %q, want first", got.AccountID)
	}
}

func TestClaudeAccountSwitchAdoptsGenerationWithoutRestart(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("claude-account-generation")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Claude))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Claude),
		Token:                "same-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})

	if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		t.Fatalf("ensureProviderAccountReadyForSendLocked() error = %v", err)
	}
	got, _ := app.sessionManager().get(thread.ID)
	if got.Token != "same-process" {
		t.Fatalf("Claude session token changed: %q", got.Token)
	}
	if got.CredentialGeneration != app.providerAccounts.Generation(string(provider.Claude)) {
		t.Fatalf("credential generation = %d, want %d", got.CredentialGeneration, app.providerAccounts.Generation(string(provider.Claude)))
	}
	if got.CredentialAccountID != "second" {
		t.Fatalf("credential account = %q, want second", got.CredentialAccountID)
	}
}

func TestCodexAccountSwitchWaitsForActiveTurn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("codex-account-active-turn")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	liveness := newSessionLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             liveness,
	})

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "active turn finishes") {
		t.Fatalf("error = %v, want active-turn pending message", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("active Codex session was modified: %+v", got)
	}
}

func TestCodexAccountSwitchWaitsForBackgroundWork(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("codex-account-background")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "background-tool", 0, 0)

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "background work finishes") {
		t.Fatalf("error = %v, want background-work pending message", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("background Codex session was modified: %+v", got)
	}
}

// A whole turn that started a command can be read off the wire and still be
// queued for triage, where neither turn counter nor the background count
// shows it. The switch handles the queued events before it decides.
func TestCodexAccountSwitchHandlesQueuedEventsBeforeDeciding(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("codex-account-queued-events")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})

	// The worker is held on one event; behind it waits the turn whose
	// command only the router will hold.
	release := make(chan struct{})
	app.providerEvents.enqueue(thread.ID, provider.ProviderEvent{}, func(provider.ProviderEvent) { <-release })
	app.providerEvents.enqueue(thread.ID, provider.ProviderEvent{}, func(provider.ProviderEvent) {
		seedSettledCodexUnifiedExecTracker(t, app, thread.ID)
	})

	result := make(chan error, 1)
	go func() { result <- app.ensureProviderAccountReadyForSendLocked(thread) }()
	queue := app.providerEvents.lookup(thread.ID)
	deadline := time.Now().Add(10 * time.Second)
	for waiting := false; !waiting; {
		select {
		case err := <-result:
			close(release)
			t.Fatalf("the switch decided with events still queued: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the switch never waited for the queued events")
		}
		queue.mu.Lock()
		waiting = queue.changed != nil
		queue.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	close(release)

	if err := <-result; err == nil || !strings.Contains(err.Error(), "background work finishes") {
		t.Fatalf("error = %v, want background-work pending message", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("the Codex session was modified: %+v", got)
	}
}

// The session's turn counter follows the wire and can read zero while
// triage still has the turn open. The switch waits for the turn triage has
// open.
func TestCodexAccountSwitchWaitsForATurnTriageStillHasOpen(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("codex-account-triage-turn")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: thread.ID + ":turn", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "active turn finishes") {
		t.Fatalf("error = %v, want active-turn pending message", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("the Codex session was modified: %+v", got)
	}
}

// A running Codex command exists only in the router. The switch waits for
// it as it does for a stored launch.
func TestCodexAccountSwitchWaitsForWorkOnlyTheRouterHolds(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	thread := testThread("codex-account-router-only")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})
	seedSettledCodexUnifiedExecTracker(t, app, thread.ID)

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "background work finishes") {
		t.Fatalf("error = %v, want background-work pending message", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("background Codex session was modified: %+v", got)
	}
}

// A launch that left the live set keeps `status='running'`. It does not
// hold the switch: the idle session reconnects to the selected account.
func TestCodexAccountSwitchIgnoresLaunchesNoLongerLive(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("codex-account-inactive-launch")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		CredentialAccountID:  "first",
		Liveness:             newSessionLiveness(time.Now()),
	})
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "background-tool", 0, 0)
	if n, err := app.store.MarkLiveBackgroundToolCallsInactive(thread.ID, time.Now().UnixMilli()); err != nil || n != 1 {
		t.Fatalf("mark the launch inactive = %d, %v; want 1", n, err)
	}
	app.stopSessionFn = func(threadID string) error {
		app.sessionManager().take(threadID)
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		selection := app.captureProviderAccountSelection(string(provider.Codex))
		app.sessionManager().put(threadID, session{
			Provider:             string(provider.Codex),
			Token:                "new-process",
			CredentialGeneration: selection.Generation,
			CredentialAccountID:  selection.AccountID,
			Liveness:             newSessionLiveness(time.Now()),
		})
		return nil
	}

	if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		t.Fatalf("ensureProviderAccountReadyForSendLocked() error = %v", err)
	}
	if got, _ := app.sessionManager().get(thread.ID); got.Token != "new-process" || got.CredentialAccountID != "second" {
		t.Fatalf("reconnected session = %+v", got)
	}
}

func TestCodexAccountSwitchRejectsSendWhenReconnectGateIsOwned(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("codex-account-reconnect-gate")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(thread.ID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		CredentialAccountID:  "first",
		Liveness:             newSessionLiveness(time.Now()),
	})
	app.sessionManager().runtime.BeginReconnect(thread.ID)

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "did not switch") {
		t.Fatalf("error = %v, want explicit unswitched error", err)
	}
}

func TestCodexAccountSelectionTracksSelectedIdentityOnly(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	installTestProviderAccounts(t, app, string(provider.Codex))
	selection := app.captureProviderAccountSelection(string(provider.Codex))
	if selection.AccountID != "second" {
		t.Fatalf("selection = %+v, want second account", selection)
	}
}

// Reconnecting to apply a pending account switch while a multi-item flush batch
// is dispatching must not call stopSessionForRestart/restoreUnconfirmedQueue:
// doing so bumped dispatchGeneration, restored all queued messages (including
// the one being sent) to the composer draft, sent the first item anyway, and
// aborted the rest of the batch.
func TestFlushDispatchBatchSurvivesAccountSwitchReconnect(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	logPath := filepath.Join(t.TempDir(), "wire.log")
	binary := writeSharedPoolAgyBinary(t, logPath)
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}

	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Agy))

	// Start a real session first, then roll its stored CredentialGeneration
	// back to simulate an account switch that landed while the session lived.
	if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("startSessionTakingLock: %v", err)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })
	oldSess, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("expected live session after StartSession")
	}
	oldSess.CredentialGeneration = 1
	oldSess.CredentialAccountID = "first"
	app.sessionManager().put(thread.ID, oldSess)

	// Persist and enqueue 3 messages as one batch (as happens when a turn ends
	// while messages waited on a pending account switch).
	now := time.Now().UnixMilli()
	batch := make([]triage.QueuedFlushItem, 0, 3)
	for i, msg := range []string{"queued-one", "queued-two", "queued-three"} {
		id := "q-" + msg
		if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
			ID:         id,
			ThreadID:   thread.ID,
			SendID:     "send-" + msg,
			Message:    msg,
			Payload:    []byte(`{"sendId":"send-` + msg + `"}`),
			EnqueuedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("InsertFlushQueueItem(%s): %v", msg, err)
		}
		batch = append(batch, triage.QueuedFlushItem{
			ID:                 id,
			Message:            msg,
			Payload:            []byte(`{"sendId":"send-` + msg + `"}`),
			EnqueuedAt:         now + int64(i),
			DispatchGeneration: app.currentFlushDispatchGeneration(thread.ID),
		})
	}

	app.enqueueFlushDispatch(thread.ID, batch)
	if err := app.drainFlushDispatch(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("drainFlushDispatch: %v", err)
	}

	// Verify session reconnected to the new account generation.
	newSess, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("expected live session after account-switch reconnect")
	}
	if newSess.Token == oldSess.Token {
		t.Fatalf("session token did not change across account switch: %q", newSess.Token)
	}
	if newSess.CredentialAccountID != "second" {
		t.Fatalf("reconnected account = %q, want second", newSess.CredentialAccountID)
	}

	// Verify ALL 3 queued messages were dispatched on the wire (wait briefly
	// for the mock bash script's read loop to append the final line to logPath).
	var wire string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		wireBytes, err := os.ReadFile(logPath)
		if err == nil {
			wire = string(wireBytes)
			if strings.Contains(wire, "queued-one") && strings.Contains(wire, "queued-two") && strings.Contains(wire, "queued-three") {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, msg := range []string{"queued-one", "queued-two", "queued-three"} {
		if !strings.Contains(wire, msg) {
			t.Errorf("wire log missing dispatched queued message %q:\n%s", msg, wire)
		}
	}

	// Verify NOTHING was restored back to the composer draft.
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.TrimSpace(draft.Content) != "" {
		t.Fatalf("queued messages were unexpectedly restored to composer draft: %q", draft.Content)
	}
}

func installTestProviderAccounts(t *testing.T, app *App, providerName string) {
	t.Helper()
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID: "first", Provider: providerName,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID: "second", Provider: providerName,
	}); err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
}
