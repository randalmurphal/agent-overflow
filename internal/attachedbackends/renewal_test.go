package attachedbackends

import (
	"bytes"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
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
	remotetest.Require(t)
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
// again, so its renewal loop ends and no rotation ever comes due. The window
// is far off so the assertion is the loop's end, not a race against the
// clock between the carrier's start and the removal.
func TestRemovingACarrierStopsItsRenewal(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	seedExpiring(t, dir, p, time.Hour)
	if _, err := manager.carrier("peer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the renewal loop starts", func() bool { return renewalRunning() })
	if err := manager.Remove("peer"); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		manager.renewals.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the removed carrier's renewal loop is still running")
	}
	if p.rotations.Load() != 0 {
		t.Fatalf("rotations = %d after removal, want none", p.rotations.Load())
	}
}

// TestCloseJoinsRenewalsAndRefusesNewCarriers: Close is the shutdown join.
// A rotation already sent runs to completion, because the far side may have
// committed it, and Close returns only after it and the renewal loop have
// ended. Nothing starts another renewal afterwards.
func TestCloseJoinsRenewalsAndRefusesNewCarriers(t *testing.T) {
	remotetest.Require(t)
	manager, dir := newManager(t)
	p := newPeer(t)
	p.held, p.release = make(chan struct{}, 1), make(chan struct{})
	var release sync.Once
	answer := func() { release.Do(func() { close(p.release) }) }
	// Runs before the peer's own cleanup, which waits for this handler.
	t.Cleanup(answer)
	seedExpiring(t, dir, p, renewMargin)
	if _, err := manager.carrier("peer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the renewal never reached the far side")
	}
	closed := make(chan struct{})
	go func() {
		manager.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a rotation was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	answer()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the rotation was answered")
	}
	requireNoRenewals(t)
	if session, err := deviceclient.LoadSession(dir, "peer"); err != nil || session.Credential != "credential-1" {
		t.Fatalf("the rotation's successor was not saved: %+v, %v", session, err)
	}
	if _, err := manager.carrier("peer"); !errors.Is(err, ErrClosed) {
		t.Fatalf("carrier after Close = %v, want ErrClosed", err)
	}
	manager.Close()
}

// TestCloseJoinsAnIdleRenewal: a renewal loop waiting for its next window
// has ended by the time Close returns.
func TestCloseJoinsAnIdleRenewal(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	seedExpiring(t, dir, p, time.Hour)
	if _, err := manager.carrier("peer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the renewal loop starts", func() bool { return renewalRunning() })
	// One P: the cancelled loop cannot run until Close blocks, so only a
	// Close that waits for it sees it gone.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	manager.Close()
	requireNoRenewals(t)
	if p.rotations.Load() != 0 {
		t.Fatalf("rotations = %d, want none before the window", p.rotations.Load())
	}
}

func renewalStacks() []byte {
	stacks := make([]byte, 1<<20)
	return stacks[:runtime.Stack(stacks, true)]
}

func renewalRunning() bool {
	return bytes.Contains(renewalStacks(), []byte("(*carrier).keepRenewed"))
}

func requireNoRenewals(t *testing.T) {
	t.Helper()
	stacks := renewalStacks()
	for _, frame := range []string{"(*carrier).keepRenewed", "(*Client).renew.func"} {
		if bytes.Contains(stacks, []byte(frame)) {
			t.Fatalf("%s outlived Close:\n%s", frame, stacks)
		}
	}
}
