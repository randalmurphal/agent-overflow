package attachedbackends

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// One minute is deviceclient's renewMargin: a credential this close to
// expiry is due for rotation.
const renewMargin = time.Minute

// seedExpiring stores a session on p whose credential p honours and whose
// window closes soon enough that the carrier's renewal comes due.
func seedExpiring(t *testing.T, dir string, p *peer, in time.Duration) {
	t.Helper()
	p.mu.Lock()
	p.credential, p.window = "credential-0", in
	p.mu.Unlock()
	legacy := false
	seed(t, dir, deviceclient.Session{
		RefreshRecovery: &legacy,
		BackendID:       "peer", Endpoint: p.URL, SessionID: "session", Credential: "credential-0",
		ExpiresAtMs:   time.Now().Add(in).UnixMilli(),
		RefreshSecret: "refresh-0", RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli(),
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCarrierRenewsBeforeItsWindowClosesAndReportsTheVerdict — a held
// carrier rotates its session as the access window closes with nothing
// dialled and no manifest fetched, so the sockets already riding that
// session stay authorized. When the far side then refuses the rotation for
// good, the verdict evicts the carrier and tells the observer, exactly as
// a refused manifest would.
func TestCarrierRenewsBeforeItsWindowClosesAndReportsTheVerdict(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	seedExpiring(t, dir, p, renewMargin+200*time.Millisecond)
	// The observer is told from the renewal goroutine, so the record is
	// read under the same lock it is written under.
	var mu sync.Mutex
	var ended []SetChange
	manager.SetChanged(func(change SetChange) {
		mu.Lock()
		defer mu.Unlock()
		if change.Action == SetRemoved {
			ended = append(ended, change)
		}
	})
	removed := func() []SetChange {
		mu.Lock()
		defer mu.Unlock()
		return append([]SetChange(nil), ended...)
	}

	if _, err := manager.carrier("peer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rotation", func() bool {
		stored, err := deviceclient.LoadSession(dir, "peer")
		return err == nil && stored.Credential == "credential-1"
	})
	if p.rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want one", p.rotations.Load())
	}
	if got := removed(); len(got) != 0 {
		t.Fatalf("a renewal ended the session: %v", got)
	}

	p.revoke()
	waitFor(t, "the verdict", func() bool { return len(removed()) == 1 })
	if got := removed(); got[0].ID != "peer" || got[0].Reason != RemovedByComputer {
		t.Errorf("observer told %v, want the one ended pairing with its reason", got)
	}
	if manager.Carrier("peer") != nil {
		t.Error("a revoked pairing still has a carrier")
	}
	if _, err := deviceclient.LoadSession(dir, "peer"); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Errorf("profile after revocation: %v, want it gone", err)
	}
}

// TestRemovingACarrierStopsItsRenewal — a removed pairing presents nothing
// again, so the renewal that was due never runs.
func TestRemovingACarrierStopsItsRenewal(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	seedExpiring(t, dir, p, renewMargin+200*time.Millisecond)
	if _, err := manager.carrier("peer"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove("peer"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if p.rotations.Load() != 0 {
		t.Fatalf("rotations = %d after removal, want none", p.rotations.Load())
	}
}
