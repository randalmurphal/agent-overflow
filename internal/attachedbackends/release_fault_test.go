//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// errReleaseRefused stands for locks that fail to release after the
// transition ran (Manager.releaseFault).
var errReleaseRefused = errors.New("release refused")

// announcements records m's set changes.
type announcements struct {
	mu      sync.Mutex
	changes []SetChange
}

func announced(m *Manager) *announcements {
	a := &announcements{}
	m.SetChanged(func(change SetChange) {
		a.mu.Lock()
		a.changes = append(a.changes, change)
		a.mu.Unlock()
	})
	return a
}

func (a *announcements) count(action SetAction, reason RemovalReason) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, change := range a.changes {
		if change.Action == action && change.Reason == reason {
			n++
		}
	}
	return n
}

// managerWhoseLocksFailToRelease is a manager every transaction of which
// commits and then answers errReleaseRefused, with one stored session
// cached for id.
func managerWhoseLocksFailToRelease(t *testing.T, id string) (*Manager, string, *carrier) {
	t.Helper()
	m, dir := newManager(t)
	m.releaseFault = errReleaseRefused
	seed(t, dir, deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1"})
	held, err := m.carrier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir, held
}

// A transaction that committed and whose locks then failed to release is
// two answers: its changes stand, in the files and in this process's cache,
// and are announced, and the release's error still reaches the caller.
func TestACommittedTransitionWhoseLocksFailToReleaseStands(t *testing.T) {
	t.Parallel()
	const id = "0f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c11"
	t.Run("agent access", func(t *testing.T) {
		t.Parallel()
		m, _, _ := managerWhoseLocksFailToRelease(t, id)
		if err := m.SetAgentAccess(id, true); !errors.Is(err, errReleaseRefused) {
			t.Fatalf("SetAgentAccess = %v; want the release's error", err)
		}
		if access, err := m.AgentAccess(); err != nil || !access[id] {
			t.Fatalf("agent access = %v, %v; want the opt-in on", access, err)
		}
	})
	t.Run("removal", func(t *testing.T) {
		t.Parallel()
		m, dir, held := managerWhoseLocksFailToRelease(t, id)
		changes := announced(m)
		if err := m.Remove(id); !errors.Is(err, errReleaseRefused) {
			t.Fatalf("Remove = %v; want the release's error", err)
		}
		if _, err := deviceclient.LoadSession(dir, id); !errors.Is(err, deviceclient.ErrNoSession) {
			t.Fatalf("stored after the removal = %v; want nothing", err)
		}
		if cachedCarrier(m, id) != nil || !held.client.Retired() {
			t.Fatal("the removed computer's carrier is still cached or live")
		}
		if n := changes.count(SetRemoved, ""); n != 1 {
			t.Fatalf("%d removals announced; want 1", n)
		}
	})
	t.Run("verdict", func(t *testing.T) {
		t.Parallel()
		m, dir, held := managerWhoseLocksFailToRelease(t, id)
		if err := deviceclient.ForgetSession(dir, id); err != nil {
			t.Fatal(err)
		}
		changes := announced(m)
		m.endSession(id, held)
		if cachedCarrier(m, id) != nil || !held.client.Retired() {
			t.Fatal("the ended carrier is still cached or live")
		}
		if _, ended := m.TakeVerdict(id); !ended {
			t.Fatal("no end was recorded")
		}
		if n := changes.count(SetRemoved, RemovedByComputer); n != 1 {
			t.Fatalf("%d ends announced; want 1", n)
		}
	})
	t.Run("person's pairing", func(t *testing.T) {
		t.Parallel()
		p := newIntroducedPeer(t, nil)
		m, _ := newManager(t)
		m.releaseFault = errReleaseRefused
		if got := within(t, "the pairing", 5*time.Second, p.pair(m)); !errors.Is(got.err, errReleaseRefused) {
			t.Fatalf("pairing = %v; want the release's error", got.err)
		}
		if built := enrolled(t, m, p.backendID); built.onEnded == nil {
			t.Fatal("the saved pairing's carrier is not renewing")
		}
	})
	t.Run("introduction", func(t *testing.T) {
		t.Parallel()
		p := newIntroducedPeer(t, nil)
		m, _ := newManager(t)
		m.releaseFault = errReleaseRefused
		changes := announced(m)
		got := within(t, "the introduction", 5*time.Second, p.introduce(context.Background(), m))
		if !got.added || !errors.Is(got.err, errReleaseRefused) {
			t.Fatalf("introduction = added %t, %v; want added and the release's error", got.added, got.err)
		}
		if built := enrolled(t, m, p.backendID); built.onEnded == nil {
			t.Fatal("the saved introduction's carrier is not renewing")
		}
		if n := changes.count(SetMembership, ""); n == 0 {
			t.Fatal("the saved introduction was not announced")
		}
	})
}
