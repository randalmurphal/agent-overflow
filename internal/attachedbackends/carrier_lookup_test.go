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
// and activation, which can take minutes. A thread tools call to that
// computer gives up when its own context ends instead of waiting the change
// out, and the change itself still finishes.
func TestALookupWithADeadlineIsReleasedWhileItsProfileChanges(t *testing.T) {
	t.Parallel()
	held := startHeldIntroduction(t, uuid.NewString())

	lookup, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() { answered <- held.m.CallThreadPeer(lookup, held.backendID, "ThreadToolQuery", nil) }()
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
	// A page's carried request to the same computer is released the same
	// way when its client goes.
	carried := make(chan bool, 1)
	go func() { carried <- held.m.Carrier(lookup, held.backendID) == nil }()
	select {
	case none := <-carried:
		if !none {
			t.Fatal("a carried request whose context ended was handed a carrier")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a carried request whose context ended was still waiting for the profile change")
	}

	if got := held.finish(t); got.err != nil || !got.added {
		t.Fatalf("the introduction = added %t, %v; want it enrolled", got.added, got.err)
	}
}
