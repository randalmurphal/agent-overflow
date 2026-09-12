package deviceclient

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// keepRenewed runs KeepRenewed until the test ends and answers its result.
func keepRenewed(t *testing.T, client *Client, report func(error)) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- client.KeepRenewed(ctx, report) }()
	return done, cancel
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

// TestKeepRenewed_RotatesBeforeExpiryWithoutADial — the credential is
// rotated as it enters renewMargin, with no ticket minted and nothing
// dialled, and the rotated pair is stored before the loop waits again.
func TestKeepRenewed_RotatesBeforeExpiryWithoutADial(t *testing.T) {
	be := newBackend(t)
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(renewMargin + 200*time.Millisecond).UnixMilli()
	})
	reported := func(err error) { t.Errorf("reported %v, want no transient failure", err) }
	done, cancel := keepRenewed(t, client, reported)

	waitFor(t, "the rotation", func() bool { return be.rotations.Load() == 1 })
	stored, err := LoadSession(dir, "backend-a")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if stored.Credential == "ao1.issued-0" || stored.Credential != client.Session().Credential {
		t.Fatalf("stored credential %q, in memory %q, want the rotated one in both", stored.Credential, client.Session().Credential)
	}
	if be.tickets.Load() != 0 {
		t.Fatal("a proactive renewal minted a socket ticket")
	}
	// The fresh credential lasts an hour: nothing rotates again.
	time.Sleep(100 * time.Millisecond)
	if be.rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want one until the next window closes", be.rotations.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("KeepRenewed after cancel = %v, want context.Canceled", err)
	}
}

// TestKeepRenewed_RetriesAnOutageAndKeepsThePairing — a rotation the
// network dropped is reported and retried until it lands, and the pairing
// is never removed for an outage.
func TestKeepRenewed_RetriesAnOutageAndKeepsThePairing(t *testing.T) {
	previous := renewRetryDelay
	renewRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { renewRetryDelay = previous })

	be := newBackend(t)
	be.failureStatus.Store(http.StatusServiceUnavailable)
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(renewMargin / 2).UnixMilli()
	})
	var reports atomic.Int32
	done, cancel := keepRenewed(t, client, func(error) { reports.Add(1) })

	waitFor(t, "a second attempt", func() bool { return reports.Load() >= 2 })
	if be.rotations.Load() != 0 {
		t.Fatalf("rotations = %d during the outage", be.rotations.Load())
	}
	if stored, err := LoadSession(dir, "backend-a"); err != nil || stored.RefreshSecret != "refresh-0" {
		t.Fatalf("the outage changed the stored session: %+v %v", stored, err)
	}
	be.failureStatus.Store(0)
	waitFor(t, "the rotation after the outage", func() bool { return be.rotations.Load() == 1 })
	waitFor(t, "the rotated session", func() bool {
		stored, err := LoadSession(dir, "backend-a")
		return err == nil && stored.RefreshSecret != "refresh-0"
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("KeepRenewed after cancel = %v, want context.Canceled", err)
	}
}

// TestKeepRenewed_EndsWhenTheBackendRefusesTheSessionForGood — a revoked
// session is the verdict: the loop returns it, the owner is retired and the
// stored session is gone, without a retry.
func TestKeepRenewed_EndsWhenTheBackendRefusesTheSessionForGood(t *testing.T) {
	be := newBackend(t)
	be.refusal = "revoked_session"
	client, dir := openAgainst(t, be, func(s *Session) {
		s.ExpiresAtMs = time.Now().Add(renewMargin / 2).UnixMilli()
	})
	done, _ := keepRenewed(t, client, func(err error) { t.Errorf("reported %v, want the verdict returned", err) })

	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionEnded) {
			t.Fatalf("KeepRenewed = %v, want ErrSessionEnded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the verdict did not end the loop")
	}
	if !client.Retired() {
		t.Fatal("a revoked session left the owner live")
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("stored session after revocation: %v, want it gone", err)
	}
	if be.rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want the one refused attempt", be.rotations.Load())
	}
}

// TestKeepRenewed_StopsOnceTheOwnerIsRetired — a replacement owner or a
// removal retires this one, and a retired owner never presents anything
// again.
func TestKeepRenewed_StopsOnceTheOwnerIsRetired(t *testing.T) {
	be := newBackend(t)
	client, _ := openAgainst(t, be, nil)
	client.Retire()
	done, _ := keepRenewed(t, client, func(err error) { t.Errorf("reported %v", err) })
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionEnded) {
			t.Fatalf("KeepRenewed on a retired owner = %v, want ErrSessionEnded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a retired owner kept its renewal loop")
	}
	if be.rotations.Load() != 0 {
		t.Fatal("a retired owner rotated")
	}
}
