package ctxutil

import (
	"context"
	"testing"
)

type detachKey struct{}

func TestDetachKeepsValuesIgnoresTheCallerAndEndsWithTheLifetime(t *testing.T) {
	t.Parallel()
	caller, cancelCaller := context.WithCancel(context.WithValue(context.Background(), detachKey{}, "principal"))
	lifetime, endLifetime := context.WithCancel(context.Background())
	detached, release := Detach(caller, lifetime)
	defer release()
	if detached.Value(detachKey{}) != "principal" {
		t.Fatal("Detach dropped the caller's values")
	}
	cancelCaller()
	if detached.Err() != nil {
		t.Fatal("the caller's cancellation reached the detached context")
	}
	endLifetime()
	<-detached.Done()

	// Release alone ends it too, and a lifetime already over starts it done.
	again, releaseAgain := Detach(context.Background(), context.Background())
	releaseAgain()
	<-again.Done()
	over, overRelease := Detach(context.Background(), lifetime)
	defer overRelease()
	<-over.Done()
}
