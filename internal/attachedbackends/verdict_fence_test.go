//go:build !noremote

package attachedbackends

import (
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// countRemovedByComputer counts m's announcements that a computer ended its
// pairing.
func countRemovedByComputer(m *Manager) *atomic.Int32 {
	var n atomic.Int32
	m.SetChanged(func(change SetChange) {
		if change.Action == SetRemoved && change.Reason == RemovedByComputer {
			n.Add(1)
		}
	})
	return &n
}

// A verdict on a carrier whose pairing another manager has replaced on disk
// since takes the carrier out of the cache and nothing else: the pairing
// stored now keeps its agent opt-in and its row, and no end is recorded.
func TestAVerdictKeepsAPairingStoredSince(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	if got := within(t, "the pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("pairing = added %t, %v", got.added, got.err)
	}
	held := enrolled(t, m, p.backendID)
	if err := m.SetAgentAccess(p.backendID, true); err != nil {
		t.Fatal(err)
	}
	theirs := deviceclient.Session{BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
		SessionID: "other-manager", Credential: "c", RefreshSecret: "r", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}
	seed(t, dir, theirs)
	removed := countRemovedByComputer(m)

	m.endSession(p.backendID, held)

	if !held.client.Retired() {
		t.Fatal("the ended carrier is still live")
	}
	if access, err := m.AgentAccess(); err != nil || !access[p.backendID] {
		t.Fatalf("agent access = %v, %v; want the stored pairing's kept", access, err)
	}
	if _, ended := m.TakeVerdict(p.backendID); ended {
		t.Fatal("an end was recorded for a computer whose pairing is stored")
	}
	if n := removed.Load(); n != 0 {
		t.Fatalf("%d removals announced; want none", n)
	}
	if stored, err := deviceclient.LoadSession(dir, p.backendID); err != nil || stored.SessionID != theirs.SessionID {
		t.Fatalf("stored = %+v, %v; want the other manager's pairing", stored, err)
	}
}

// A verdict on a carrier a re-pairing replaced touches nothing: the
// replacement stays cached and working, with its opt-in.
func TestAVerdictOnAReplacedCarrierTouchesNothing(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("first pairing = added %t, %v", got.added, got.err)
	}
	old := enrolled(t, m, p.backendID)
	if got := within(t, "the re-pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("re-pairing = added %t, %v", got.added, got.err)
	}
	replacement := enrolled(t, m, p.backendID)
	if err := m.SetAgentAccess(p.backendID, true); err != nil {
		t.Fatal(err)
	}
	before, err := deviceclient.LoadSession(dir, p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	removed := countRemovedByComputer(m)

	m.endSession(p.backendID, old)

	unchangedCarrier(t, m, p.backendID, replacement, before)
	if access, err := m.AgentAccess(); err != nil || !access[p.backendID] {
		t.Fatalf("agent access = %v, %v; want the replacement's kept", access, err)
	}
	if _, ended := m.TakeVerdict(p.backendID); ended {
		t.Fatal("an end was recorded for a replaced carrier")
	}
	if n := removed.Load(); n != 0 {
		t.Fatalf("%d removals announced; want none", n)
	}
}
