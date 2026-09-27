package transport

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestConnStateContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, state := WithConnState(context.Background(), ConnPrincipal{})
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
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var order []int
	state.RegisterCleanup(func() { order = append(order, 1) })
	state.RegisterCleanup(func() { order = append(order, 2) })
	state.RegisterCleanup(func() { order = append(order, 3) })
	state.RunCleanups()
	if got := order; len(got) != 3 || got[0] != 3 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("expected LIFO [3 2 1], got %v", got)
	}
}

// TestUnboundCleanupsDoNotRun unbinds cleanups before the connection ends:
// they do not run, the rest still run last-registered first, and an unbind
// after teardown, of an unknown key or without a connection does nothing.
func TestUnboundCleanupsDoNotRun(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var order []int
	for i := 1; i <= 5; i++ {
		ran := func() { order = append(order, i) }
		var ok bool
		if i%2 == 0 {
			ok = state.BindCleanup(fmt.Sprintf("sub-%d", i), ran)
		} else {
			ok = state.RegisterCleanup(ran)
		}
		if !ok {
			t.Fatalf("cleanup %d refused on an open connection", i)
		}
	}
	state.UnbindCleanup("sub-2")
	state.UnbindCleanup("sub-4")
	state.UnbindCleanup("sub-4")
	state.UnbindCleanup("unknown")
	state.mu.Lock()
	held := len(state.cleanups)
	state.mu.Unlock()
	if held != 3 {
		t.Fatalf("connection holds %d cleanups after two unbinds, want 3", held)
	}
	state.RunCleanups()
	if len(order) != 3 || order[0] != 5 || order[1] != 3 || order[2] != 1 {
		t.Fatalf("ran %v, want [5 3 1]", order)
	}
	state.UnbindCleanup("sub-2")
	var none *ConnState
	none.UnbindCleanup("sub-2")
}

func TestRunCleanupsIsIdempotent(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
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
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	state.RunCleanups()
	if ok := state.RegisterCleanup(func() {}); ok {
		t.Fatalf("RegisterCleanup after runCleanups must return false")
	}
}

func TestPanickingCleanupDoesNotAbortOthers(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
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
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	if state.RegisterCleanup(nil) {
		t.Fatalf("RegisterCleanup(nil) must return false")
	}
}
