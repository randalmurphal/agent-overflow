package transport

import (
	"context"
	"testing"
	"time"
)

// Unbound methods outlive caller cancellation while caller-bound methods cancel
// with their caller.
func TestMethodContextRespectsCallerBoundFlag(t *testing.T) {
	lifetime, stopServer := context.WithCancel(context.Background())
	defer stopServer()

	caller, cancelCaller := context.WithCancel(withServerLifetime(context.Background(), lifetime))
	unbound := methodContext(caller, &Method{Name: "Unbound", CallerBound: false})
	bound := methodContext(caller, &Method{Name: "Bound", CallerBound: true})

	cancelCaller()

	if unbound.Err() != nil {
		t.Fatalf("unbound method context canceled with caller: %v", unbound.Err())
	}
	if bound.Err() == nil {
		t.Fatal("caller-bound method context outlived its caller")
	}
}

// Shutdown still ends a method that is not caller-bound.
func TestServerShutdownEndsAnUnboundMethod(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	caller := withServerLifetime(context.Background(), lifetime)
	ctx := methodContext(caller, &Method{Name: "Unbound"})
	derived, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	stop()
	select {
	case <-derived.Done():
	case <-time.After(time.Second):
		t.Fatal("derived context outlived server shutdown")
	}
	if ctx.Err() == nil {
		t.Fatal("method context outlived server shutdown")
	}
}
