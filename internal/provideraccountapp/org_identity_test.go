package provideraccountapp

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccounts"
)

const sharedEmail = "same@example.com"

// sameEmailOrgs is two saved accounts on one email in different
// organizations, the first selected on its canonical login, the second
// holding its own slot.
func sameEmailOrgs(t *testing.T, transport http.RoundTripper) (
	*Manager,
	*provideraccounts.Store,
	*provideraccounts.Credentials,
	[]byte,
	[]byte,
) {
	t.Helper()
	manager, store, credentials := newTestManager(t)
	manager.deps.Accounts = &recordingAccountSink{}
	probe := writeClaudeIdentityProbe(t, sharedEmail)
	manager.deps.ProviderBinary = func(string) string { return probe }
	manager.deps.HTTPClient = func() *http.Client { return &http.Client{Transport: transport} }

	claudeName := string(provider.Claude)
	loginA := claudeLogin(claudefake.AccessToken(sharedEmail, "org-a", "a1"), time.Now().Add(20*24*time.Hour))
	loginB := claudeLogin(claudefake.AccessToken(sharedEmail, "org-b", "b1"), time.Now().Add(20*24*time.Hour))
	for _, account := range []provideraccounts.Account{
		{ID: "b", Provider: claudeName, Email: sharedEmail, OrgID: "org-b"},
		{ID: "a", Provider: claudeName, Email: sharedEmail, OrgID: "org-a"},
	} {
		if _, err := store.UpsertAndActivate(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := credentials.WriteAccountCredential(claudeName, "b", loginB); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(claudeName, "a", loginA); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest(claudeName, loginA); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.rememberProviderCredentialFingerprintLocked(claudeName, loginA)
	manager.mu.Unlock()
	return manager, store, credentials, loginA, loginB
}

// staleRecordAfterSwitch switches from a to b, then does what a CLI that was
// running on a does: writes a's oauthAccount record back over the one the
// switch retired, and rotates the canonical credential, which is now b's.
func staleRecordAfterSwitch(
	t *testing.T,
	manager *Manager,
	credentials *provideraccounts.Credentials,
	rotatedB []byte,
) {
	t.Helper()
	claudeName := string(provider.Claude)
	if _, err := manager.SwitchProviderAccount(claudeName, "b"); err != nil {
		t.Fatal(err)
	}
	paths, err := credentials.Paths(claudeName)
	if err != nil {
		t.Fatal(err)
	}
	record := `{"oauthAccount":{"emailAddress":"` + sharedEmail + `","organizationUuid":"org-a","organizationName":"Org A"}}`
	if err := os.WriteFile(paths.GlobalConfig, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest(claudeName, rotatedB); err != nil {
		t.Fatal(err)
	}
}

// The reported incident: after a switch between two organizations on one
// email, a stale identity record named the previous organization. The
// rotated login must stay with the account it belongs to, and the previous
// account's own login must survive.
func TestReconcileAfterASameEmailSwitchKeepsEachOrganizationsLogin(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, credentials, loginA, _ := sameEmailOrgs(t, claudefake.Transport{})
	rotatedB := claudeLogin(claudefake.AccessToken(sharedEmail, "org-b", "b2"), time.Now().Add(20*24*time.Hour))
	staleRecordAfterSwitch(t, manager, credentials, rotatedB)

	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatal(err)
	}
	if got := activeID(t, store); got != "b" {
		t.Fatalf("active = %q, want b: the rotation was attributed to the previous organization", got)
	}
	if !bytes.Equal(readCredential(t, credentials, "a"), loginA) {
		t.Fatal("the previous organization's saved login was overwritten")
	}
	if !bytes.Equal(readCredential(t, credentials, "b"), rotatedB) {
		t.Fatal("the rotation was not saved to the account it belongs to")
	}
}

// Without the server's answer the credential is not attributed at all: the
// reconcile fails visibly and nothing moves.
func TestReconcileThatCannotIdentifyTheLoginChangesNothing(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, credentials, loginA, loginB := sameEmailOrgs(t, refusingProfile{})
	if _, err := manager.SwitchProviderAccount(claudeName, "b"); err != nil {
		t.Fatal(err)
	}
	rotatedB := claudeLogin(claudefake.AccessToken(sharedEmail, "org-b", "b2"), time.Now().Add(20*24*time.Hour))
	if err := credentials.WriteNativeCredentialForTest(claudeName, rotatedB); err != nil {
		t.Fatal(err)
	}

	err := manager.ReconcileExternalProviderAccount(claudeName)
	if !errors.Is(err, errProfileRefused) {
		t.Fatalf("reconcile = %v, want the profile failure", err)
	}
	if got := activeID(t, store); got != "b" {
		t.Fatalf("active = %q, want b", got)
	}
	if !bytes.Equal(readCredential(t, credentials, "a"), loginA) ||
		!bytes.Equal(readCredential(t, credentials, "b"), loginB) {
		t.Fatal("an unidentified credential was written to a slot")
	}
	if !bytes.Equal(readCredential(t, credentials, ""), rotatedB) {
		t.Fatal("the canonical rotation was disturbed")
	}
}

// A sign-in on the other organization's account lands in that account's slot
// whatever the record beside it says.
func TestASignInIsMatchedByItsTokensOrganization(t *testing.T) {
	manager, store, credentials, loginA, _ := sameEmailOrgs(t, claudefake.Transport{})
	fresh := claudeLogin(claudefake.AccessToken(sharedEmail, "org-b", "b-new"), time.Now().Add(30*24*time.Hour))

	result, err := manager.adoptLogin(completedLogin(t, manager, credentials, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "b" || result.Active {
		t.Fatalf("adoptLogin = %+v, want b renewed beside the selected account", result)
	}
	if got := activeID(t, store); got != "a" {
		t.Fatalf("active = %q, want a", got)
	}
	if !bytes.Equal(readCredential(t, credentials, "b"), fresh) ||
		!bytes.Equal(readCredential(t, credentials, ""), loginA) {
		t.Fatal("the sign-in did not land only in b's slot")
	}
}

// A credential AO already attributed needs no request: an account probe of
// it works without the profile endpoint. The moment the canonical bytes are
// ones AO did not attribute, the same probe has to ask, and fails without an
// answer.
func TestAccountProbeAsksOnlyAboutAnUnattributedCredential(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, credentials, loginA, _ := sameEmailOrgs(t, refusingProfile{})
	request := ProbeRequest{
		ProviderName: claudeName,
		Probe:        manager.canonicalProviderAccountProbe(claudeName, manager.providerBinaryPath(claudeName)),
	}

	info, err := manager.RunAccountProbe(request)
	if err != nil {
		t.Fatalf("probe of the attributed credential = %v, want no request", err)
	}
	if info.Email != sharedEmail || info.OrgID != "org-a" {
		t.Fatalf("identity = %+v, want the selected account's", info)
	}

	unattributed := claudeLogin(claudefake.AccessToken(sharedEmail, "org-b", "x"), time.Now().Add(20*24*time.Hour))
	if err := credentials.WriteNativeCredentialForTest(claudeName, unattributed); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunAccountProbe(request); !errors.Is(err, errProfileRefused) {
		t.Fatalf("probe of an unattributed credential = %v, want the profile failure", err)
	}
	if got := activeID(t, store); got != "a" {
		t.Fatalf("active = %q, want a", got)
	}
	if !bytes.Equal(readCredential(t, credentials, "a"), loginA) {
		t.Fatal("an unidentified credential was written to a slot")
	}
}

// Starting a sign-in with no account saved adopts the native login first,
// under mu, identified by its token.
func TestStartingASignInAdoptsTheNativeLoginByItsToken(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, credentials, sink := signInManager(t)
	native := claudeLogin(claudefake.AccessToken("native@example.com", "org-n", "1"), time.Now().Add(20*24*time.Hour))
	if err := credentials.WriteNativeCredentialForTest(claudeName, native); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		attempt, err := manager.beginLogin(claudeName)
		if err == nil {
			err = attempt.cleanup()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("beginLogin did not return: adopting the native login blocked")
	}
	active, ok := store.Active(claudeName, time.Now())
	if !ok || active.Email != "native@example.com" || active.OrgID != "org-n" {
		t.Fatalf("active = %+v (ok=%v), want the native login's own identity", active, ok)
	}
	if signIn := onlySignIn(t, sink); !signIn.Added {
		t.Fatalf("sign-in = %+v, want the native login added", signIn)
	}
}

var errProfileRefused = errors.New("profile endpoint unreachable")

type refusingProfile struct{}

func (refusingProfile) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errProfileRefused
}
