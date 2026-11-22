package provideraccountapp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// trackedUse is a credential use standing in for a running CLI.
type trackedUse struct {
	// ended closes when a change ends the use; selectedAtEnd is the account
	// selected at that moment, read before the use releases.
	ended         chan struct{}
	selectedAtEnd string
	release       func()
	exited        chan struct{}
	endOnce       sync.Once
}

// beginUse admits a use of the Claude credential under the current
// generation. Once ended it records the selection, runs onEnd (a CLI writing
// its refreshed login as it exits) and releases.
func beginUse(t *testing.T, manager *Manager, onEnd func()) *trackedUse {
	t.Helper()
	claudeName := string(provider.Claude)
	use := &trackedUse{ended: make(chan struct{}), exited: make(chan struct{})}
	release, err := manager.BeginCredentialUse(t.Context(), claudeName, manager.Generation(claudeName), func() {
		use.endOnce.Do(func() { close(use.ended) })
	})
	if err != nil {
		t.Fatal(err)
	}
	use.release = release
	go func() {
		<-use.ended
		if active, ok := manager.store.Active(claudeName, time.Now()); ok {
			use.selectedAtEnd = active.ID
		}
		if onEnd != nil {
			onEnd()
		}
		// Exited before the release, as a process is: the release is what
		// lets the change go on, so the change never returns before this.
		close(use.exited)
		release()
		release()
	}()
	return use
}

// wantEndedBeforeTheChange fails unless the use was ended while from was
// still selected.
func (u *trackedUse) wantEndedBeforeTheChange(t *testing.T, from string) {
	t.Helper()
	select {
	case <-u.exited:
	default:
		t.Fatal("the change returned with the use still running")
	}
	if u.selectedAtEnd != from {
		t.Fatalf("the use ended with %q selected, want it ended before the selection left %q", u.selectedAtEnd, from)
	}
}

// wantStillRunning fails if the change ended the use, then releases it.
func (u *trackedUse) wantStillRunning(t *testing.T) {
	t.Helper()
	select {
	case <-u.ended:
		t.Fatal("a change that kept the selected account ended a running use")
	default:
	}
	u.release()
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

// emptyClaudeRig is signInManager with nothing selected and the canonical
// store holding a login of first@example.com.
func emptyClaudeRig(t *testing.T) *Manager {
	t.Helper()
	manager, _, credentials, _ := signInManager(t)
	if err := credentials.WriteNativeCredentialForTest(string(provider.Claude),
		claudeLoginAs("first@example.com", "first", time.Now().Add(30*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	return manager
}

func outsideLogin(t *testing.T, manager *Manager, email, nonce string) {
	t.Helper()
	if err := manager.credentials.WriteNativeCredentialForTest(string(provider.Claude),
		claudeLoginAs(email, nonce, time.Now().Add(30*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
}

func probeCanonical(t *testing.T, manager *Manager) {
	t.Helper()
	claudeName := string(provider.Claude)
	if _, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: claudeName,
		Probe:        manager.canonicalProviderAccountProbe(claudeName, manager.providerBinaryPath(claudeName)),
	}); err != nil {
		t.Fatal(err)
	}
}

// Every path that moves a provider's selection to another account (or away
// from the last one) ends the running uses before it moves it.
func TestEveryChangeOfTheSelectedAccountEndsTheRunningUsesFirst(t *testing.T) {
	claudeName := string(provider.Claude)
	fresh := claudeLoginAs("first@example.com", "signed-in", time.Now().Add(30*24*time.Hour))
	for _, tc := range []struct {
		name string
		rig  func(t *testing.T) *Manager
		// from is the account selected before the change.
		from   string
		change func(t *testing.T, manager *Manager)
	}{
		{"removing the selected account when it is the last", func(t *testing.T) *Manager {
			manager, _, _ := selectionRig(t)
			if err := manager.RemoveProviderAccount(claudeName, "other"); err != nil {
				t.Fatal(err)
			}
			return manager
		}, "current", func(t *testing.T, manager *Manager) {
			if err := manager.RemoveProviderAccount(claudeName, "current"); err != nil {
				t.Fatal(err)
			}
		}},
		{"the first sign-in", emptyClaudeRig, "", func(t *testing.T, manager *Manager) {
			if _, err := manager.adoptLogin(completedLogin(t, manager, manager.credentials, fresh)); err != nil {
				t.Fatal(err)
			}
		}},
		{"an outside login", func(t *testing.T) *Manager {
			manager, _, _ := selectionRig(t)
			outsideLogin(t, manager, "other@example.com", "external")
			return manager
		}, "current", func(t *testing.T, manager *Manager) {
			if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
				t.Fatal(err)
			}
		}},
		{"a probe that finds an outside login", func(t *testing.T) *Manager {
			manager, _, _ := selectionRig(t)
			outsideLogin(t, manager, "other@example.com", "external")
			return manager
		}, "current", probeCanonical},
		{"the first sign-in with no login on disk", func(t *testing.T) *Manager {
			manager, _, _, _ := signInManager(t)
			return manager
		}, "", func(t *testing.T, manager *Manager) {
			if _, err := manager.adoptLogin(completedLogin(t, manager, manager.credentials, fresh)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a probe with nothing selected", emptyClaudeRig, "", probeCanonical},
		{"a sign-in starting with nothing selected", emptyClaudeRig, "", func(t *testing.T, manager *Manager) {
			attempt, err := manager.beginLogin(claudeName)
			if err != nil {
				t.Fatal(err)
			}
			if err := attempt.cleanup(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := tc.rig(t)
			use := beginUse(t, manager, nil)
			before, _ := manager.store.Active(claudeName, time.Now())
			tc.change(t, manager)
			after, _ := manager.store.Active(claudeName, time.Now())
			if after.ID == before.ID {
				t.Fatalf("the selection stayed %q; the case does not change it", after.ID)
			}
			use.wantEndedBeforeTheChange(t, tc.from)
		})
	}
}

// A probe that adopts a Codex login whose credential names no email, with
// nothing selected, ends the running Codex uses before it selects the login.
func TestAProbeAdoptingALoginWithoutAnEmailEndsTheRunningUsesFirst(t *testing.T) {
	codexName := string(provider.Codex)
	manager, store, credentials := newTestManager(t)
	if err := credentials.WriteNativeCredentialForTest(codexName,
		[]byte(`{"tokens":{"access_token":"anonymous"}}`)); err != nil {
		t.Fatal(err)
	}
	ended := make(chan string, 1)
	var release func()
	release, err := manager.BeginCredentialUse(t.Context(), codexName, manager.Generation(codexName), func() {
		active, _ := store.Active(codexName, time.Now())
		ended <- active.ID
		go release()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: codexName,
		Probe: func(context.Context) (provider.AccountInfo, error) {
			return provider.AccountInfo{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if active, ok := store.Active(codexName, time.Now()); !ok || active.ID == "" {
		t.Fatal("the probe selected nothing; the case does not change the selection")
	}
	select {
	case selected := <-ended:
		if selected != "" {
			t.Fatalf("the use ended with %q selected, want it ended before the probe selected the login", selected)
		}
	default:
		t.Fatal("the probe selected the login with the use still running")
	}
}

// A path that keeps the selected account leaves the running uses alone.
func TestAChangeThatKeepsTheSelectedAccountLeavesTheRunningUses(t *testing.T) {
	claudeName := string(provider.Claude)
	soon := time.Now().Add(30 * 24 * time.Hour)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, manager *Manager)
	}{
		{"removing an inactive account", func(t *testing.T, manager *Manager) {
			if err := manager.RemoveProviderAccount(claudeName, "other"); err != nil {
				t.Fatal(err)
			}
		}},
		{"signing in to an inactive account", func(t *testing.T, manager *Manager) {
			login := claudeLoginAs("other@example.com", "other-new", soon)
			if _, err := manager.adoptLogin(completedLogin(t, manager, manager.credentials, login)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a switch to the selected account", func(t *testing.T, manager *Manager) {
			if _, err := manager.SwitchProviderAccount(claudeName, "current"); err != nil {
				t.Fatal(err)
			}
		}},
		{"signing the selected account in again", func(t *testing.T, manager *Manager) {
			login := claudeLoginAs("current@example.com", "current-new", soon)
			if _, err := manager.adoptLogin(completedLogin(t, manager, manager.credentials, login)); err != nil {
				t.Fatal(err)
			}
		}},
		{"an outside rotation of the selected login", func(t *testing.T, manager *Manager) {
			outsideLogin(t, manager, "current@example.com", "rotated")
			if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
				t.Fatal(err)
			}
		}},
		{"a probe that finds the selected login rotated", func(t *testing.T, manager *Manager) {
			outsideLogin(t, manager, "current@example.com", "rotated")
			probeCanonical(t, manager)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, _ := selectionRig(t)
			use := beginUse(t, manager, nil)
			tc.change(t, manager)
			if active, _ := manager.store.Active(claudeName, time.Now()); active.ID != "current" {
				t.Fatalf("selected = %q, want current kept", active.ID)
			}
			use.wantStillRunning(t)
		})
	}
}

// A switch that is refused or declined changes nothing, so it ends no running
// use and leaves the generation where it was.
func TestARefusedSwitchEndsNoRunningUse(t *testing.T) {
	claudeName := string(provider.Claude)
	otherSlot := func(t *testing.T, manager *Manager) string {
		t.Helper()
		path, err := manager.credentials.AccountCredentialPath(claudeName, "other")
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, manager *Manager)
		target  string
		refused bool
	}{
		{"the target is not saved", func(*testing.T, *Manager) {}, "nobody", true},
		{"the target's credential is missing", func(t *testing.T, manager *Manager) {
			if err := os.Remove(otherSlot(t, manager)); err != nil {
				t.Fatal(err)
			}
		}, "other", true},
		{"the target is signed out", func(t *testing.T, manager *Manager) {
			husk := []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
			if err := os.WriteFile(otherSlot(t, manager), husk, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "other", true},
		{"the target's login expired", func(t *testing.T, manager *Manager) {
			expired := claudeLoginAs("other@example.com", "other-old", time.Now().Add(-time.Hour))
			if err := manager.credentials.WriteAccountCredential(claudeName, "other", expired); err != nil {
				t.Fatal(err)
			}
		}, "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, _ := selectionRig(t)
			tc.prepare(t, manager)
			use := beginUse(t, manager, nil)
			before := manager.Generation(claudeName)
			result, err := manager.SwitchProviderAccount(claudeName, tc.target)
			if tc.refused && err == nil {
				t.Fatalf("SwitchProviderAccount = %+v, want it refused", result)
			}
			if !tc.refused && (err != nil || !result.SignInRequired) {
				t.Fatalf("SwitchProviderAccount = %+v, %v; want it declined for a sign-in", result, err)
			}
			if active, _ := manager.store.Active(claudeName, time.Now()); active.ID != "current" {
				t.Fatalf("selected = %q, want current kept", active.ID)
			}
			if after := manager.Generation(claudeName); after != before {
				t.Fatalf("generation = %d, want %d unchanged", after, before)
			}
			use.wantStillRunning(t)
		})
	}
}

// A switch and a removal of the selected account end the running uses and
// wait for them to exit before they read the canonical credential, so the
// login a CLI refreshed on its way out is reconciled as the account it
// belongs to and never lands over the incoming one. A use asked for while the
// path ends them waits for the change, and then reads the generation the
// change left.
func TestASwitchReadsTheCredentialOnlyOnceTheRunningUsesHaveExited(t *testing.T) {
	claudeName := string(provider.Claude)
	rotated := claudeLoginAs("current@example.com", "rotated", time.Now().Add(20*24*time.Hour))
	for _, tc := range []struct {
		name   string
		change func(manager *Manager) error
		// preserves is true when the change keeps the outgoing login in the
		// current account's slot.
		preserves bool
	}{
		{"switch", func(manager *Manager) error {
			_, err := manager.SwitchProviderAccount(claudeName, "other")
			return err
		}, true},
		{"removing the selected account", func(manager *Manager) error {
			return manager.RemoveProviderAccount(claudeName, "current")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, _ := selectionRig(t)
			incoming := readCredential(t, manager.credentials, "other")
			before := manager.Generation(claudeName)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			asked := make(chan (<-chan admission), 1)
			use := beginUse(t, manager, func() {
				asked <- askAdmission(ctx, manager, before)
				if err := manager.credentials.WriteNativeCredentialForTest(claudeName, rotated); err != nil {
					t.Error(err)
				}
			})
			if err := tc.change(manager); err != nil {
				t.Fatal(err)
			}
			use.wantEndedBeforeTheChange(t, "current")
			if got := readCredential(t, manager.credentials, ""); !bytes.Equal(got, incoming) {
				t.Fatalf("canonical credential = %s, want the incoming account's", got)
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

// A switch that finds another account's login outside the app changes the
// selection twice in one hold of the reconcile lock; a use asked for during
// the first drain is answered when the switch ends, not left waiting on it.
func TestAUseAskedForDuringASwitchThatAlsoAdoptsAnOutsideLoginIsAnswered(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := selectionRig(t)
	outsideLogin(t, manager, "other@example.com", "external")
	before := manager.Generation(claudeName)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	asked := make(chan (<-chan admission), 1)
	use := beginUse(t, manager, func() { asked <- askAdmission(ctx, manager, before) })
	if _, err := manager.SwitchProviderAccount(claudeName, "other"); err != nil {
		t.Fatal(err)
	}
	use.wantEndedBeforeTheChange(t, "current")
	if answer := <-<-asked; !errors.Is(answer.err, ErrSelectionMoved) {
		t.Fatalf("the use asked for during the switch = %v, want ErrSelectionMoved once the switch ended", answer.err)
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
		t.Fatal("a use was answered while the switch was still changing the selection")
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

// A use is admitted while the manager's own lock is held: the reconciliation
// of an outside login ends the running uses holding it, and a use registered
// just before must reach its release rather than wait on that lock.
func TestAUseIsAdmittedWhileTheManagerLockIsHeld(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := selectionRig(t)
	manager.mu.Lock()
	answer := askAdmission(t.Context(), manager, manager.store.Generation(claudeName))
	select {
	case got := <-answer:
		manager.mu.Unlock()
		if got.err != nil {
			t.Fatalf("BeginCredentialUse = %v, want it admitted", got.err)
		}
		got.release()
	case <-time.After(10 * time.Second):
		manager.mu.Unlock()
		<-answer
		t.Fatal("the admission waited on the manager's lock")
	}
}

// A use waiting for a change stops waiting when its context ends.
func TestAUseStopsWaitingForAChangeWhenItsContextEnds(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := newTestManager(t)
	reconcileMu := manager.reconcileMutex(claudeName)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	manager.selectionChanging(claudeName)
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

// The store refuses to move a provider's selection while one of its uses is
// running: a path that changes it without ending them fails instead.
func TestTheStoreRefusesToMoveTheSelectionUnderARunningUse(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, _ := selectionRig(t)
	use := beginUse(t, manager, nil)
	if _, err := manager.store.Activate(claudeName, "other"); err == nil {
		t.Fatal("the store moved the selection with a use running")
	}
	if active, _ := manager.store.Active(claudeName, time.Now()); active.ID != "current" {
		t.Fatalf("selected = %q after the refusal, want current", active.ID)
	}
	use.wantStillRunning(t)
	if _, err := manager.store.Activate(claudeName, "other"); err != nil {
		t.Fatalf("Activate with no use running: %v", err)
	}
}
