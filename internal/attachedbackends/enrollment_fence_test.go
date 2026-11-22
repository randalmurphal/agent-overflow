//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/filelock"
)

// otherManager is a second manager over dir, as the frontend-only desktop
// runs beside the host's.
func otherManager(t *testing.T, dir string) *Manager {
	t.Helper()
	other, err := New(dir, "this-machine", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

// holdSessionFile takes the OS lock on id's session file, the fence every
// manager over dir takes to decide on and write that file, and returns its
// release.
func holdSessionFile(t *testing.T, dir, id string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, err := filelock.Acquire(ctx, filepath.Join(dir, "locks", id+".json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if err := filelock.Release(held); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// waitForEnrollmentState waits until id's enrollment in flight is in want.
func waitForEnrollmentState(t *testing.T, m *Manager, id string, want enrollmentState) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if state, _ := enrollmentOf(m, id); state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("enrollment never reached state %d", want)
		}
	}
}

// writeAsOtherManager writes id's session file as another manager does
// while it holds the session file's lock, which the caller holds.
func writeAsOtherManager(t *testing.T, dir string, session deviceclient.Session) {
	t.Helper()
	if err := atomicfile.WriteJSON(filepath.Join(dir, deviceclient.SessionsDirName, session.BackendID+".json"), session); err != nil {
		t.Fatal(err)
	}
}

// Another manager over the same profile directory forgets the computer, or
// saves an own-device pairing with it, after this manager's introduction
// was checked and before it is saved. The save decides under the session
// file's lock and keeps what the other manager wrote.
func TestAnotherManagersWriteBetweenTheCheckAndTheSaveIsKept(t *testing.T) {
	t.Parallel()
	for _, write := range []string{"forget", "own-device pairing"} {
		t.Run(write, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			other := otherManager(t, dir)
			release := holdSessionFile(t, dir, p.backendID)
			answer := p.introduce(context.Background(), m)
			// Checked, redeemed and committed: the save waits on the lock.
			waitForEnrollmentState(t, m, p.backendID, enrollmentCommitted)
			var theirs deviceclient.Session
			if write == "forget" {
				// The other manager's Forget writes the exclusion, then
				// deletes the session, which there is none of yet.
				if err := other.excludeOwnDevice(p.backendID, true); err != nil {
					t.Fatal(err)
				}
			} else {
				theirs = deviceclient.Session{OwnDevice: true, BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
					SessionID: "other-manager", Credential: "c", RefreshSecret: "r", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}
				writeAsOtherManager(t, dir, theirs)
			}
			release()
			if got := within(t, "the introduction", 5*time.Second, answer); got.added || got.err != nil {
				t.Fatalf("introduction overtaken by the other manager = added %t, %v; want not added with no error", got.added, got.err)
			}
			saved, err := deviceclient.LoadSession(dir, p.backendID)
			if write == "forget" {
				if !errors.Is(err, deviceclient.ErrNoSession) {
					t.Fatalf("forgotten computer after the introduction = %+v, %v; want nothing stored", saved, err)
				}
				return
			}
			if err != nil || saved.SessionID != theirs.SessionID {
				t.Fatalf("stored after the introduction = %+v, %v; want the other manager's pairing", saved, err)
			}
		})
	}
}

// Forget drops whatever pairing is stored, also one another manager saved
// after this manager's carrier was built from the older one.
func TestForgetDropsAPairingAnotherManagerSaved(t *testing.T) {
	t.Parallel()
	m, dir := newManager(t)
	const id = "0f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c11"
	older := deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "older", Credential: "c1"}
	seed(t, dir, older)
	held, err := m.carrier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	newer := older
	newer.SessionID, newer.Credential = "newer", "c2"
	seed(t, dir, newer)
	if err := m.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := deviceclient.LoadSession(dir, id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("stored after Forget = %v; want nothing", err)
	}
	if !held.client.Retired() {
		t.Fatal("the carrier built from the older pairing was not retired")
	}
}

// A tombstone forgets only the session it names. One another manager saved
// after the tombstone's check is kept, with its agent opt-in.
func TestATombstoneKeepsAPairingAnotherManagerSavedAfterItsCheck(t *testing.T) {
	t.Parallel()
	m, dir := newManager(t)
	const id = "0f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c11"
	tombstoned := deviceclient.Session{OwnDevice: true, BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "tombstoned", Credential: "c1"}
	seed(t, dir, tombstoned)
	if err := m.SetAgentAccess(id, true); err != nil {
		t.Fatal(err)
	}
	release := holdSessionFile(t, dir, id)
	type answer struct {
		dropped bool
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		dropped, err := m.retireOwnProfile(id, tombstoned.SessionID)
		done <- answer{dropped, err}
	}()
	// Checked and past the agent opt-in: the forget waits on the lock.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if access, err := m.AgentAccess(); err == nil && !access[id] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tombstone never reached its forget")
		}
	}
	newer := tombstoned
	newer.SessionID, newer.Credential = "newer", "c2"
	writeAsOtherManager(t, dir, newer)
	release()
	if got := within(t, "the tombstone", 5*time.Second, done); got.dropped || got.err != nil {
		t.Fatalf("tombstone after another manager's save = dropped %t, %v; want nothing dropped", got.dropped, got.err)
	}
	if saved, err := deviceclient.LoadSession(dir, id); err != nil || saved.SessionID != "newer" {
		t.Fatalf("stored after the tombstone = %+v, %v; want the other manager's pairing", saved, err)
	}
	if access, err := m.AgentAccess(); err != nil || !access[id] {
		t.Fatalf("agent access after the tombstone kept the pairing = %v, %v; want it given back", access, err)
	}
}
