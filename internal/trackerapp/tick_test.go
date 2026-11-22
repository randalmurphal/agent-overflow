package trackerapp

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// units are the units an outcome ran, in order.
func units(out Outcome) []Unit {
	var u []Unit
	for _, r := range out.Ran {
		u = append(u, r.Unit)
	}
	return u
}

// A board tick runs what is due in the fixed order and names what each
// unit stored.
func TestBoardTickRunsItsUnitsInOrder(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
		return tracker.Listing{IDs: ids("10001", "10002"), Complete: true}, nil
	}
	m.p.boardRows = func(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
		return tracker.Rows{Rows: []tracker.Row{row("10001", 1_799_000_000_000, "a")}}, nil
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10002", 1_799_000_000_000, "b")})
	m.p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	out := must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{History: true, Delta: true, Reconcile: true, Structure: true}, Background))
	if got := units(out); !slices.Equal(got, []Unit{UnitStructure, UnitDelta, UnitReconcile, UnitHistory}) {
		t.Fatalf("ran %v", got)
	}
	// The reconcile reads only the row the delta's walk did not return.
	if got := m.p.callsInOrder(); !slices.Equal(got, []string{"Structure", "ChangedRows", "BoardIssues", "BoardRows", "History"}) {
		t.Fatalf("calls %v", got)
	}
	if !out.Ran[0].Changes.Structure || !slices.Equal(out.Ran[1].Changes.Rows, ids("10002")) ||
		!slices.Equal(out.Ran[2].Changes.Rows, ids("10001")) || !slices.Equal(out.Ran[3].Changes.History, ids("10001", "10002")) {
		t.Fatalf("changes %+v", out.Ran)
	}
	if s := m.sync(testBoard); s.ReadAt == 0 || s.ReconciledAt == 0 || s.Failure.Kind != "" {
		t.Fatalf("sync state %+v", s)
	}
}

// A tick stops at the first unit that fails, records that failure once,
// keeps what the units before it stored and records what they record.
func TestBoardTickStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := newCountingStore(m.st)
	m.svc.store = cs
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{}, &trackerapi.TransientError{Err: errors.New("reset")}
	}
	out := must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Reconcile: true, Delta: true}, Background))
	if got := units(out); !slices.Equal(got, []Unit{UnitStructure, UnitDelta}) || out.Failure == nil || !out.Changes().Structure {
		t.Fatalf("outcome %+v", out)
	}
	if m.p.count("BoardIssues") != 0 {
		t.Fatal("the reconcile after the failed delta ran")
	}
	calls := cs.take()
	if calls["RecordMirrorSyncAttempt"] != 1 {
		t.Fatalf("sync writes %v", calls)
	}
	if s := m.sync(testBoard); s.Failure.Kind != string(FailureTransient) || s.ReadAt != 0 {
		t.Fatalf("sync state %+v", s)
	}
	// The reconcile runs alone and succeeds: it records its time and clears
	// the failure in one write.
	m.p.set(func(p *scripted) {
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{Complete: true}, nil
		}
	})
	out = must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	calls = cs.take()
	if out.Failure != nil || calls["RecordMirrorSyncAttempt"] != 1 {
		t.Fatalf("reconcile %+v, sync writes %v", out, calls)
	}
	if s := m.sync(testBoard); s.Failure.Kind != "" || s.ReadAt != 0 || s.ReconciledAt == 0 {
		t.Fatalf("sync state %+v", s)
	}
	// A reconcile and a delta that succeed and a history that fails: one
	// write records both successes and then the failure, which stands.
	m.p.set(func(p *scripted) {
		p.changedRows = pagedDelta(t, []tracker.Row{row("10002", 1_799_000_000_000, "b")})
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: ids("10002"), Complete: true}, nil
		}
		p.history = func(tracker.HistoryQuery) (tracker.History, error) {
			return tracker.History{}, &trackerapi.StatusError{Tracker: "jira", Status: 403}
		}
	})
	out = must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Reconcile: true, Delta: true, History: true}, Background))
	calls = cs.take()
	if out.Failure == nil || out.Failure.Kind != FailureForbidden || calls["RecordMirrorSyncAttempt"] != 1 {
		t.Fatalf("tick %+v, sync writes %v", out, calls)
	}
	if s := m.sync(testBoard); s.Failure.Kind != string(FailureForbidden) || s.ReadAt == 0 || s.ReconciledAt == 0 {
		t.Fatalf("sync state %+v", s)
	}
}

// A board no project lists any more ends its tick: no failure, nothing
// stored, and the tracker's answer is not recorded as the scope's.
func TestUnlistedBoardEndsItsTick(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.listBoards(m.conn.ID, "2")
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{row("10002", 1_799_000_000_000, "b")}, Next: cursorAfter("10002")}, nil
		}
	})
	for _, due := range []BoardDue{{Delta: true}, {Structure: true}, {Reconcile: true}} {
		out, err := m.svc.syncTick(t.Context(), m.scope(), due, Background)
		if err != nil || !out.Ended || out.Failure != nil {
			t.Fatalf("tick %+v of an unlisted board: %+v, %v", due, out, err)
		}
	}
	// A board never listed ends the same way.
	if out, err := m.svc.tickStructure(t.Context(), Scope{Connection: m.conn.ID, Board: "42"}, Background); err != nil || !out.Ended {
		t.Fatalf("a board never listed: %+v, %v", out, err)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10002"})); len(cards) != 0 {
		t.Fatalf("an unlisted board stored %+v", cards)
	}
}

// A linked tick with nothing due builds no client and touches nothing. (A
// board's tick chooses its units from the sync state it reads, and always
// runs the delta.)
func TestTickWithNothingDueDoesNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := newCountingStore(m.st)
	m.svc.store = cs
	if out := must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{}, Background)); len(out.Ran) != 0 || out.Ended || out.Failure != nil {
		t.Fatalf("outcome %+v", out)
	}
	m.svc.mu.Lock()
	n := len(m.svc.clients)
	m.svc.mu.Unlock()
	if calls := cs.take(); len(calls) != 0 || n != 0 {
		t.Fatalf("store calls %v, %d clients", calls, n)
	}
}

// waitForWaiters spins until key's lock has n holders and waiters, or
// done closes (a guard removed lets the second tick through instead).
func waitForWaiters(s *Service, key lockKey, n int, done <-chan struct{}) {
	for {
		s.mu.Lock()
		l := s.locks[key]
		s.mu.Unlock()
		if l != nil && refsOf(s, l) >= n {
			return
		}
		select {
		case <-done:
			return
		default:
			runtime.Gosched()
		}
	}
}

func refsOf(s *Service, l *scopeLock) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return l.refs
}

// A second tick of a scope waits for the running one, then runs; the lock
// map is empty after.
func TestTicksOfOneScopeRunOneAfterTheOther(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	var inside, most atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	first := atomic.Bool{}
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			if n := inside.Add(1); n > most.Load() {
				most.Store(n)
			}
			defer inside.Add(-1)
			if first.CompareAndSwap(false, true) {
				close(arrived)
				<-release
			}
			return tracker.RowPage{Next: cursorAfter("10001")}, nil
		}
	})
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, err := m.svc.tickDelta(t.Context(), m.scope(), Background); err != nil {
			t.Error(err)
		}
	}()
	<-arrived
	go func() {
		defer close(secondDone)
		if _, err := m.svc.tickDelta(t.Context(), m.scope(), Background); err != nil {
			t.Error(err)
		}
	}()
	waitForWaiters(m.svc, lockKey{scope: m.scope()}, 2, secondDone)
	close(release)
	<-firstDone
	<-secondDone
	if most.Load() != 1 || m.p.count("ChangedRows") != 2 {
		t.Fatalf("%d deltas at once, %d in all", most.Load(), m.p.count("ChangedRows"))
	}
	m.svc.mu.Lock()
	defer m.svc.mu.Unlock()
	if len(m.svc.locks) != 0 {
		t.Fatalf("locks left %v", m.svc.locks)
	}
}

// A tick waiting for its scope's lock ends with its context.
func TestTickWaitingForItsScopeEndsWithItsContext(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	unlock := must[func()](t)(m.svc.lock(t.Context(), lockKey{scope: m.scope()}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() {
		_, err := m.svc.tickDelta(ctx, m.scope(), Background)
		done <- err
	}()
	waitForWaiters(m.svc, lockKey{scope: m.scope()}, 2, nil)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting tick: %v", err)
	}
	unlock()
	m.svc.mu.Lock()
	defer m.svc.mu.Unlock()
	if len(m.svc.locks) != 0 {
		t.Fatalf("locks left %v", m.svc.locks)
	}
}

// Two boards of one connection never read history at once: the second
// waits, then finds nothing due and reads nothing.
func TestHistoryRunsOnceAConnection(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	other := Scope{Connection: m.conn.ID, Board: "2"}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), other, Background))
	var inside, most atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	first := atomic.Bool{}
	m.p.set(func(p *scripted) {
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			if n := inside.Add(1); n > most.Load() {
				most.Store(n)
			}
			defer inside.Add(-1)
			if first.CompareAndSwap(false, true) {
				close(arrived)
				<-release
			}
			return tracker.History{}, nil
		}
	})
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(firstDone)
		if _, err := m.svc.tickHistory(t.Context(), m.scope(), Background); err != nil {
			t.Error(err)
		}
	}()
	<-arrived
	go func() {
		defer close(secondDone)
		if _, err := m.svc.tickHistory(t.Context(), other, Background); err != nil {
			t.Error(err)
		}
	}()
	waitForWaiters(m.svc, lockKey{scope: Scope{Connection: m.conn.ID}, history: true}, 2, secondDone)
	close(release)
	<-firstDone
	<-secondDone
	if most.Load() != 1 || m.p.count("History") != 1 {
		t.Fatalf("%d history reads at once, %d in all", most.Load(), m.p.count("History"))
	}
}
