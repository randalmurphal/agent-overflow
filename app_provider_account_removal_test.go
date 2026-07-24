package main

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func TestRemoveActiveProviderAccountSelectsNextWithoutChangingRunningCodexSession(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Codex), "first", "second")
	app.sessions["thread"] = session{
		provider:             string(provider.Codex),
		credentialAccountID:  "second",
		credentialGeneration: app.providerAccounts.Generation(string(provider.Codex)),
		credentialAccount: provider.AccountInfo{
			Email: "second@example.com",
		},
	}
	app.rateLimitsByProvider = map[string]provider.RateLimitsSnapshot{
		rateLimitsCacheKey(string(provider.Codex), "second"): {
			Provider: string(provider.Codex), AccountID: "second",
		},
	}

	if err := app.RemoveProviderAccount(string(provider.Codex), "second"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	active, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || active.ID != "first" {
		t.Fatalf("active account = %+v, ok=%v, want first", active, ok)
	}
	got, err := app.providerCredentials.ReadCredential(string(provider.Codex), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"account":"first"}` {
		t.Fatalf("active credential = %s, want first", got)
	}
	if got, err := app.providerCredentials.ReadCredential(
		string(provider.Codex),
		"second",
		false,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("removed saved credential = %s, %v, want missing", got, err)
	}
	if _, exists := app.rateLimitsByProvider[rateLimitsCacheKey(string(provider.Codex), "second")]; exists {
		t.Fatal("removed account rate-limit cache still exists")
	}
	if got := app.sessions["thread"].credentialAccountID; got != "second" {
		t.Fatalf("running Codex session account = %q, want cached second", got)
	}
	if event := app.providerSessionAccount("thread"); event == nil ||
		event.Account.Email != "second@example.com" {
		t.Fatalf("running Codex account event = %+v, want retained identity", event)
	}

}

func TestRemoveFinalProviderAccountSignsOutAndClearsClaudeSessionAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "only")
	app.sessions["thread"] = session{
		provider:             string(provider.Claude),
		credentialAccountID:  "only",
		credentialGeneration: app.providerAccounts.Generation(string(provider.Claude)),
	}
	var accountEvent ProviderAccountEvent
	var sessionEvent ProviderSessionAccountEvent
	app.testEmitHook = func(name string, data any) {
		switch name {
		case "provider:account":
			accountEvent, _ = data.(ProviderAccountEvent)
		case "provider:session_account":
			sessionEvent, _ = data.(ProviderSessionAccountEvent)
		}
	}

	if err := app.RemoveProviderAccount(string(provider.Claude), "only"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	if accounts := app.providerAccounts.List(string(provider.Claude), time.Now()); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
	if _, err := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"",
		true,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("active credential error = %v, want credential missing", err)
	}
	if _, err := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"only",
		false,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("saved credential error = %v, want credential missing", err)
	}
	if !accountEvent.Cleared || accountEvent.Provider != string(provider.Claude) {
		t.Fatalf("provider account event = %+v, want cleared Claude event", accountEvent)
	}
	if sessionEvent.Connected || sessionEvent.ThreadID != "thread" {
		t.Fatalf("provider session event = %+v, want disconnected thread", sessionEvent)
	}
	if got := app.sessions["thread"].credentialAccountID; got != "" {
		t.Fatalf("running Claude session account = %q, want cleared", got)
	}
}

func TestRemoveInactiveProviderAccountLeavesActiveCredentialUntouched(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "first", "second")
	fingerprint := app.providerCredentialFingerprints[string(provider.Claude)]

	if err := app.RemoveProviderAccount(string(provider.Claude), "first"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	active, ok := app.providerAccounts.Active(string(provider.Claude), time.Now())
	if !ok || active.ID != "second" {
		t.Fatalf("active account = %+v, ok=%v, want second", active, ok)
	}
	got, err := app.providerCredentials.ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"account":"second"}` {
		t.Fatalf("active credential = %s, want second", got)
	}
	if got := app.providerCredentialFingerprints[string(provider.Claude)]; got != fingerprint {
		t.Fatal("inactive removal replaced the known canonical credential fingerprint")
	}
}

func installRemovalTestAccounts(
	t *testing.T,
	app *App,
	providerName string,
	accountIDs ...string,
) {
	t.Helper()
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range accountIDs {
		if err := credentials.WriteAccountCredential(
			providerName,
			accountID,
			[]byte(`{"account":"`+accountID+`"}`),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
			ID:       accountID,
			Provider: providerName,
			Email:    accountID + "@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(accountIDs) == 0 {
		t.Fatal("installRemovalTestAccounts requires at least one account")
	}
	if err := credentials.Activate(
		providerName,
		"",
		accountIDs[len(accountIDs)-1],
	); err != nil {
		t.Fatal(err)
	}
	app.providerAccounts = accounts
	app.providerCredentials = credentials
	app.providerAccountMu.Lock()
	app.rememberProviderCredentialFingerprintLocked(
		providerName,
		[]byte(`{"account":"`+accountIDs[len(accountIDs)-1]+`"}`),
	)
	app.providerAccountMu.Unlock()
}
