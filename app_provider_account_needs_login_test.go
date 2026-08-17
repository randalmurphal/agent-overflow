package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// Metadata can outlive its credential. Such an account stays listed — it is
// still the user's record of that login and its usage history — but it must
// say it cannot be selected instead of looking like a working choice.
func TestListProviderAccountsFlagsAnAccountWhoseCredentialIsGone(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Codex)
	installRemovalTestAccounts(t, app, providerName, "gone", "present")
	if err := app.providerCredentials.RemoveAccount(providerName, "gone"); err != nil {
		t.Fatal(err)
	}

	accounts, err := app.ListProviderAccounts()
	if err != nil {
		t.Fatalf("ListProviderAccounts() error = %v", err)
	}
	byID := map[string]ManagedProviderAccount{}
	for _, account := range accounts {
		byID[account.ID] = account
	}
	if got, ok := byID["gone"]; !ok || !got.NeedsLogin {
		t.Fatalf("slotless account = %+v, ok=%v, want needsLogin", got, ok)
	}
	if got, ok := byID["present"]; !ok || got.NeedsLogin {
		t.Fatalf("active account = %+v, ok=%v, want usable", got, ok)
	}
}

// The active account is backed by the canonical store, not a saved slot, so
// its usability is a separate question from every other card's.
func TestListProviderAccountsFlagsTheActiveAccountWhenTheCanonicalCredentialIsGone(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Codex)
	installRemovalTestAccounts(t, app, providerName, "inactive", "active")
	if err := app.providerCredentials.RemoveActive(providerName); err != nil {
		t.Fatal(err)
	}

	accounts, err := app.ListProviderAccounts()
	if err != nil {
		t.Fatalf("ListProviderAccounts() error = %v", err)
	}
	for _, account := range accounts {
		wantNeedsLogin := account.ID == "active"
		if account.NeedsLogin != wantNeedsLogin {
			t.Fatalf(
				"account %q needsLogin = %v, want %v",
				account.ID,
				account.NeedsLogin,
				wantNeedsLogin,
			)
		}
	}
}

// Selecting such an account cannot work. It must say what to do about it
// rather than surface the missing path.
func TestSwitchToAnAccountWhoseCredentialIsGoneAsksForALogin(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Codex)
	installRemovalTestAccounts(t, app, providerName, "gone", "present")
	if err := app.providerCredentials.RemoveAccount(providerName, "gone"); err != nil {
		t.Fatal(err)
	}

	_, err := app.SwitchProviderAccount(providerName, "gone")
	if err == nil {
		t.Fatal("SwitchProviderAccount() selected an account with no credential")
	}
	if !strings.Contains(err.Error(), "sign in") {
		t.Fatalf("error = %v, want a sign-in-again instruction", err)
	}
	if !strings.Contains(err.Error(), "gone@example.com") {
		t.Fatalf("error = %v, want the account named", err)
	}

	active, ok := app.providerAccounts.Active(providerName, time.Now())
	if !ok || active.ID != "present" {
		t.Fatalf("active account = %+v, ok=%v, want the failed switch to change nothing", active, ok)
	}
	credential, err := app.providerCredentials.ReadCredential(providerName, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential) != `{"account":"present"}` {
		t.Fatalf("canonical credential = %s, want the previous account untouched", credential)
	}
}

// A husked slot is the other shape of "gone": the file is there, but the
// provider ended that login and only a fresh sign-in replaces it. Reporting it
// as usable is what let a card advertise a switch that could only destroy the
// working account it replaced.
func TestListProviderAccountsFlagsAHuskedSlotAsNeedingLogin(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Claude)
	installRemovalTestAccounts(t, app, providerName, "husked", "present")
	writeSlotCredentialDirectly(
		t,
		app,
		"husked",
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	)

	accounts, err := app.ListProviderAccounts()
	if err != nil {
		t.Fatalf("ListProviderAccounts() error = %v", err)
	}
	byID := map[string]ManagedProviderAccount{}
	for _, account := range accounts {
		byID[account.ID] = account
	}
	if got, ok := byID["husked"]; !ok || !got.NeedsLogin {
		t.Fatalf("husked account = %+v, ok=%v, want needsLogin", got, ok)
	}
	if got, ok := byID["present"]; !ok || got.NeedsLogin {
		t.Fatalf("active account = %+v, ok=%v, want usable", got, ok)
	}
}

// The active account is the one card backed by the canonical store, which is
// exactly where the CLI writes its husk when a refresh comes back with
// invalid_grant. Its file is present and non-empty, so a presence check calls
// that account healthy — and the card then offers "refresh usage" and
// "continue this session" for a login that ended, while the only recovery
// (sign in again) goes unmentioned.
func TestListProviderAccountsFlagsTheActiveAccountWhenTheCanonicalCredentialIsHusked(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Claude)
	installRemovalTestAccounts(t, app, providerName, "inactive", "active")
	// Written the way the CLI writes it — Agent Overflow's own canonical write
	// refuses these bytes.
	if err := app.providerCredentials.WriteNativeCredentialForTest(
		providerName,
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"subscriptionType":"Claude Max"}}`),
	); err != nil {
		t.Fatal(err)
	}

	accounts, err := app.ListProviderAccounts()
	if err != nil {
		t.Fatalf("ListProviderAccounts() error = %v", err)
	}
	for _, account := range accounts {
		wantNeedsLogin := account.ID == "active"
		if account.NeedsLogin != wantNeedsLogin {
			t.Fatalf(
				"account %q needsLogin = %v, want %v",
				account.ID,
				account.NeedsLogin,
				wantNeedsLogin,
			)
		}
	}
}

// Installing a husk into the canonical store would retire the working login
// for a dead one and leave the app looking signed in until the next request
// failed. The switch refuses and records the finding.
func TestSwitchToAHuskedAccountIsRefusedAndAudited(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Claude)
	installRemovalTestAccounts(t, app, providerName, "husked", "present")
	writeSlotCredentialDirectly(
		t,
		app,
		"husked",
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	)
	auditPath := filepath.Join(t.TempDir(), "account-audit.log")
	app.accountAuditPath = auditPath

	_, err := app.SwitchProviderAccount(providerName, "husked")
	if err == nil {
		t.Fatal("SwitchProviderAccount() installed a sign-out husk")
	}
	if !strings.Contains(err.Error(), "sign in") || !strings.Contains(err.Error(), "husked@example.com") {
		t.Fatalf("error = %v, want a named sign-in-again instruction", err)
	}
	credential, err := app.providerCredentials.ReadCredential(providerName, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential) != `{"account":"present"}` {
		t.Fatalf("canonical credential = %s, want the previous account untouched", credential)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(audit), "husked") {
		t.Fatalf("audit log = %q, want the refused account recorded", audit)
	}
}
