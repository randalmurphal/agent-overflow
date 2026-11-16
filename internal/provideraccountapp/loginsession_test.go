package provideraccountapp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil"
)

// loginFrameSink records the sign-in states a manager publishes.
type loginFrameSink struct {
	recordingAccountSink
	mu     sync.Mutex
	frames []LoginState
}

func (s *loginFrameSink) PublishLogin(state LoginState) {
	s.mu.Lock()
	s.frames = append(s.frames, state)
	s.mu.Unlock()
}

func (s *loginFrameSink) newest() LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest LoginState
	for _, frame := range s.frames {
		if frame.Seq > newest.Seq {
			newest = frame
		}
	}
	return newest
}

// registerAwaitingCodeRun installs a Claude run waiting for a pasted code,
// as StartProviderLogin leaves one, without a provider process.
func registerAwaitingCodeRun(t *testing.T, manager *Manager, url string) *loginRun {
	t.Helper()
	run := &loginRun{
		provider:  string(provider.Claude),
		done:      make(chan struct{}),
		pastes:    make(chan loginPaste, 1),
		startedAt: time.Now(),
		deadline:  time.Now().Add(time.Minute),
		cancel:    func() {},
	}
	manager.registerLoginRun(run)
	manager.publishLoginFlow(run, loginFlow{
		method: LoginMethodRemote, phase: LoginPhaseAwaitingCode, authorizeURL: url,
	}, "")
	return run
}

// A rejected code replaces the link at once. The run's answer to the paste
// must be stored after "verifying", and so carry the higher Seq, however
// quickly it comes: otherwise the backend and every client end on
// "verifying" over the burned link, with no code box.
func TestARunsAnswerToAPastedCodeIsNewerThanVerifying(t *testing.T) {
	sink := &loginFrameSink{}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper(), Accounts: sink})
	for range 200 {
		run := registerAwaitingCodeRun(t, manager, "https://example.test/first")
		seen := make(chan LoginPhase, 1)
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			<-run.pastes
			// What the run reads the moment it holds the code.
			seen <- manager.ProviderLoginState(string(provider.Claude)).Phase
			manager.publishLoginFlow(run, loginFlow{
				method: LoginMethodRemote, phase: LoginPhaseAwaitingCode, authorizeURL: "https://example.test/second",
			}, loginBurnedNotice)
		}()
		verifying, err := manager.SubmitProviderLoginCode(string(provider.Claude), "code#state")
		if err != nil {
			t.Fatal(err)
		}
		if phase := <-seen; phase != LoginPhaseVerifying {
			t.Fatalf("the run held the code while the stored phase was %q, want verifying", phase)
		}
		<-answered
		replaced := manager.ProviderLoginState(string(provider.Claude))
		if replaced.AuthorizeURL != "https://example.test/second" || replaced.Phase != LoginPhaseAwaitingCode {
			t.Fatalf("stored state = %+v, want the replacement link awaiting a code", replaced)
		}
		if verifying.Seq == 0 || verifying.Seq >= replaced.Seq {
			t.Fatalf("verifying Seq %d must precede the replacement's %d", verifying.Seq, replaced.Seq)
		}
		if newest := sink.newest(); newest.Seq != replaced.Seq || newest.AuthorizeURL != replaced.AuthorizeURL {
			t.Fatalf("newest published frame = %+v, want the stored state %+v", newest, replaced)
		}
		manager.settleLoginRun(run, LoginPhaseFailed, "")
	}
}

// Every stored state takes the next Seq, and a call answers with it.
func TestSignInStatesAreNumberedInTheOrderTheyAreStored(t *testing.T) {
	sink := &loginFrameSink{}
	manager := NewManager(Deps{Reaper: procutiltest.Reaper(), Accounts: sink})
	run := registerAwaitingCodeRun(t, manager, "https://example.test/first")
	awaiting := manager.ProviderLoginState(string(provider.Claude))
	verifying, err := manager.SubmitProviderLoginCode(string(provider.Claude), "code#state")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SubmitProviderLoginCode(string(provider.Claude), "code#state"); !errors.Is(err, errLoginCodeBusy) {
		t.Fatalf("second paste err = %v, want busy", err)
	}
	manager.settleLoginRun(run, LoginPhaseFailed, "")
	failed := manager.ProviderLoginState(string(provider.Claude))
	idle := manager.CancelProviderLogin(string(provider.Claude))
	if awaiting.Seq >= verifying.Seq || verifying.Seq >= failed.Seq || failed.Seq >= idle.Seq {
		t.Fatalf("Seq order awaiting %d, verifying %d, failed %d, idle %d", awaiting.Seq, verifying.Seq, failed.Seq, idle.Seq)
	}
	if newest := sink.newest(); newest.Seq != idle.Seq || newest.Phase != LoginPhaseIdle {
		t.Fatalf("newest frame = %+v, want idle", newest)
	}
}

// A cancel that lands while StartProviderLogin is still opening its first
// flow must return. The run sits in the registry for the whole spawn, so a
// concurrent CancelProviderLogin — or the shutdown step that joins every
// login — grabs it and parks on run.done; when the open then fails (it will,
// its context was just cancelled), driveLogin never runs, and Start's own
// error path is the only thing left that can close the channel. This test
// held CancelProviderLogin forever before that close existed.
func TestCancelDuringLoginStartUnblocks(t *testing.T) {
	manager, store, _ := newTestManager(t)
	// A mock script with no scripted responses reads stdin and answers
	// nothing, so Authenticate blocks on its context — which keeps
	// StartProviderLogin inside openLoginFlow for as long as the test needs.
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), nil)
	manager.deps.ProviderBinary = func(string) string { return binary }
	// An active account makes beginLogin skip the canonical-home probe, which
	// would otherwise spawn the same silent script and stall before the part
	// under test.
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "seeded", Provider: string(provider.Claude), Email: "seeded@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	go func() {
		defer close(started)
		// The remote method never touches a browser opener; the call blocks
		// in Authenticate until the cancel below ends its context.
		_, _ = manager.StartProviderLogin(string(provider.Claude), LoginMethodRemote)
	}()

	// Wait until the run is registered, so the cancel is guaranteed to grab
	// this run rather than find nothing. Wall-clock bounded: a wait that
	// gives up must fail, never return.
	deadline := time.Now().Add(10 * time.Second)
	for {
		manager.loginMu.Lock()
		registered := manager.logins[string(provider.Claude)] != nil
		manager.loginMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("login run was never registered")
		}
		time.Sleep(time.Millisecond)
	}

	cancelled := make(chan LoginState, 1)
	go func() {
		cancelled <- manager.CancelProviderLogin(string(provider.Claude))
	}()

	select {
	case state := <-cancelled:
		if state.Phase != LoginPhaseIdle {
			t.Fatalf("cancel left phase %q, want idle", state.Phase)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("CancelProviderLogin never returned: run.done was not closed on the start-error path")
	}

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("StartProviderLogin never returned after its context was cancelled")
	}
}
