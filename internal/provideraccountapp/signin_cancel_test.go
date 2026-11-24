package provideraccountapp

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/testutil/mockexec"
)

// lingerLimit is how long a test waits for a sign-in step to notice that
// its sign-in ended before it fails the test and lets the step go on. A step
// that sees the end returns at once.
const lingerLimit = 5 * time.Second

// adoptionRun is a sign-in in its verifying phase, registered as the
// coordinator registers one, whose driver does what driveLogin does once the
// flow has settled: adopt, then end the run.
type adoptionRun struct {
	run    *loginRun
	result chan error
}

func startAdoption(ctx context.Context, t *testing.T, manager *Manager, attempt *loginAttempt, cancel context.CancelFunc) adoptionRun {
	t.Helper()
	run := &loginRun{provider: attempt.provider, attempt: attempt, done: make(chan struct{}), cancel: cancel}
	manager.registerLoginRun(run)
	result := make(chan error, 1)
	go func() {
		defer close(run.done)
		_, err := manager.adoptLogin(ctx, attempt)
		result <- err
		manager.finishLoginRun(run, err, run.teardown())
	}()
	return adoptionRun{run: run, result: result}
}

func (a adoptionRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-a.result:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the sign-in never finished")
		return nil
	}
}

// heldRequest is an HTTP answer that waits until its request's context ends
// and records that it did. Past lingerLimit it answers with answer instead,
// as a request the sign-in's end never reached would be answered.
type heldRequest struct {
	entered   chan struct{}
	enterOnce sync.Once
	mu        sync.Mutex
	sawEnd    bool
	answer    func(*http.Request) (*http.Response, error)
}

func newHeldRequest(answer func(*http.Request) (*http.Response, error)) *heldRequest {
	return &heldRequest{entered: make(chan struct{}), answer: answer}
}

func (h *heldRequest) RoundTrip(req *http.Request) (*http.Response, error) {
	h.enterOnce.Do(func() { close(h.entered) })
	select {
	case <-req.Context().Done():
		h.mu.Lock()
		h.sawEnd = true
		h.mu.Unlock()
		return nil, req.Context().Err()
	case <-time.After(lingerLimit):
		return h.answer(req)
	}
}

func (h *heldRequest) reachedByTheEnd() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sawEnd
}

func (h *heldRequest) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the sign-in never made its request")
	}
}

func claudeSavedNothing(t *testing.T, manager *Manager, sink *recordingAccountSink, fresh []byte) {
	t.Helper()
	claudeName := string(provider.Claude)
	if accounts := manager.store.List(claudeName, time.Now()); len(accounts) != 0 {
		t.Fatalf("accounts after the cancelled sign-in = %+v, want none", accounts)
	}
	if len(sink.signIns) != 0 {
		t.Fatalf("the cancelled sign-in was reported: %+v", sink.signIns)
	}
	if canonical, err := manager.credentials.ReadCredential(claudeName, "", true); err == nil && string(canonical) == string(fresh) {
		t.Fatal("the cancelled sign-in replaced the canonical login")
	}
}

// agyLoginThenProbe is a mock agy whose `acp --login` prints a Google sign-in
// link, takes the pasted code, writes credential into the home it was given
// and exits; every other launch is the account probe.
func agyLoginThenProbe(t *testing.T, credential []byte) string {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, credential, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := agyAccountProbe(t)
	return mockexec.Write(t, filepath.Join(dir, "mock-agy"), `#!/bin/bash
args=("$@")
login=0 geminiDir= appDataDir=
while [ $# -gt 0 ]; do
  case "$1" in
    --login) login=1 ;;
    --gemini-dir) geminiDir=$2; shift ;;
    --app-data-dir) appDataDir=$2; shift ;;
  esac
  shift
done
if [ "$login" = 1 ]; then
  echo "Open https://accounts.google.com/o/oauth2/v2/auth?response_type=code&state=s1&client_id=test&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback"
  read -r _
  cp '`+token+`' "$geminiDir/$appDataDir/antigravity-oauth-token"
  exit 0
fi
exec '`+probe+`' "${args[@]}"
`)
}

// Cancel pressed while an Antigravity sign-in asks Google which account it
// is, driven from Start through the pasted code: the lookup stops at once,
// nothing of the sign-in is saved, and the panel is idle.
func TestCancellingAnAgySignInDuringItsLookupSavesNothing(t *testing.T) {
	dir := t.TempDir()
	rig := newAgyReadingRigAt(t, dir)
	agyName := string(provider.Agy)
	before := agyAccountIDs(rig)
	generation := rig.manager.store.Generation(agyName)
	lookup := newHeldRequest(func(*http.Request) (*http.Response, error) {
		return agyAnswer(http.StatusOK, `{"email":"new@example.com","name":"New"}`), nil
	})
	rig.quota.answerUserinfo(lookup.RoundTrip)
	fresh := agyCredential("new-1", "")
	binary := agyLoginThenProbe(t, fresh)
	rig.manager.deps.ProviderBinary = func(string) string { return binary }

	started, err := rig.manager.StartProviderLogin(agyName, LoginMethodRemote)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != LoginPhaseAwaitingCode {
		t.Fatalf("sign-in state after Start = %+v, want awaiting a code", started)
	}
	if _, err := rig.manager.SubmitProviderLoginCode(agyName, "code-1"); err != nil {
		t.Fatal(err)
	}
	lookup.waitEntered(t)
	if phase := rig.manager.ProviderLoginState(agyName).Phase; phase != LoginPhaseVerifying {
		t.Fatalf("sign-in phase during the lookup = %q, want verifying", phase)
	}
	state := rig.manager.CancelProviderLogin(agyName)

	if !lookup.reachedByTheEnd() {
		t.Fatal("the Google lookup ran on after Cancel")
	}
	if state.Phase != LoginPhaseIdle || rig.manager.ProviderLoginState(agyName).Phase != LoginPhaseIdle {
		t.Fatalf("sign-in state after Cancel = %+v, want idle", rig.manager.ProviderLoginState(agyName))
	}
	if got := agyAccountIDs(rig); len(got) != len(before) || got[0] != before[0] {
		t.Fatalf("accounts after the cancelled sign-in = %v, want %v", got, before)
	}
	if got := reservedIDs(t, dir, agyName); len(got) != 0 {
		t.Fatalf("reservations after the cancelled sign-in = %v, want none", got)
	}
	if got := rig.slot(t, rig.devID); got == string(fresh) {
		t.Fatal("dev's slot took the cancelled sign-in")
	}
	if rig.manager.store.Generation(agyName) != generation {
		t.Fatal("the cancelled sign-in changed the selection")
	}
}

// A sign-in whose time ran out while Google's lookup was waiting reads as a
// sign-in that ran out of time, not as a failed lookup.
//
// The sign-in's context in the app has a deadline; here it is ended with the
// deadline's cause at a chosen moment, so the test does not race a clock.
func TestAnAgySignInThatRunsOutOfTimeWhileVerifyingSaysSo(t *testing.T) {
	rig := newAgyReadingRig(t)
	agyName := string(provider.Agy)
	lookup := newHeldRequest(func(*http.Request) (*http.Response, error) {
		return agyAnswer(http.StatusOK, `{"email":"new@example.com","name":"New"}`), nil
	})
	rig.quota.answerUserinfo(lookup.RoundTrip)

	ctx, end := context.WithCancelCause(context.Background())
	defer end(nil)
	adoption := startAdoption(ctx, t, rig.manager, completedAgyLogin(t, rig, agyCredential("new-1", "")), func() { end(context.Canceled) })
	lookup.waitEntered(t)
	end(context.DeadlineExceeded)

	if err := adoption.wait(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the sign-in ended with %v, want its time limit", err)
	}
	<-adoption.run.done
	state := rig.manager.ProviderLoginState(agyName)
	if state.Phase != LoginPhaseFailed || state.Error != loginTimedOutNotice {
		t.Fatalf("sign-in state = %+v, want failed with %q", state, loginTimedOutNotice)
	}
}

// Cancel pressed while a Claude sign-in asks Anthropic for its profile: the
// request stops at once and nothing of the sign-in is saved or reported.
func TestCancellingAClaudeSignInDuringItsProfileRequestSavesNothing(t *testing.T) {
	manager, _, credentials, sink := signInManager(t)
	profile := newHeldRequest(claudefake.Transport{}.RoundTrip)
	manager.deps.HTTPClient = func() *http.Client { return &http.Client{Transport: profile} }
	fresh := claudeLoginAs("new@example.com", "signed-in", time.Now().Add(30*24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adoption := startAdoption(ctx, t, manager, completedLogin(t, manager, credentials, fresh), cancel)
	profile.waitEntered(t)
	manager.CancelProviderLogin(string(provider.Claude))

	if err := adoption.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled sign-in ended with %v, want it cancelled", err)
	}
	if !profile.reachedByTheEnd() {
		t.Fatal("the profile request ran on after Cancel")
	}
	claudeSavedNothing(t, manager, sink, fresh)
}

// Cancel pressed while a Claude sign-in's account probe is running: the
// probe's process is stopped and nothing of the sign-in is saved.
func TestCancellingAClaudeSignInDuringItsProbeSavesNothing(t *testing.T) {
	manager, _, credentials, sink := signInManager(t)
	started := filepath.Join(t.TempDir(), "started")
	// The probe never answers: it says it started and waits out lingerLimit,
	// so only a stop reaches it in time.
	manager.deps.ProviderBinary = func(string) string {
		return mockexec.Write(t, filepath.Join(t.TempDir(), "mock-claude"),
			"#!/bin/bash\n: > '"+started+"'\nexec sleep 5\n")
	}
	fresh := claudeLoginAs("new@example.com", "signed-in", time.Now().Add(30*24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adoption := startAdoption(ctx, t, manager, completedLogin(t, manager, credentials, fresh), cancel)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never started")
		}
	}
	cancelledAt := time.Now()
	manager.CancelProviderLogin(string(provider.Claude))

	if err := adoption.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled sign-in ended with %v, want it cancelled", err)
	}
	if waited := time.Since(cancelledAt); waited >= lingerLimit/2 {
		t.Fatalf("Cancel waited %s for the probe, want the probe stopped", waited)
	}
	claudeSavedNothing(t, manager, sink, fresh)
}

// Cancel pressed after the sign-in was verified but while it waits for the
// account lock, before the commit point: nothing of it is saved.
func TestCancellingASignInThatWaitsForTheAccountLockSavesNothing(t *testing.T) {
	manager, _, credentials, sink := signInManager(t)
	claudeName := string(provider.Claude)
	fresh := claudeLoginAs("new@example.com", "signed-in", time.Now().Add(30*24*time.Hour))
	attempt := completedLogin(t, manager, credentials, fresh)

	reconcileMu := manager.reconcileMutex(claudeName)
	reconcileMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adoption := startAdoption(ctx, t, manager, attempt, cancel)
	// Adoption removes the temporary home just before it takes the lock.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(attempt.home.Path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			reconcileMu.Unlock()
			t.Fatal("the sign-in never finished verifying")
		}
	}
	cancel()
	reconcileMu.Unlock()

	if err := adoption.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled sign-in ended with %v, want it cancelled", err)
	}
	claudeSavedNothing(t, manager, sink, fresh)
}

// Cancel pressed once the sign-in is past its commit point, while the first
// sign-in ends the running uses of its provider: the sign-in is saved
// whole and reported, and the panel is idle.
func TestACancelAfterTheCommitPointStillSavesAndReportsTheSignIn(t *testing.T) {
	manager, _, credentials, sink := signInManager(t)
	claudeName := string(provider.Claude)
	fresh := claudeLoginAs("new@example.com", "signed-in", time.Now().Add(30*24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan LoginState, 1)
	// The first sign-in ends this use under the account lock; ending it
	// presses Cancel and waits until the sign-in's context has ended.
	use := beginUse(t, manager, func() {
		go func() { cancelled <- manager.CancelProviderLogin(claudeName) }()
		<-ctx.Done()
	})
	adoption := startAdoption(ctx, t, manager, completedLogin(t, manager, credentials, fresh), cancel)

	if err := adoption.wait(t); err != nil {
		t.Fatalf("the sign-in cancelled past its commit point failed: %v", err)
	}
	<-use.exited
	if state := <-cancelled; state.Phase != LoginPhaseIdle {
		t.Fatalf("sign-in state after Cancel = %+v, want idle", state)
	}
	active, ok := manager.store.Active(claudeName, time.Now())
	if !ok || active.Email != "new@example.com" {
		t.Fatalf("selected after the sign-in = %+v, %v; want new@example.com", active, ok)
	}
	if got := readCredential(t, credentials, ""); string(got) != string(fresh) {
		t.Fatal("the canonical login is not the sign-in's")
	}
	if got := readCredential(t, credentials, active.ID); string(got) != string(fresh) {
		t.Fatal("the account's slot is not the sign-in's")
	}
	if len(sink.signIns) != 1 || sink.signIns[0].Account.Email != "new@example.com" {
		t.Fatalf("sign-ins reported = %+v, want the one sign-in", sink.signIns)
	}
}
