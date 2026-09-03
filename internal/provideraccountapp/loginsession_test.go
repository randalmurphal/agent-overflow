package provideraccountapp

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil"
)

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
