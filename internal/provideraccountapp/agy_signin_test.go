package provideraccountapp

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// completedAgyLogin is what `agy acp --login` leaves behind: a temporary home
// holding the new credential.
func completedAgyLogin(t *testing.T, rig agyReadingRig, credential []byte) *loginAttempt {
	t.Helper()
	agyName := string(provider.Agy)
	home, err := rig.credentials.NewEphemeralHomeWithCredential(agyName, credential, provideraccounts.EphemeralOwner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.Cleanup() })
	return &loginAttempt{provider: agyName, binary: rig.manager.providerBinaryPath(agyName), home: home}
}

func agyAccountIDs(rig agyReadingRig) []string {
	var ids []string
	for _, account := range rig.manager.store.List(string(provider.Agy), time.Now()) {
		ids = append(ids, account.ID)
	}
	return ids
}

// Signing in again to the selected account while Google's userinfo cannot be
// reached, with a credential that carries no email, names nobody for certain.
// The sign-in fails and asks for another try; nothing of it is saved. The
// next sign-in, with the lookup answering, renews the saved account.
func TestAnAgySignInWhoseLookupFailedSavesNothingAndTheNextOneRenews(t *testing.T) {
	dir := t.TempDir()
	rig := newAgyReadingRigAt(t, dir)
	agyName := string(provider.Agy)
	before := agyAccountIDs(rig)
	generation := rig.manager.store.Generation(agyName)
	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("userinfo unreachable for the test")
	})

	fresh := agyCredential("dev-new", "")
	attempt := completedAgyLogin(t, rig, fresh)
	_, err := rig.manager.adoptLogin(attempt)
	if err == nil {
		t.Fatalf("adoptLogin saved a sign-in nobody could identify; accounts = %v", agyAccountIDs(rig))
	}
	message := loginProse(err)
	if !strings.Contains(message, "look up") || !strings.Contains(message, "Google account") || !strings.Contains(message, "try again") {
		t.Fatalf("sign-in failure reads %q, want it to name the failed lookup and ask to try again", message)
	}
	if got := agyAccountIDs(rig); len(got) != len(before) || got[0] != before[0] {
		t.Fatalf("accounts after the failed sign-in = %v, want %v", got, before)
	}
	if got := reservedIDs(t, dir, agyName); len(got) != 0 {
		t.Fatalf("reservations after the failed sign-in = %v, want none", got)
	}
	slotRoot := filepath.Dir(filepath.Dir(mustAgyAccountCredentialPath(t, rig.credentials, rig.devID)))
	if names := slotNames(t, slotRoot); len(names) != 1 || names[0] != rig.devID {
		t.Fatalf("slots after the failed sign-in = %v, want only dev's", names)
	}
	if got := rig.slot(t, rig.devID); got == string(fresh) {
		t.Fatal("dev's slot took a sign-in nobody identified")
	}
	if rig.manager.store.Generation(agyName) != generation {
		t.Fatal("the failed sign-in changed the selection")
	}
	if err := attempt.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(attempt.home.Path); !os.IsNotExist(statErr) {
		t.Fatalf("temporary sign-in home after teardown: %v, want it gone", statErr)
	}

	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return agyAnswer(http.StatusOK, `{"email":"dev@example.com","name":"Dev"}`), nil
	})
	renewed := agyCredential("devr-1", "")
	result, err := rig.manager.adoptLogin(completedAgyLogin(t, rig, renewed))
	if err != nil {
		t.Fatalf("sign-in with the lookup answering: %v", err)
	}
	if result.Account.ID != rig.devID || result.Account.Email != "dev@example.com" {
		t.Fatalf("sign-in landed on %+v, want dev renewed", result.Account)
	}
	if got := agyAccountIDs(rig); len(got) != 1 || got[0] != rig.devID {
		t.Fatalf("accounts after the renewal = %v, want only dev", got)
	}
	if got := rig.slot(t, rig.devID); got != string(renewed) {
		t.Fatalf("dev's slot = %s, want the renewed login", got)
	}
}

func mustAgyAccountCredentialPath(t *testing.T, credentials *provideraccounts.Credentials, accountID string) string {
	t.Helper()
	path, err := credentials.AccountCredentialPath(string(provider.Agy), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
