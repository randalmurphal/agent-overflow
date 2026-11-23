package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
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

// A lookup that ran out of time is a failed lookup: the person reads the
// lookup's sentence, not the sentence of a sign-in that ran out of time.
func TestAnAgySignInWhoseLookupTimedOutNamesTheLookup(t *testing.T) {
	rig := newAgyReadingRig(t)
	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("userinfo for the test: %w", context.DeadlineExceeded)
	})

	_, err := rig.manager.adoptLogin(completedAgyLogin(t, rig, agyCredential("dev-new", "")))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("adoptLogin error = %v, want the lookup's timeout kept in the chain", err)
	}
	if got := loginProse(err); got != agySignInUnidentifiedSentence {
		t.Fatalf("sign-in failure reads %q, want %q", got, agySignInUnidentifiedSentence)
	}
	if code, _, ok := errorsx.PublicDetails(err); !ok || code != "agy_sign_in_unidentified" {
		t.Fatalf("public details = %q, %v; want the sign-in's own code", code, ok)
	}
}

// A sign-in whose lookup failed and whose teardown then failed too, ended as
// the coordinator ends every run: the person reads the lookup's sentence and
// the teardown's failure, and the audit names the teardown, since the
// temporary home it left holds the new credential.
func TestAFailedSignInWhoseTeardownFailedSaysBoth(t *testing.T) {
	rig := newAgyReadingRigAt(t, t.TempDir())
	audit := filepath.Join(t.TempDir(), "audit.log")
	rig.manager.auditPath = audit
	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("userinfo unreachable for the test")
	})
	attempt := completedAgyLogin(t, rig, agyCredential("dev-new", ""))
	run := &loginRun{provider: string(provider.Agy), attempt: attempt, done: make(chan struct{}), cancel: func() {}}
	rig.manager.registerLoginRun(run)

	_, err := rig.manager.adoptLogin(attempt)
	if err == nil {
		t.Fatal("adoptLogin saved a sign-in nobody could identify")
	}
	parent := filepath.Dir(attempt.home.Path)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	down := run.teardown()
	if down.homeErr == nil {
		t.Fatal("the teardown removed the temporary home from a read-only directory")
	}
	rig.manager.finishLoginRun(run, err, down)

	state := rig.manager.ProviderLoginState(string(provider.Agy))
	if state.Phase != LoginPhaseFailed || !strings.HasPrefix(state.Error, agySignInUnidentifiedSentence) || !strings.Contains(state.Error, "remove temporary agy home") {
		t.Fatalf("sign-in state = %s %q, want the lookup's sentence and the teardown's failure", state.Phase, state.Error)
	}
	logged, readErr := os.ReadFile(audit)
	if readErr != nil || !strings.Contains(string(logged), "temporary home could not be removed") || strings.Contains(string(logged), "did not close") {
		t.Fatalf("audit = %q, %v; want the home's removal failure and nothing else", logged, readErr)
	}
}

// A sign-in error longer than its bound still leaves the teardown failure
// visible after it, and a process that did not close is audited as that, not
// as a temporary home left behind.
func TestALongSignInErrorKeepsItsTeardownFailureVisible(t *testing.T) {
	manager, _, _ := newTestManager(t)
	audit := filepath.Join(t.TempDir(), "audit.log")
	manager.auditPath = audit
	run := &loginRun{provider: string(provider.Agy), done: make(chan struct{}), cancel: func() {}}
	manager.registerLoginRun(run)
	long := errors.New(strings.Repeat("the provider said something long. ", 20))
	if n := len([]rune(long.Error())); n <= loginProseRunes {
		t.Fatalf("the sign-in error is %d runes, want more than %d", n, loginProseRunes)
	}

	manager.finishLoginRun(run, long, loginTeardown{closeErr: errors.New("close the agy sign-in process: broken pipe")})

	state := manager.ProviderLoginState(string(provider.Agy))
	if state.Phase != LoginPhaseFailed || !strings.HasPrefix(state.Error, "the provider said something long.") || !strings.HasSuffix(state.Error, "close the agy sign-in process: broken pipe") {
		t.Fatalf("sign-in state = %s %q, want the bounded sign-in error and then the teardown failure", state.Phase, state.Error)
	}
	logged, err := os.ReadFile(audit)
	if err != nil || !strings.Contains(string(logged), "process did not close cleanly") || strings.Contains(string(logged), "temporary home") {
		t.Fatalf("audit = %q, %v; want the process close failure and no home", logged, err)
	}
}

// Every failed sign-in shows its own sentence and then its teardown's
// failure; the generic one reads as it always has.
func TestEveryFailedSignInShowsItsTeardownFailure(t *testing.T) {
	cleanup := errors.New("clean temporary agy login home: busy")
	for name, err := range map[string]error{
		"lookup":  agySignInUnidentifiedError{cause: context.DeadlineExceeded},
		"timeout": context.DeadlineExceeded,
		"cancel":  context.Canceled,
		"generic": errors.New("agy login exited"),
	} {
		got := loginFailureProse(err, cleanup)
		if !strings.HasPrefix(got, loginProse(err)) || !strings.HasSuffix(got, cleanup.Error()) {
			t.Errorf("%s: failure reads %q, want %q followed by the teardown's failure", name, got, loginProse(err))
		}
		if loginFailureProse(err, nil) != loginProse(err) {
			t.Errorf("%s: failure without a teardown failure reads %q", name, loginFailureProse(err, nil))
		}
	}
	generic := errors.New("agy login exited")
	if got, was := loginFailureProse(generic, cleanup), loginProse(errors.Join(generic, cleanup)); got != was {
		t.Errorf("generic failure reads %q, was %q", got, was)
	}
}
