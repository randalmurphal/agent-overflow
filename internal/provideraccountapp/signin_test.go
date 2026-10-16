package provideraccountapp

import (
	"bytes"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

// writeClaudeIdentityProbe is a Claude binary whose account probe reports
// email, whichever home it runs in.
func writeClaudeIdentityProbe(t *testing.T, email string) string {
	t.Helper()
	response := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init",` +
		`"response":{"account":{"email":"` + email + `","subscriptionType":"Claude Max"}}}}`
	return mockexec.Write(t, filepath.Join(t.TempDir(), "mock-claude"),
		"#!/bin/bash\nread -r _ || true\nprintf '%s\\n' '"+response+"'\nexit 0\n")
}

type unreachableTransport struct{}

func (unreachableTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("usage endpoint unreachable in tests")
}

// signInManager is newTestManager whose provider probe identifies email and
// whose usage reads fail without leaving the process.
func signInManager(t *testing.T, email string) (
	*Manager,
	*provideraccounts.Store,
	*provideraccounts.Credentials,
	*recordingAccountSink,
) {
	t.Helper()
	manager, store, credentials := newTestManager(t)
	sink := &recordingAccountSink{}
	manager.deps.Accounts = sink
	probe := writeClaudeIdentityProbe(t, email)
	manager.deps.ProviderBinary = func(string) string { return probe }
	manager.deps.HTTPClient = func() *http.Client {
		return &http.Client{Transport: unreachableTransport{}}
	}
	return manager, store, credentials, sink
}

// completedLogin is what the provider's sign-in leaves behind: a temporary
// home holding the new credential.
func completedLogin(
	t *testing.T,
	manager *Manager,
	credentials *provideraccounts.Credentials,
	credential []byte,
) *loginAttempt {
	t.Helper()
	claudeName := string(provider.Claude)
	home, err := credentials.NewEphemeralHomeWithCredential(claudeName, credential, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.Cleanup() })
	return &loginAttempt{provider: claudeName, binary: manager.providerBinaryPath(claudeName), home: home}
}

func readCredential(t *testing.T, credentials *provideraccounts.Credentials, accountID string) []byte {
	t.Helper()
	data, err := credentials.ReadCredential(string(provider.Claude), accountID, accountID == "")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func activeID(t *testing.T, store *provideraccounts.Store) string {
	t.Helper()
	active, ok := store.Active(string(provider.Claude), time.Now())
	if !ok {
		return ""
	}
	return active.ID
}

func listedAccountIDs(store *provideraccounts.Store) []string {
	var ids []string
	for _, account := range store.List(string(provider.Claude), time.Now()) {
		ids = append(ids, account.ID)
	}
	return ids
}

func onlySignIn(t *testing.T, sink *recordingAccountSink) SignIn {
	t.Helper()
	if len(sink.signIns) != 1 {
		t.Fatalf("sign-in events = %+v, want exactly one", sink.signIns)
	}
	return sink.signIns[0]
}

// Signing in to a saved account that is not the selected one refreshes that
// account's own login. Whatever is running keeps running on the selected
// account: the canonical credential, the selection and its generation stay.
func TestASignInForAnotherSavedAccountRenewsItWithoutSwitching(t *testing.T) {
	manager, store, credentials, sink := signInManager(t, "other@example.com")
	claudeName := string(provider.Claude)
	canonical := claudeLogin("current", time.Now().Add(20*24*time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, canonical, "other", claudeLogin("other-old", time.Now().Add(time.Hour)))
	order := listedAccountIDs(store)
	generation := store.Generation(claudeName)
	fresh := claudeLogin("other-new", time.Now().Add(30*24*time.Hour))

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if result.Active || result.ID != "other" {
		t.Fatalf("adoptLogin = %+v, want the inactive other account", result)
	}
	if got := activeID(t, store); got != "current" {
		t.Fatalf("active = %q, want current", got)
	}
	if got := store.Generation(claudeName); got != generation {
		t.Fatalf("generation = %d, want %d unchanged", got, generation)
	}
	if !bytes.Equal(readCredential(t, credentials, ""), canonical) {
		t.Fatal("the canonical credential changed for a sign-in to an inactive account")
	}
	if !bytes.Equal(readCredential(t, credentials, "other"), fresh) {
		t.Fatal("the other account's slot does not hold the new login")
	}
	if got := listedAccountIDs(store); !slices.Equal(got, order) {
		t.Fatalf("order = %v, want %v unchanged", got, order)
	}
	saved, _ := store.Get(claudeName, "other", time.Now())
	if want := loginExpiresAtMillis(claudeName, fresh); saved.RefreshTokenExpiresAt != want {
		t.Fatalf("other's login expiry = %d, want the new login's %d", saved.RefreshTokenExpiresAt, want)
	}
	signIn := onlySignIn(t, sink)
	if signIn.Account.ID != "other" || !signIn.Renewed || signIn.Added || signIn.Switched || signIn.Active {
		t.Fatalf("sign-in = %+v, want other renewed and not active", signIn)
	}
	if !slices.Contains(sink.events, "changed") {
		t.Fatalf("events = %v, want the listing nudge", sink.events)
	}
}

// A login nobody has saved is appended after the existing accounts and is not
// selected.
func TestASignInForANewAccountIsAddedLastWithoutSwitching(t *testing.T) {
	manager, store, credentials, sink := signInManager(t, "new@example.com")
	claudeName := string(provider.Claude)
	canonical := claudeLogin("current", time.Now().Add(20*24*time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, canonical, "other", claudeLogin("other", time.Now().Add(20*24*time.Hour)))
	order := listedAccountIDs(store)
	generation := store.Generation(claudeName)
	fresh := claudeLogin("new", time.Now().Add(30*24*time.Hour))

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listedAccountIDs(store), append(order, result.ID); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want the new account last: %v", got, want)
	}
	if result.Active || result.Email != "new@example.com" {
		t.Fatalf("adoptLogin = %+v, want the new inactive account", result)
	}
	if got := activeID(t, store); got != "current" {
		t.Fatalf("active = %q, want current", got)
	}
	if got := store.Generation(claudeName); got != generation {
		t.Fatalf("generation = %d, want %d unchanged", got, generation)
	}
	if !bytes.Equal(readCredential(t, credentials, ""), canonical) {
		t.Fatal("the canonical credential changed for a new account's sign-in")
	}
	if !bytes.Equal(readCredential(t, credentials, result.ID), fresh) {
		t.Fatal("the new account's slot does not hold its login")
	}
	signIn := onlySignIn(t, sink)
	if signIn.Account.ID != result.ID || !signIn.Added || signIn.Renewed || signIn.Switched || signIn.Active {
		t.Fatalf("sign-in = %+v, want the new account added and not active", signIn)
	}
}

// Signing the selected account in again replaces the canonical login, which
// is the one every provider process runs on.
func TestASignInForTheSelectedAccountRenewsTheCanonicalLogin(t *testing.T) {
	manager, store, credentials, sink := signInManager(t, "current@example.com")
	canonical := claudeLogin("current", time.Now().Add(time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, canonical, "other", claudeLogin("other", time.Now().Add(20*24*time.Hour)))
	fresh := claudeLogin("current-new", time.Now().Add(30*24*time.Hour))

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || result.ID != "current" {
		t.Fatalf("adoptLogin = %+v, want the active current account", result)
	}
	if !bytes.Equal(readCredential(t, credentials, ""), fresh) {
		t.Fatal("the canonical credential does not hold the renewed login")
	}
	signIn := onlySignIn(t, sink)
	if signIn.Account.ID != "current" || !signIn.Renewed || !signIn.Active || signIn.Switched || signIn.Added {
		t.Fatalf("sign-in = %+v, want current renewed and active", signIn)
	}
}

// With nothing selected there is nothing to keep running, so the first login
// becomes the selected account.
func TestTheFirstSignInBecomesTheSelectedAccount(t *testing.T) {
	manager, store, credentials, sink := signInManager(t, "first@example.com")
	fresh := claudeLogin("first", time.Now().Add(30*24*time.Hour))

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || activeID(t, store) != result.ID {
		t.Fatalf("adoptLogin = %+v, active = %q, want the first account selected", result, activeID(t, store))
	}
	if !bytes.Equal(readCredential(t, credentials, ""), fresh) {
		t.Fatal("the canonical credential does not hold the first login")
	}
	signIn := onlySignIn(t, sink)
	if !signIn.Added || !signIn.Active || signIn.Switched || signIn.Renewed {
		t.Fatalf("sign-in = %+v, want added and active", signIn)
	}
}

// reconcileSeed is two saved accounts with "current" selected on a login
// whose refresh deadline is deadline, and the canonical fingerprint forgotten
// the way a fresh process starts.
func reconcileSeed(t *testing.T, email string, deadline time.Time) (
	*Manager,
	*provideraccounts.Store,
	*provideraccounts.Credentials,
	*recordingAccountSink,
) {
	t.Helper()
	manager, store, credentials, sink := signInManager(t, email)
	canonical := claudeCredential("current", time.Now().Add(time.Hour), deadline)
	seedClaudeAccounts(t, manager, store, credentials, canonical, "other", claudeLogin("other", time.Now().Add(20*24*time.Hour)))
	manager.mu.Lock()
	delete(manager.fingerprints, string(provider.Claude))
	manager.mu.Unlock()
	return manager, store, credentials, sink
}

// A process start re-adopts the selected login it finds, and the CLI rotates
// tokens all day. Neither is a sign-in.
func TestReconcilingTheSelectedLoginOrItsRotationReportsNoSignIn(t *testing.T) {
	claudeName := string(provider.Claude)
	deadline := time.Now().Add(20 * 24 * time.Hour)

	manager, store, credentials, sink := reconcileSeed(t, "current@example.com", deadline)
	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatal(err)
	}
	rotated := claudeCredential("current-rotated", time.Now().Add(8*time.Hour), deadline)
	if err := credentials.WriteNativeCredentialForTest(claudeName, rotated); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readCredential(t, credentials, "current"), rotated) {
		t.Fatal("the rotation was not reconciled into the selected account's slot")
	}
	if got := activeID(t, store); got != "current" {
		t.Fatalf("active = %q, want current", got)
	}
	if len(sink.signIns) != 0 || slices.Contains(sink.events, "changed") {
		t.Fatalf("events = %v, sign-ins = %+v, want neither for a re-adoption or rotation", sink.events, sink.signIns)
	}
}

// A `/login` for the selected account outside Agent Overflow carries a later
// refresh deadline than the login it replaces.
func TestAnExternalSignInForTheSelectedAccountReportsARenewal(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, credentials, sink := reconcileSeed(t, "current@example.com", time.Now().Add(time.Hour))
	if err := credentials.WriteNativeCredentialForTest(
		claudeName,
		claudeLogin("current-new", time.Now().Add(30*24*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatal(err)
	}
	signIn := onlySignIn(t, sink)
	if signIn.Account.ID != "current" || !signIn.Renewed || !signIn.Active || signIn.Switched || signIn.Added {
		t.Fatalf("sign-in = %+v, want current renewed in place", signIn)
	}
}

// A `/login` to another account outside Agent Overflow moves the canonical
// credential, which every provider process follows, so it is a switch.
func TestAnExternalSignInForAnotherAccountReportsTheSwitch(t *testing.T) {
	claudeName := string(provider.Claude)
	for _, tc := range []struct {
		email string
		added bool
	}{
		{email: "other@example.com"},
		{email: "new@example.com", added: true},
	} {
		t.Run(tc.email, func(t *testing.T) {
			manager, store, credentials, sink := reconcileSeed(t, tc.email, time.Now().Add(20*24*time.Hour))
			if err := credentials.WriteNativeCredentialForTest(
				claudeName,
				claudeLogin("external", time.Now().Add(30*24*time.Hour)),
			); err != nil {
				t.Fatal(err)
			}
			if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
				t.Fatal(err)
			}
			signIn := onlySignIn(t, sink)
			if signIn.Account.Email != tc.email || !signIn.Switched || !signIn.Active || signIn.Added != tc.added {
				t.Fatalf("sign-in = %+v, want a switch to %s (added=%v)", signIn, tc.email, tc.added)
			}
			if got := activeID(t, store); got != signIn.Account.ID {
				t.Fatalf("active = %q, want %q", got, signIn.Account.ID)
			}
			if !slices.Contains(sink.events, "changed") {
				t.Fatalf("events = %v, want the listing nudge", sink.events)
			}
			if ids := listedAccountIDs(store); ids[len(ids)-1] != signIn.Account.ID && tc.added {
				t.Fatalf("order = %v, want the added account last", ids)
			}
		})
	}
}

func TestReorderProviderAccountsNudgesTheListing(t *testing.T) {
	manager, store, credentials, sink := signInManager(t, "current@example.com")
	seedClaudeAccounts(t, manager, store, credentials, claudeLogin("current", time.Now().Add(time.Hour)), "other", claudeLogin("other", time.Now().Add(time.Hour)))

	if err := manager.ReorderProviderAccounts(string(provider.Claude), []string{"current", "other"}); err != nil {
		t.Fatal(err)
	}
	if got := listedAccountIDs(store); !slices.Equal(got, []string{"current", "other"}) {
		t.Fatalf("order = %v", got)
	}
	if !slices.Equal(sink.events, []string{"changed"}) {
		t.Fatalf("events = %v, want one listing nudge", sink.events)
	}
	sink.events = nil
	if err := manager.ReorderProviderAccounts(string(provider.Claude), []string{"other"}); !errors.Is(err, provideraccounts.ErrAccountOrderStale) {
		t.Fatalf("stale reorder = %v, want ErrAccountOrderStale", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("a refused reorder emitted %v", sink.events)
	}
}
