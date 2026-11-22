//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"github.com/google/uuid"
)

// An introduction holds no profile lock while its redemption waits on the
// other computer, so a lookup of that computer answers at once, and the
// introduction still finishes.
func TestALookupDoesNotWaitForAnIntroductionsRedemption(t *testing.T) {
	t.Parallel()
	held := startHeldIntroduction(t, uuid.NewString())
	if refs := held.m.profiles.Refs(held.backendID); refs != 0 {
		t.Fatalf("profile lock references during the redemption = %d, want none", refs)
	}
	looked := make(chan bool, 1)
	go func() { looked <- held.m.Carrier(context.Background(), held.backendID) == nil }()
	select {
	case none := <-looked:
		if !none {
			t.Fatal("a lookup during the redemption found a carrier before anything was saved")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a lookup waited for the introduction's redemption")
	}
	if got := held.finish(t); got.err != nil || !got.added {
		t.Fatalf("the introduction = added %t, %v; want it enrolled", got.added, got.err)
	}
	waitForProfileRefs(t, held.m, held.backendID, 0)
}

// TestALookupWithADeadlineIsReleasedWhileItsProfileChanges: a change to a
// profile, such as an enrollment committing, holds its profile lock. A
// thread tools call queued behind it gives up when its own context ends
// instead of waiting the change out.
func TestALookupWithADeadlineIsReleasedWhileItsProfileChanges(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	id := uuid.NewString()
	unlock := m.profiles.Lock(id)
	defer unlock()
	waitForProfileRefs(t, m, id, 1)

	lookup, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() { answered <- m.CallThreadPeer(lookup, id, "ThreadToolQuery", nil) }()
	// The change, and the lookup queued behind it.
	waitForProfileRefs(t, m, id, 2)
	cancel()
	select {
	case err := <-answered:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the lookup ended with %v, want its own cancellation", err)
		}
		// Not a refusal about the pairing: nothing is wrong with it.
		if code, _, public := errorsx.PublicDetails(err); public {
			t.Fatalf("the lookup's cancellation was reported as %s: %v", code, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lookup whose context ended was still waiting for the profile change")
	}
	// The lookup left the queue: the change is the one reference left.
	waitForProfileRefs(t, m, id, 1)

	// A page's carried request to the same computer is released the same
	// way when its client goes.
	carried, cancelCarried := context.WithCancel(context.Background())
	none := make(chan bool, 1)
	go func() { none <- m.Carrier(carried, id) == nil }()
	waitForProfileRefs(t, m, id, 2)
	cancelCarried()
	select {
	case none := <-none:
		if !none {
			t.Fatal("a carried request whose context ended was handed a carrier")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a carried request whose context ended was still waiting for the profile change")
	}
	waitForProfileRefs(t, m, id, 1)
}

// waitForProfileRefs waits until the profile lock of id has want holders
// and waiters, the one observable of a caller parked on it.
func waitForProfileRefs(t *testing.T, m *Manager, id string, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); m.profiles.Refs(id) != want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("profile lock references = %d, want %d", m.profiles.Refs(id), want)
		}
	}
}
