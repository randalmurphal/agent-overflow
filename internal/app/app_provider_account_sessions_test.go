package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func TestSessionRateLimitsStayAttributedToOriginalAccount(t *testing.T) {
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
	event := app.attributeSessionRateLimits(provider.ProviderEvent{
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

func TestCodexIdleAccountSwitchReconnectsToSelectedAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("codex-account-idle")
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
	got, _ := app.sessionManager().get(thread.ID)
	if got.Token != "new-process" || got.CredentialAccountID != "second" {
		t.Fatalf("reconnected session = %+v", got)
	}
}

func TestCodexAccountSwitchRejectsSendWhenReconnectGateIsOwned(t *testing.T) {
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
	app := newTestAppWithStore(t)
	installTestProviderAccounts(t, app, string(provider.Codex))
	selection := app.captureProviderAccountSelection(string(provider.Codex))
	if selection.AccountID != "second" {
		t.Fatalf("selection = %+v, want second account", selection)
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
