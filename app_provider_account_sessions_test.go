package main

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
	app.sessions["thread"] = session{
		token:               "old-process",
		credentialAccountID: "first",
	}
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
	app.sessions[thread.ID] = session{
		provider:             string(provider.Claude),
		token:                "same-process",
		credentialGeneration: 1,
		liveness:             newSessionLiveness(time.Now()),
	}

	if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		t.Fatalf("ensureProviderAccountReadyForSendLocked() error = %v", err)
	}
	got := app.sessions[thread.ID]
	if got.token != "same-process" {
		t.Fatalf("Claude session token changed: %q", got.token)
	}
	if got.credentialGeneration != app.providerAccounts.Generation(string(provider.Claude)) {
		t.Fatalf("credential generation = %d, want %d", got.credentialGeneration, app.providerAccounts.Generation(string(provider.Claude)))
	}
	if got.credentialAccountID != "second" {
		t.Fatalf("credential account = %q, want second", got.credentialAccountID)
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
	liveness.activeTurns.Store(1)
	app.sessions[thread.ID] = session{
		provider:             string(provider.Codex),
		token:                "old-process",
		credentialGeneration: 1,
		liveness:             liveness,
	}

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "active turn finishes") {
		t.Fatalf("error = %v, want active-turn pending message", err)
	}
	if got := app.sessions[thread.ID]; got.token != "old-process" || got.credentialGeneration != 1 {
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
	app.sessions[thread.ID] = session{
		provider:             string(provider.Codex),
		token:                "old-process",
		credentialGeneration: 1,
		liveness:             newSessionLiveness(time.Now()),
	}
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "background-tool", 0, 0)

	err := app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "background work finishes") {
		t.Fatalf("error = %v, want background-work pending message", err)
	}
	if got := app.sessions[thread.ID]; got.token != "old-process" || got.credentialGeneration != 1 {
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
	app.sessions[thread.ID] = session{
		provider:             string(provider.Codex),
		token:                "old-process",
		credentialGeneration: 1,
		credentialAccountID:  "first",
		liveness:             newSessionLiveness(time.Now()),
	}
	app.stopSessionFn = func(threadID string) error {
		app.sessionManager().take(threadID)
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		selection := app.captureProviderAccountSelection(string(provider.Codex))
		app.sessionManager().put(threadID, session{
			provider:             string(provider.Codex),
			token:                "new-process",
			credentialGeneration: selection.Generation,
			credentialAccountID:  selection.AccountID,
			liveness:             newSessionLiveness(time.Now()),
		})
		return nil
	}

	if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		t.Fatalf("ensureProviderAccountReadyForSendLocked() error = %v", err)
	}
	got := app.sessions[thread.ID]
	if got.token != "new-process" || got.credentialAccountID != "second" {
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
	app.sessions[thread.ID] = session{
		provider:             string(provider.Codex),
		token:                "old-process",
		credentialGeneration: 1,
		credentialAccountID:  "first",
		liveness:             newSessionLiveness(time.Now()),
	}
	app.reconnectingThreads[thread.ID] = true

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
	credentials, err := provideraccounts.NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app.providerAccounts = accounts
	app.providerCredentials = credentials
}
