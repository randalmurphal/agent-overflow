package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// Metadata can outlive its credential. Such an account stays listed — it is
// still the user's record of that login and its usage history — but it must
// say it cannot be selected instead of looking like a working choice.
func TestListProviderAccountsFlagsAnAccountWhoseCredentialIsGone(t *testing.T) {
	app := newTestAppWithStore(t)
	providerName := string(provider.Codex)
	installRemovalTestAccounts(t, app, providerName, "gone", "present")
	if err := providerCredentialsForTest(t, app).RemoveAccount(providerName, "gone"); err != nil {
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
	if err := providerCredentialsForTest(t, app).RemoveActive(providerName); err != nil {
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
	if err := providerCredentialsForTest(t, app).RemoveAccount(providerName, "gone"); err != nil {
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
	credential, err := providerCredentialsForTest(t, app).ReadCredential(providerName, "", true)
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
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
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
	app.providerAccounts.SetAuditPathForTest(auditPath)

	_, err := app.SwitchProviderAccount(providerName, "husked")
	if err == nil {
		t.Fatal("SwitchProviderAccount() installed a sign-out husk")
	}
	if !strings.Contains(err.Error(), "sign in") || !strings.Contains(err.Error(), "husked@example.com") {
		t.Fatalf("error = %v, want a named sign-in-again instruction", err)
	}
	credential, err := providerCredentialsForTest(t, app).ReadCredential(providerName, "", true)
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

func TestRemoveActiveProviderAccountSelectsNextWithoutChangingRunningCodexSession(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Codex), "first", "second")
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Codex),
		CredentialAccountID:  "second",
		CredentialGeneration: app.providerAccounts.Generation(string(provider.Codex)),
		CredentialAccount: provider.AccountInfo{
			Email: "second@example.com",
		},
	})
	app.providerLifecycleService().RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{
		Action: "rate_limits",
		RateLimits: &provider.RateLimitsSnapshot{
			Provider: string(provider.Codex), AccountID: "second",
			Limits: []provider.RateLimitEntry{{LimitID: "session", WindowMins: 300}},
		},
	})

	if err := app.RemoveProviderAccount(string(provider.Codex), "second"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	active, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || active.ID != "first" {
		t.Fatalf("active account = %+v, ok=%v, want first", active, ok)
	}
	got, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Codex), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"account":"first"}` {
		t.Fatalf("active credential = %s, want first", got)
	}
	if got, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Codex),
		"second",
		false,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("removed saved credential = %s, %v, want missing", got, err)
	}
	if hasRateLimitsSnapshot(app, string(provider.Codex), "second") {
		t.Fatal("removed account rate-limit cache still exists")
	}
	entry, _ := app.sessionManager().get("thread")
	if got := entry.CredentialAccountID; got != "second" {
		t.Fatalf("running Codex session account = %q, want cached second", got)
	}
	if event := app.providerSessionAccount("thread"); event == nil ||
		event.Account.Email != "second@example.com" {
		t.Fatalf("running Codex account event = %+v, want retained identity", event)
	}
}

func hasRateLimitsSnapshot(app *App, providerName, accountID string) bool {
	for _, snapshot := range app.GetRateLimitsSnapshots() {
		if snapshot.Provider == providerName && snapshot.AccountID == accountID {
			return true
		}
	}
	return false
}

func TestRemoveFinalProviderAccountSignsOutAndClearsClaudeSessionAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "only")
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Claude),
		CredentialAccountID:  "only",
		CredentialGeneration: app.providerAccounts.Generation(string(provider.Claude)),
	})
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
	if _, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"",
		true,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("active credential error = %v, want credential missing", err)
	}
	if _, err := providerCredentialsForTest(t, app).ReadCredential(
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
	entry, _ := app.sessionManager().get("thread")
	if got := entry.CredentialAccountID; got != "" {
		t.Fatalf("running Claude session account = %q, want cleared", got)
	}
}

func TestRemoveInactiveProviderAccountLeavesActiveCredentialUntouched(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "first", "second")
	fingerprint, _ := app.providerAccounts.CredentialFingerprintForTest(string(provider.Claude))

	if err := app.RemoveProviderAccount(string(provider.Claude), "first"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	active, ok := app.providerAccounts.Active(string(provider.Claude), time.Now())
	if !ok || active.ID != "second" {
		t.Fatalf("active account = %+v, ok=%v, want second", active, ok)
	}
	got, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"account":"second"}` {
		t.Fatalf("active credential = %s, want second", got)
	}
	if got, _ := app.providerAccounts.CredentialFingerprintForTest(string(provider.Claude)); got != fingerprint {
		t.Fatal("inactive removal replaced the known canonical credential fingerprint")
	}
}

// The CLI blanks the canonical credential when a dead account's refresh
// fails. Removing that account must read the husk as a sign-out and proceed:
// refusing was the "cannot even delete the bricked account" half of the
// 2026-08-03 lockout.
func TestRemoveActiveAccountSucceedsAfterProviderBlanksCanonicalCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "first", "second")
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	); err != nil {
		t.Fatal(err)
	}

	if err := app.RemoveProviderAccount(string(provider.Claude), "second"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	active, ok := app.providerAccounts.Active(string(provider.Claude), time.Now())
	if !ok || active.ID != "first" {
		t.Fatalf("active account = %+v, ok=%v, want first", active, ok)
	}
	canonical, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"account":"first"}` {
		t.Fatalf("canonical credential = %s, want the replacement installed over the husk", canonical)
	}
	if got, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"second",
		false,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("removed saved credential = %s, %v, want missing", got, err)
	}
}

// Removing the final account promises a cleared canonical credential; a
// leftover husk is still a file claiming a login exists.
func TestRemoveFinalAccountClearsBlankedCanonicalCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "only")
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	); err != nil {
		t.Fatal(err)
	}

	if err := app.RemoveProviderAccount(string(provider.Claude), "only"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}

	if _, ok := app.providerAccounts.Active(string(provider.Claude), time.Now()); ok {
		t.Fatal("an account is still active after removing the final one")
	}
	if got, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"",
		true,
	); !provideraccounts.IsCredentialMissing(err) {
		t.Fatalf("canonical credential = %s, %v, want cleared", got, err)
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
	credentials := newTestProviderCredentials(t, t.TempDir())
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
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
	app.providerAccounts.BlessCredentialForTest(
		providerName,
		[]byte(`{"account":"`+accountIDs[len(accountIDs)-1]+`"}`),
	)
}
