package app

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
)

func installIdentityTestAccount(
	t *testing.T,
	app *App,
	providerName string,
	accountID string,
	email string,
	credential []byte,
) {
	t.Helper()
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	credentials := newTestProviderCredentials(t, home)
	if err := credentials.WriteAccountCredential(providerName, accountID, credential); err != nil {
		t.Fatal(err)
	}
	// Through the native-store hook, not a direct file write, so the
	// fixture doesn't encode the platform's canonical-store layout.
	if err := credentials.WriteNativeCredentialForTest(providerName, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID:       accountID,
		Provider: providerName,
		Email:    email,
	}); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
	app.providerAccounts.BlessCredentialForTest(providerName, credential)
}

func writeCodexIdentityProbeBinary(t *testing.T, email string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock-codex")
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		"read -r _ || true\n" +
		"read -r _ || true\n" +
		"printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"v2\"}}'\n" +
		"printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"account\":{\"type\":\"chatgpt\",\"email\":\"" + email + "\",\"planType\":\"pro\"},\"requiresOpenaiAuth\":true}}'\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeRotatingProbeMockBinary answers the identity probe and rewrites the
// canonical credential the first time it runs — what Claude Code does when its
// startup finds an expired access token.
func writeRotatingProbeMockBinary(
	t *testing.T,
	credentialPath string,
	rotated string,
	accountJSON string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mock-claude")
	marker := filepath.Join(dir, "rotated")
	response := `{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"ao-probe-init","response":{"account":` + accountJSON + `}}}`
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		"if [ ! -f " + shellQuote(marker) + " ]; then\n" +
		"  printf '%s' '" + rotated + "' > " + shellQuote(credentialPath) + "\n" +
		"  : > " + shellQuote(marker) + "\n" +
		"fi\n" +
		"printf '%s\\n' '" + response + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A token rotation during the identity probe is the provider doing its job, not
// an account switch. Reconciliation has to absorb it and adopt the value the
// probe left behind: this runs on the send path, so failing here turns an
// expired access token into a failed send.
func TestReconciliationAbsorbsARotationDuringTheIdentityProbe(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"first"}}`),
	)

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	const rotated = `{"claudeAiOauth":{"accessToken":"rotated"}}`
	binary := writeRotatingProbeMockBinary(
		t,
		activePath,
		rotated,
		`{"email":"first@example.com","subscriptionType":"Claude Max",`+
			`"tokenSource":"oauth","apiProvider":"firstParty"}`,
	)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	// An unrecognized canonical value is what sends reconciliation to the probe
	// in the first place.
	if err := os.WriteFile(
		activePath,
		[]byte(`{"claudeAiOauth":{"accessToken":"external"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := app.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	saved, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != rotated {
		t.Fatalf("saved credential = %s, want the rotated value", saved)
	}
	fingerprint, _ := app.providerAccounts.CredentialFingerprintForTest(string(provider.Claude))
	if fingerprint != sha256.Sum256([]byte(rotated)) {
		t.Fatal("fingerprint was not advanced to the value the probe left behind")
	}
}

// claude >= 2.1.219 blanks the canonical credential in place when its startup
// token refresh fails. Reconciliation must read that husk as a sign-out and
// leave the saved slot alone — adopting it is the write that destroyed a saved
// login on 2026-08-03.
func TestReconciliationRefusesToAdoptABlankedClaudeCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	good := []byte(`{"claudeAiOauth":{"accessToken":"good","refreshToken":"good-refresh"}}`)
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		good,
	)
	// A husk must never reach the identity probe: spawning the CLI against a
	// destroyed login is a wasted process that answers tokenSource:"none".
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "must-not-run"),
	}); err != nil {
		t.Fatal(err)
	}
	husk := []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		husk,
	); err != nil {
		t.Fatal(err)
	}

	if err := app.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	saved, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(good) {
		t.Fatalf("saved slot = %s, want the original credential preserved", saved)
	}
	// The husk must not be fingerprint-blessed either: a real login replacing
	// it has to look like a change and reconcile.
	fingerprint, _ := app.providerAccounts.CredentialFingerprintForTest(string(provider.Claude))
	if fingerprint != sha256.Sum256(good) {
		t.Fatal("credential fingerprint moved off the saved credential for a blanked canonical file")
	}
}

func TestExternalClaudeLoginReconcilesMetadataAndLiveSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"first"}}`),
	)
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Claude),
		Token:                "claude-process",
		CredentialGeneration: app.providerAccounts.Generation(string(provider.Claude)),
		CredentialAccountID:  "first",
		Liveness:             newSessionLiveness(time.Now()),
	})

	binary := writeProbeMockBinary(
		t,
		`{"email":"second@example.com","subscriptionType":"Claude Max","tokenSource":"oauth","apiProvider":"firstParty"}`,
	)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"second"}}`),
	); err != nil {
		t.Fatal(err)
	}

	var events struct {
		sync.Mutex
		account int
		session int
	}
	app.testEmitHook = func(name string, _ any) {
		events.Lock()
		defer events.Unlock()
		switch name {
		case "provider:account":
			events.account++
		case "provider:session_account":
			events.session++
		}
	}

	if err := app.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}
	active, ok := app.providerAccounts.Active(string(provider.Claude), time.Now())
	if !ok || active.Email != "second@example.com" {
		t.Fatalf("active account = %+v, ok=%v", active, ok)
	}
	entry, _ := app.sessionManager().get("thread")
	if got := entry.CredentialAccountID; got != active.ID {
		t.Fatalf("live Claude session account = %q, want %q", got, active.ID)
	}
	events.Lock()
	defer events.Unlock()
	if events.account != 1 || events.session != 1 {
		t.Fatalf("events = account:%d session:%d, want 1/1", events.account, events.session)
	}
}

func TestExternalCodexLoginLeavesRunningSessionOnCachedAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"first@example.com",
		[]byte(`{"tokens":{"access_token":"first"}}`),
	)
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Codex),
		Token:                "codex-process",
		CredentialGeneration: app.providerAccounts.Generation(string(provider.Codex)),
		CredentialAccountID:  "first",
		Liveness:             newSessionLiveness(time.Now()),
	})

	binary := writeCodexIdentityProbeBinary(t, "second@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		activePath,
		[]byte(`{"tokens":{"access_token":"second"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}
	active, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || active.Email != "second@example.com" {
		t.Fatalf("active account = %+v, ok=%v", active, ok)
	}
	entry, _ := app.sessionManager().get("thread")
	if got := entry.CredentialAccountID; got != "first" {
		t.Fatalf("running Codex session account = %q, want cached account first", got)
	}
}

func TestExternalCodexTokenRotationUpdatesSavedAccountCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"first@example.com",
		[]byte(`{"tokens":{"access_token":"initial"}}`),
	)
	generation := app.providerAccounts.Generation(string(provider.Codex))

	accountPath, err := providerCredentialsForTest(t, app).AccountCredentialPath(
		string(provider.Codex),
		"first",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		accountPath,
		[]byte(`{"tokens":{"access_token":"managed-old"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(accountPath, old, old); err != nil {
		t.Fatal(err)
	}

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		activePath,
		[]byte(`{"tokens":{"access_token":"external-refresh"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	binary := writeCodexIdentityProbeBinary(t, "first@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	got, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	const externalCredential = `{"tokens":{"access_token":"external-refresh"}}`
	if string(got) != externalCredential {
		t.Fatalf("saved account credential = %s, want %s", got, externalCredential)
	}
	if got := app.providerAccounts.Generation(string(provider.Codex)); got != generation+1 {
		t.Fatalf("credential generation = %d, want %d", got, generation+1)
	}
}

func TestExternalCodexReconciliationUsesCanonicalCredentialAsTruth(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"first@example.com",
		[]byte(`{"tokens":{"access_token":"initial"}}`),
	)

	accountPath, err := providerCredentialsForTest(t, app).AccountCredentialPath(
		string(provider.Codex),
		"first",
	)
	if err != nil {
		t.Fatal(err)
	}
	const managedCredential = `{"tokens":{"access_token":"managed-refresh"}}`
	if err := os.WriteFile(accountPath, []byte(managedCredential), 0o600); err != nil {
		t.Fatal(err)
	}

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		activePath,
		[]byte(`{"tokens":{"access_token":"external-old"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(activePath, old, old); err != nil {
		t.Fatal(err)
	}

	binary := writeCodexIdentityProbeBinary(t, "first@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	got, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	const canonicalCredential = `{"tokens":{"access_token":"external-old"}}`
	if string(got) != canonicalCredential {
		t.Fatalf("saved account credential = %s, want %s", got, canonicalCredential)
	}
}

func TestManagedClaudeSwitchUpdatesLiveSessionsImmediately(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"first"}}`),
	)
	if err := providerCredentialsForTest(t, app).WriteAccountCredential(
		string(provider.Claude),
		"second",
		[]byte(`{"claudeAiOauth":{"accessToken":"second"}}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().UpsertAndActivate(provideraccounts.Account{
		ID:       "second",
		Provider: string(provider.Claude),
		Email:    "second@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().Activate(string(provider.Claude), "first"); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Claude),
		Token:                "claude-process",
		CredentialGeneration: app.providerAccounts.Generation(string(provider.Claude)),
		CredentialAccountID:  "first",
		Liveness:             newSessionLiveness(time.Now()),
	})

	var sessionEvents int
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:session_account" {
			sessionEvents++
		}
	}
	if _, err := app.SwitchProviderAccount(string(provider.Claude), "second"); err != nil {
		t.Fatalf("SwitchProviderAccount: %v", err)
	}

	got, _ := app.sessionManager().get("thread")
	if got.CredentialAccountID != "second" {
		t.Fatalf("live Claude session account = %q, want second", got.CredentialAccountID)
	}
	if got.CredentialGeneration != app.providerAccounts.Generation(string(provider.Claude)) {
		t.Fatalf(
			"live Claude session generation = %d, want %d",
			got.CredentialGeneration,
			app.providerAccounts.Generation(string(provider.Claude)),
		)
	}
	if sessionEvents != 1 {
		t.Fatalf("provider session account events = %d, want 1", sessionEvents)
	}
}

// After a dead account's failed refresh, the CLI blanks the canonical
// credential in place. Switching away must read that husk as a sign-out and
// proceed — preserving nothing — rather than refusing with "credentials
// changed". Refusing bricked every switch and delete on 2026-08-03, because
// nothing inside the app ever replaces the husk.
func TestSwitchSucceedsAfterProviderBlanksCanonicalCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	credFirst := []byte(`{"claudeAiOauth":{"accessToken":"first","refreshToken":"first-refresh"}}`)
	credSecond := []byte(`{"claudeAiOauth":{"accessToken":"second","refreshToken":"second-refresh"}}`)
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		credFirst,
	)
	if err := providerCredentialsForTest(t, app).WriteAccountCredential(
		string(provider.Claude),
		"second",
		credSecond,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().UpsertAndActivate(provideraccounts.Account{
		ID:       "second",
		Provider: string(provider.Claude),
		Email:    "second@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.providerAccounts.MetadataStoreForTest().Activate(string(provider.Claude), "first"); err != nil {
		t.Fatal(err)
	}
	// A husk must not send reconciliation or the switch to the identity probe.
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "must-not-run"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	); err != nil {
		t.Fatal(err)
	}

	got, err := app.SwitchProviderAccount(string(provider.Claude), "second")
	if err != nil {
		t.Fatalf("SwitchProviderAccount() error = %v", err)
	}
	if got.Account.ID != "second" {
		t.Fatalf("switched account = %+v, want second", got.Account)
	}
	canonical, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(credSecond) {
		t.Fatalf("canonical credential = %s, want the target account installed", canonical)
	}
	// The outgoing slot keeps its last saved pair; the husk reaches nothing.
	saved, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Claude), "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(credFirst) {
		t.Fatalf("outgoing slot = %s, want its saved credential preserved", saved)
	}
	fingerprint, _ := app.providerAccounts.CredentialFingerprintForTest(string(provider.Claude))
	if fingerprint != sha256.Sum256(credSecond) {
		t.Fatal("credential fingerprint was not advanced to the switched-in credential")
	}
}

func TestUnchangedCredentialFingerprintSkipsIdentityProbe(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"unchanged"}}`),
	)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "must-not-run"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := app.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		t.Fatalf("unchanged credential unexpectedly probed: %v", err)
	}
}

func TestObservedIdentityEnrichesLegacyAccountWithoutCreatingDuplicate(t *testing.T) {
	app := newTestAppWithStore(t)
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"legacy",
		"",
		[]byte(`{"tokens":{"access_token":"legacy"}}`),
	)

	type identityResult struct {
		accountID string
		updated   *provideraccounts.Account
		err       error
	}
	result := make(chan identityResult, 1)
	releaseSelection := app.providerAccounts.HoldSelectionWriteLockForTest()
	go func() {
		accountID, updated, err := app.accountIDForObservedIdentity(
			string(provider.Codex),
			"legacy",
			provider.AccountInfo{
				Email:            "legacy@example.com",
				SubscriptionType: "pro",
				APIProvider:      "openai",
			},
		)
		result <- identityResult{
			accountID: accountID,
			updated:   updated,
			err:       err,
		}
	}()
	var got identityResult
	select {
	case got = <-result:
		releaseSelection()
	case <-time.After(time.Second):
		releaseSelection()
		<-result
		t.Fatal("identity reconciliation blocked while caller held providerAccountMu")
	}
	accountID, updated, err := got.accountID, got.updated, got.err
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || updated.Email != "legacy@example.com" {
		t.Fatalf("updated account = %+v, want enriched metadata", updated)
	}
	if accountID != "legacy" {
		t.Fatalf("account ID = %q, want legacy", accountID)
	}
	accounts := app.providerAccounts.List(string(provider.Codex), time.Now())
	if len(accounts) != 1 || accounts[0].Email != "legacy@example.com" {
		t.Fatalf("accounts = %+v, want one enriched legacy account", accounts)
	}
}

func TestThreadLiveStateIncludesCurrentSessionAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-account-live-state")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"first@example.com",
		[]byte(`{"tokens":{"access_token":"first"}}`),
	)
	app.sessionManager().put(thread.ID, session{
		Provider:            string(provider.Codex),
		Token:               "codex-process",
		CredentialAccountID: "first",
		Liveness:            newSessionLiveness(time.Now()),
	})

	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ProviderAccount == nil ||
		state.ProviderAccount.AccountID != "first" ||
		state.ProviderAccount.Account.Email != "first@example.com" {
		t.Fatalf("provider account = %+v", state.ProviderAccount)
	}
}

// A second workspace on the SAME email is a different login, not a rotation.
// Before org capture this silently overwrote the saved account; now the saved
// slot's own bytes name its workspace (backfill), the observed credential
// names the new one, and reconciliation creates a separate account.
func TestExternalCodexLoginOnDifferentWorkspaceCreatesSeparateAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"shared@example.com",
		[]byte(`{"tokens":{"access_token":"a","account_id":"ws-a"}}`),
	)

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		activePath,
		[]byte(`{"tokens":{"access_token":"b","account_id":"ws-b"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	binary := writeCodexIdentityProbeBinary(t, "shared@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	accounts := app.providerAccounts.List(string(provider.Codex), time.Now())
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want 2", accounts)
	}
	active, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || active.ID == "first" || active.OrgID != "ws-b" {
		t.Fatalf("active = %+v, want a NEW account on ws-b", active)
	}
	first, ok := app.providerAccounts.Get(string(provider.Codex), "first", time.Now())
	if !ok || first.OrgID != "ws-a" {
		t.Fatalf("first = %+v, want backfilled OrgID ws-a", first)
	}
}

// The same workspace re-observed lands back on its saved account (no
// duplicate), and the observation enriches the legacy blank org.
func TestExternalCodexSameWorkspaceRelandsOnSavedAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"first",
		"shared@example.com",
		[]byte(`{"tokens":{"access_token":"a","account_id":"ws-a"}}`),
	)

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	// A refreshed token for the same workspace: new bytes, same account_id.
	if err := os.WriteFile(
		activePath,
		[]byte(`{"tokens":{"access_token":"a2","account_id":"ws-a"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	binary := writeCodexIdentityProbeBinary(t, "shared@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	accounts := app.providerAccounts.List(string(provider.Codex), time.Now())
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want the one saved account", accounts)
	}
	if accounts[0].ID != "first" || accounts[0].OrgID != "ws-a" {
		t.Fatalf("account = %+v, want first enriched with ws-a", accounts[0])
	}
}

// An observation that cannot pick between several same-email accounts
// (no org id, e.g. an API-key credential) must not fail reconciliation on
// every tick: when the active account carries the observed email and
// nothing contradicts it, the credential re-lands on the current
// assignment.
func TestReconcileAmbiguousIdentityKeepsTheActiveAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"acct-a",
		"shared@example.com",
		[]byte(`{"OPENAI_API_KEY":"k1"}`),
	)
	// Enrich acct-a's org FIRST — the write layer refuses a same-email
	// sibling while either side's org is unknown — then add the sibling and
	// re-activate acct-a.
	for _, account := range []provideraccounts.Account{
		{ID: "acct-a", Provider: string(provider.Codex), Email: "shared@example.com", OrgID: "ws-a"},
		{ID: "acct-b", Provider: string(provider.Codex), Email: "shared@example.com", OrgID: "ws-b"},
		{ID: "acct-a", Provider: string(provider.Codex), Email: "shared@example.com", OrgID: "ws-a"},
	} {
		if _, err := app.providerAccounts.MetadataStoreForTest().UpsertAndActivate(account); err != nil {
			t.Fatal(err)
		}
	}

	activePath, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	// An externally refreshed API-key credential: new bytes, no workspace id.
	if err := os.WriteFile(activePath, []byte(`{"OPENAI_API_KEY":"k2"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	binary := writeCodexIdentityProbeBinary(t, "shared@example.com")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileExternalProviderAccount(string(provider.Codex)); err != nil {
		t.Fatalf("reconcileExternalProviderAccount: %v", err)
	}

	accounts := app.providerAccounts.List(string(provider.Codex), time.Now())
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want the two saved accounts", accounts)
	}
	active, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || active.ID != "acct-a" || active.OrgID != "ws-a" {
		t.Fatalf("active = %+v, want acct-a still active with ws-a", active)
	}
	saved, err := providerCredentialsForTest(t, app).ReadCredential(string(provider.Codex), "acct-a", false)
	if err != nil || string(saved) != `{"OPENAI_API_KEY":"k2"}` {
		t.Fatalf("acct-a slot = %q, %v; want the refreshed credential", saved, err)
	}
}

func TestAccountIDForObservedIdentityDisambiguatesByOrg(t *testing.T) {
	app := newTestAppWithStore(t)
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []provideraccounts.Account{
		{ID: "acct-a", Provider: string(provider.Claude), Email: "dup@x.com", OrgID: "org-a", OrgName: "Alpha"},
		{ID: "acct-b", Provider: string(provider.Claude), Email: "dup@x.com", OrgID: "org-b", OrgName: "Beta"},
	} {
		if _, err := accounts.UpsertAndActivate(account); err != nil {
			t.Fatal(err)
		}
	}
	attachProviderAccountStoresForTest(t, app, accounts, newTestProviderCredentials(t, t.TempDir()))

	cases := []struct {
		name     string
		expected string
		info     provider.AccountInfo
		wantID   string
		wantErr  bool
	}{
		{
			name:     "org id reassigns within the same email",
			expected: "acct-a",
			info:     provider.AccountInfo{Email: "dup@x.com", OrgID: "org-b"},
			wantID:   "acct-b",
		},
		{
			// A name is display state and never evidence: the expected
			// account keeps its claim even when the observation names the
			// sibling org, because renames and source-formatting differences
			// make names unreliable and only an org ID may reassign.
			name:     "an observed org name alone cannot reassign",
			expected: "acct-a",
			info:     provider.AccountInfo{Email: "dup@x.com", OrgName: "beta"},
			wantID:   "acct-a",
		},
		{
			name:     "email-only observation cannot contradict the expected claim",
			expected: "acct-a",
			info:     provider.AccountInfo{Email: "dup@x.com"},
			wantID:   "acct-a",
		},
		{
			name:     "empty identity keeps the expected assignment",
			expected: "acct-a",
			info:     provider.AccountInfo{},
			wantID:   "acct-a",
		},
		{
			name:     "an unsaved organization is refused, not guessed",
			expected: "acct-a",
			info:     provider.AccountInfo{Email: "dup@x.com", OrgID: "org-c"},
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		got, _, err := app.accountIDForObservedIdentity(string(provider.Claude), tc.expected, tc.info)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got %q", tc.name, got)
			}
			continue
		}
		if err != nil || got != tc.wantID {
			t.Errorf("%s: got (%q, %v), want %q", tc.name, got, err, tc.wantID)
		}
	}
}

func TestAssertSelectedClaudeIdentityUsesOrgContradiction(t *testing.T) {
	app := newTestAppWithStore(t)
	selection := providerAccountSelection{
		AccountID: "acct-a",
		Account:   provider.AccountInfo{Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha"},
	}
	cases := []struct {
		name    string
		info    provider.AccountInfo
		wantErr bool
	}{
		{"empty post-switch identity", provider.AccountInfo{}, false},
		{"same login", provider.AccountInfo{Email: "E@X.com", OrgID: "org-1"}, false},
		{"same email different org id", provider.AccountInfo{Email: "e@x.com", OrgID: "org-2"}, true},
		// A differing display name is a rename or a source-formatting
		// difference, never proof of a different login: refusing here would
		// drop the token rotation this refresh just spent (2026-08-18
		// incident shape).
		{"same email different org name without ids", provider.AccountInfo{Email: "e@x.com", OrgName: "Beta"}, false},
		{"different email", provider.AccountInfo{Email: "other@x.com"}, true},
		{"rename under the same org id", provider.AccountInfo{Email: "e@x.com", OrgID: "org-1", OrgName: "Renamed"}, false},
	}
	for _, tc := range cases {
		err := app.assertSelectedClaudeIdentity(selection, tc.info)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}
