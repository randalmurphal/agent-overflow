package transport

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
)

func TestConnStateContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, state := WithConnState(context.Background(), ConnPrincipal{})
	if got := ConnStateFromContext(ctx); got != state {
		t.Fatalf("ConnStateFromContext: got %p, want %p", got, state)
	}
	if got, err := CallerConnection(ctx); got != state || err != nil {
		t.Fatalf("CallerConnection: got %p, %v, want %p", got, err, state)
	}
}

func TestConnStateFromBareContext(t *testing.T) {
	t.Parallel()
	if got := ConnStateFromContext(context.Background()); got != nil {
		t.Fatalf("expected nil ConnState from bare context, got %v", got)
	}
}

// TestCallerConnectionRefusesACallWithNoConnection: the lookup an arming
// handler opens with refuses, and so does a tie on the nil state it would
// otherwise carry, so nothing can be bound where nothing would release it.
func TestCallerConnectionRefusesACallWithNoConnection(t *testing.T) {
	t.Parallel()
	got, err := CallerConnection(context.Background())
	if got != nil || !errors.Is(err, ErrNoConnection) {
		t.Fatalf("CallerConnection on a bare context = %p, %v; want nil, ErrNoConnection", got, err)
	}
	var none *ConnState
	tie, err := none.Bind("sub", func() { t.Error("a refused tie ran its release") })
	if !errors.Is(err, ErrNoConnection) || tie != (Tie{}) {
		t.Fatalf("Bind on a nil state = %+v, %v; want the zero Tie, ErrNoConnection", tie, err)
	}
	if none.Bound("sub") {
		t.Fatal("a nil state reports a bound release")
	}
	tie.Untie()
}

// TestUntiedReleasesDoNotRun unties releases before the connection ends:
// they do not run, the rest still run last-bound first, and an untie
// repeated, after teardown, or of the zero Tie does nothing.
func TestUntiedReleasesDoNotRun(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var order []int
	ties := map[int]Tie{}
	for i := 1; i <= 5; i++ {
		tie, err := state.Bind(fmt.Sprintf("sub-%d", i), func() { order = append(order, i) })
		if err != nil {
			t.Fatalf("release %d refused on an open connection: %v", i, err)
		}
		ties[i] = tie
	}
	ties[2].Untie()
	ties[4].Untie()
	ties[4].Untie()
	if state.Bound("sub-2") || !state.Bound("sub-3") {
		t.Fatalf("Bound after untie: sub-2 %v, sub-3 %v", state.Bound("sub-2"), state.Bound("sub-3"))
	}
	if keys := state.BoundKeys(); !slices.Equal(keys, []string{"sub-1", "sub-3", "sub-5"}) {
		t.Fatalf("bound after two unties: %v, want [sub-1 sub-3 sub-5]", keys)
	}
	state.RunCleanups()
	if len(order) != 3 || order[0] != 5 || order[1] != 3 || order[2] != 1 {
		t.Fatalf("ran %v, want [5 3 1]", order)
	}
	if keys := state.BoundKeys(); len(keys) != 0 {
		t.Fatalf("bound after the connection ended: %v", keys)
	}
	ties[2].Untie()
	Tie{}.Untie()
}

// TestBindIsKeyed: a key names one release for the connection's life. A
// second Bind of a bound key keeps the first release, so a resource
// registered once per connection needs no record of its own of whether
// the connection is registered, and any of its Ties unties it.
func TestBindIsKeyed(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var first, second atomic.Int32
	tie, err := state.Bind("claims", func() { first.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	again, err := state.Bind("claims", func() { second.Add(1) })
	if err != nil || again != tie {
		t.Fatalf("rebinding a bound key = %+v, %v; want the first Tie", again, err)
	}
	if held := len(state.BoundKeys()); held != 1 {
		t.Fatalf("a key bound twice holds %d releases, want 1", held)
	}
	state.RunCleanups()
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("releases ran first=%d second=%d, want the first once", first.Load(), second.Load())
	}
}

// TestAnEarlierTieOfAReusedKeyUntiesNothing: a Tie names its own
// registration. Once it is untied, the key bound again is a registration
// of its own, which a repeated untie of the first Tie leaves bound, and the
// connection's end runs.
func TestAnEarlierTieOfAReusedKeyUntiesNothing(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var ran atomic.Int32
	first, err := state.Bind("pane", func() { t.Error("the untied registration ran") })
	if err != nil {
		t.Fatal(err)
	}
	first.Untie()
	second, err := state.Bind("pane", func() { ran.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("the key bound again answered the untied registration's Tie")
	}
	first.Untie()
	if !state.Bound("pane") {
		t.Fatal("an untie of the earlier registration removed the key's next one")
	}
	state.RunCleanups()
	if ran.Load() != 1 {
		t.Fatalf("the connection's end ran the key's second registration %d times, want 1", ran.Load())
	}
}

// TestBindAndUntieVisitNoEntries counts, rather than times, what binding
// and untying cost: a Bind or an Untie reaches its one registration through
// the key index, so 256 fresh registrations bound and untied in any order
// visit no other entry; only the walks that answer every registration,
// BoundKeys and the teardown, visit each once.
func TestBindAndUntieVisitNoEntries(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	const n = 256
	ties := make([]Tie, 0, n)
	bind := func(round int) {
		for i := range n {
			tie, err := state.Bind(fmt.Sprintf("handle-%d-%d", round, i), func() {})
			if err != nil {
				t.Fatal(err)
			}
			ties = append(ties, tie)
		}
	}
	bind(0)
	for _, tie := range ties {
		tie.Untie()
	}
	ties = ties[:0]
	bind(1)
	for i := len(ties) - 1; i >= 0; i-- {
		ties[i].Untie()
	}
	ties = ties[:0]
	bind(2)
	state.mu.Lock()
	walked := state.walked
	state.mu.Unlock()
	if walked != 0 {
		t.Fatalf("binding and untying %d registrations visited %d entries, want 0", 3*n, walked)
	}
	if keys := state.BoundKeys(); len(keys) != n {
		t.Fatalf("%d bound, want %d", len(keys), n)
	}
	state.RunCleanups()
	state.mu.Lock()
	walked = state.walked
	state.mu.Unlock()
	if walked != 2*n {
		t.Fatalf("BoundKeys and the teardown visited %d entries, want %d", walked, 2*n)
	}
}

func TestTieHeldByItsConnectionAlone(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	_, other := WithConnState(context.Background(), ConnPrincipal{})
	tie, err := state.Bind("sub", func() {})
	if err != nil {
		t.Fatal(err)
	}
	if !tie.HeldBy(state) || tie.HeldBy(other) || tie.HeldBy(nil) || (Tie{}).HeldBy(nil) {
		t.Fatal("HeldBy answers for a connection the tie is not bound to")
	}
}

func TestRunCleanupsIsIdempotent(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var calls int32
	if _, err := state.Bind("sub", func() { atomic.AddInt32(&calls, 1) }); err != nil {
		t.Fatal(err)
	}
	state.RunCleanups()
	state.RunCleanups()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("cleanup ran %d times, want 1", got)
	}
}

// TestBindAfterCloseIsRefused: a connection whose cleanup pass began binds
// nothing and says so, so the caller rolls back what it opened.
func TestBindAfterCloseIsRefused(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	state.RunCleanups()
	tie, err := state.Bind("sub", func() { t.Error("a refused tie ran its release") })
	if !errors.Is(err, ErrConnectionClosing) || tie != (Tie{}) {
		t.Fatalf("Bind after RunCleanups = %+v, %v; want the zero Tie, ErrConnectionClosing", tie, err)
	}
	if state.Bound("sub") {
		t.Fatal("a closed connection reports a bound release")
	}
}

func TestPanickingCleanupDoesNotAbortOthers(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	var ran int32
	for i, release := range []func(){
		func() { atomic.AddInt32(&ran, 1) },
		func() { panic("boom") },
		func() { atomic.AddInt32(&ran, 1) },
	} {
		if _, err := state.Bind(fmt.Sprintf("sub-%d", i), release); err != nil {
			t.Fatal(err)
		}
	}
	state.RunCleanups()
	// LIFO: cleanup #3 runs, then #2 panics (recovered), then #1 runs.
	// Both non-panicking cleanups must execute.
	if got := atomic.LoadInt32(&ran); got != 2 {
		t.Fatalf("non-panicking cleanups ran %d times, want 2", got)
	}
}

func TestBindNeedsAKeyAndARelease(t *testing.T) {
	t.Parallel()
	_, state := WithConnState(context.Background(), ConnPrincipal{})
	if _, err := state.Bind("sub", nil); err == nil {
		t.Fatal("Bind took a nil release")
	}
	if _, err := state.Bind("", func() {}); err == nil {
		t.Fatal("Bind took an empty key")
	}
	if held := len(state.BoundKeys()); held != 0 {
		t.Fatalf("refused binds left %d releases", held)
	}
}
