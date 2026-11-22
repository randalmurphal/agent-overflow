package provideraccountapp

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// beginRefreshingUse admits a credential use under the provider's current
// generation that stands in for a CLI which refreshed its login: once ended,
// it runs onEnd, writes rotated into the canonical store and then releases,
// as the CLI writes its refreshed token on the way out. finish stands in for
// the process exiting on its own, for a change that never ended it, and
// returns once the use has released.
func beginRefreshingUse(t *testing.T, manager *Manager, rotated []byte, onEnd func()) (finish func()) {
	t.Helper()
	claudeName := string(provider.Claude)
	ended := make(chan struct{})
	var endOnce sync.Once
	end := func() { endOnce.Do(func() { close(ended) }) }
	release, err := manager.BeginCredentialUse(t.Context(), claudeName, manager.Generation(claudeName), end)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		<-ended
		onEnd()
		if err := manager.credentials.WriteNativeCredentialForTest(claudeName, rotated); err != nil {
			t.Error(err)
		}
		release()
		release()
		close(exited)
	}()
	return func() {
		end()
		<-exited
	}
}

type admission struct {
	release func()
	err     error
}

// askAdmission asks, from its own goroutine, to admit a use under generation.
func askAdmission(ctx context.Context, manager *Manager, generation uint64) <-chan admission {
	answer := make(chan admission, 1)
	go func() {
		release, err := manager.BeginCredentialUse(ctx, string(provider.Claude), generation, func() {})
		answer <- admission{release, err}
	}()
	return answer
}

// selectionHook is a SessionGateway that runs its function where a change
// applies the new selection, inside the change.
type selectionHook func()

func (h selectionHook) ApplySelection(string, string, Selection) { h() }

// Each path that replaces the canonical credential ends the running uses and
// waits for them to exit before it reads that credential, so the login a CLI
// refreshed on its way out is reconciled as the account it belongs to and
// never lands over the incoming one. A use asked for while the path ends them
// waits for the change, and then reads the generation the change left.
func TestAnAccountChangeEndsRunningUsesBeforeItReadsTheCredential(t *testing.T) {
	claudeName := string(provider.Claude)
	rotated := claudeLoginAs("current@example.com", "rotated", time.Now().Add(20*24*time.Hour))
	fresh := claudeLoginAs("current@example.com", "signed-in", time.Now().Add(30*24*time.Hour))
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, manager *Manager) error
		// installed is the credential the change leaves in the canonical
		// store.
		installed func(t *testing.T, manager *Manager) []byte
		// preserves is true when the change keeps the outgoing login in the
		// current account's slot.
		preserves bool
	}{
		{
			name: "switch",
			change: func(_ *testing.T, manager *Manager) error {
				_, err := manager.SwitchProviderAccount(claudeName, "other")
				return err
			},
			installed: func(t *testing.T, manager *Manager) []byte {
				return readCredential(t, manager.credentials, "other")
			},
			preserves: true,
		},
		{
			name: "removing the selected account",
			change: func(_ *testing.T, manager *Manager) error {
				return manager.RemoveProviderAccount(claudeName, "current")
			},
			installed: func(t *testing.T, manager *Manager) []byte {
				return readCredential(t, manager.credentials, "other")
			},
		},
		{
			name: "signing the selected account in again",
			change: func(t *testing.T, manager *Manager) error {
				_, err := manager.adoptLogin(completedLogin(t, manager, manager.credentials, fresh))
				return err
			},
			installed: func(*testing.T, *Manager) []byte { return fresh },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, _ := selectionRig(t)
			before := manager.Generation(claudeName)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			asked := make(chan (<-chan admission), 1)
			finish := beginRefreshingUse(t, manager, rotated, func() {
				asked <- askAdmission(ctx, manager, before)
			})
			if err := tc.change(t, manager); err != nil {
				t.Fatal(err)
			}
			finish()
			if got := readCredential(t, manager.credentials, ""); !bytes.Equal(got, tc.installed(t, manager)) {
				t.Fatalf("canonical credential = %s, want the one the change installed", got)
			}
			if tc.preserves {
				if got := readCredential(t, manager.credentials, "current"); !bytes.Equal(got, rotated) {
					t.Fatalf("current's slot = %s, want the login the CLI refreshed", got)
				}
			}
			answer := <-<-asked
			if !errors.Is(answer.err, ErrSelectionMoved) {
				t.Fatalf("a use asked for while the change ended the running ones = %v, want it to read the moved generation", answer.err)
			}
		})
	}
}

// A use asked for after the switch has ended the running uses, and before the
// switch is done, is answered only once the switch is done, and then reads
// the generation again: one asked under the old generation is refused, one
// under the new is admitted.
func TestAUseAskedForLateInASwitchWaitsForTheSwitchToEnd(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := selectionRig(t)
	before := manager.Generation(claudeName)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var stale, current <-chan admission
	answeredInside := false
	manager.deps.Sessions = selectionHook(func() {
		stale = askAdmission(ctx, manager, before)
		current = askAdmission(ctx, manager, manager.store.Generation(claudeName))
		// A slow machine only makes this check weaker: it cannot fail it.
		select {
		case <-stale:
			answeredInside = true
		case <-current:
			answeredInside = true
		case <-time.After(200 * time.Millisecond):
		}
	})
	if _, err := manager.SwitchProviderAccount(claudeName, "other"); err != nil {
		t.Fatal(err)
	}
	if answeredInside {
		t.Fatal("a use was answered while the switch was still changing the credential")
	}
	if answer := <-stale; !errors.Is(answer.err, ErrSelectionMoved) {
		t.Fatalf("the use asked for under the old generation = %v, want ErrSelectionMoved", answer.err)
	}
	answer := <-current
	if answer.err != nil {
		t.Fatalf("the use asked for under the new generation = %v, want it admitted", answer.err)
	}
	answer.release()
}

// A use waiting for a change stops waiting when its context ends.
func TestAUseStopsWaitingForAChangeWhenItsContextEnds(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := newTestManager(t)
	endChange := manager.lockCredentialChange(claudeName)
	defer endChange()
	ctx, cancel := context.WithCancel(t.Context())
	answer := askAdmission(ctx, manager, manager.Generation(claudeName))
	cancel()
	select {
	case got := <-answer:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("BeginCredentialUse = %v, want context.Canceled", got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the use kept waiting after its context ended")
	}
}
