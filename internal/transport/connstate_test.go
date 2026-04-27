package transport

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestConnStateContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, state := WithConnState(context.Background())
	if got := ConnStateFromContext(ctx); got != state {
		t.Fatalf("ConnStateFromContext: got %p, want %p", got, state)
	}
}

func TestConnStateFromBareContext(t *testing.T) {
	t.Parallel()
	if got := ConnStateFromContext(context.Background()); got != nil {
		t.Fatalf("expected nil ConnState from bare context, got %v", got)
	}
}

func TestRunCleanupsLIFO(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background())
	var order []int
	state.RegisterCleanup(func() { order = append(order, 1) })
	state.RegisterCleanup(func() { order = append(order, 2) })
	state.RegisterCleanup(func() { order = append(order, 3) })
	state.RunCleanups()
	if got := order; len(got) != 3 || got[0] != 3 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("expected LIFO [3 2 1], got %v", got)
	}
}

func TestRunCleanupsIsIdempotent(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background())
	var calls int32
	state.RegisterCleanup(func() { atomic.AddInt32(&calls, 1) })
	state.RunCleanups()
	state.RunCleanups()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("cleanup ran %d times, want 1", got)
	}
}

func TestRegisterAfterCloseReturnsFalse(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background())
	state.RunCleanups()
	if ok := state.RegisterCleanup(func() {}); ok {
		t.Fatalf("RegisterCleanup after runCleanups must return false")
	}
}

func TestPanickingCleanupDoesNotAbortOthers(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background())
	var ran int32
	state.RegisterCleanup(func() { atomic.AddInt32(&ran, 1) })
	state.RegisterCleanup(func() { panic("boom") })
	state.RegisterCleanup(func() { atomic.AddInt32(&ran, 1) })
	state.RunCleanups()
	// LIFO: cleanup #3 runs, then #2 panics (recovered), then #1 runs.
	// Both non-panicking cleanups must execute.
	if got := atomic.LoadInt32(&ran); got != 2 {
		t.Fatalf("non-panicking cleanups ran %d times, want 2", got)
	}
}

func TestRegisterCleanupNilNoOp(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background())
	if state.RegisterCleanup(nil) {
		t.Fatalf("RegisterCleanup(nil) must return false")
	}
}
