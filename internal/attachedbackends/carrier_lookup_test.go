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

// TestALookupWithADeadlineIsReleasedWhileItsProfileChanges: an own-device
// introduction holds the destination's profile lock across its redemption
// and activation, which can take minutes. A thread tools call queued behind
// it gives up when its own context ends instead of waiting the change out,
// and the change itself still finishes.
func TestALookupWithADeadlineIsReleasedWhileItsProfileChanges(t *testing.T) {
	t.Parallel()
	held := startHeldIntroduction(t, uuid.NewString())
	waitForProfileRefs(t, held.m, held.backendID, 1)

	lookup, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() { answered <- held.m.CallThreadPeer(lookup, held.backendID, "ThreadToolQuery", nil) }()
	// The introduction, and the lookup queued behind it.
	waitForProfileRefs(t, held.m, held.backendID, 2)
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
	// The lookup left the queue: the introduction is the one reference left.
	waitForProfileRefs(t, held.m, held.backendID, 1)

	// A page's carried request to the same computer is released the same
	// way when its client goes.
	carried, cancelCarried := context.WithCancel(context.Background())
	none := make(chan bool, 1)
	go func() { none <- held.m.Carrier(carried, held.backendID) == nil }()
	waitForProfileRefs(t, held.m, held.backendID, 2)
	cancelCarried()
	select {
	case none := <-none:
		if !none {
			t.Fatal("a carried request whose context ended was handed a carrier")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a carried request whose context ended was still waiting for the profile change")
	}
	waitForProfileRefs(t, held.m, held.backendID, 1)

	if got := held.finish(t); got.err != nil || !got.added {
		t.Fatalf("the introduction = added %t, %v; want it enrolled", got.added, got.err)
	}
	waitForProfileRefs(t, held.m, held.backendID, 0)
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
