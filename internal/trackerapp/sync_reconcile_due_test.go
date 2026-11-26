package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// idlePump is a pump of m's board whose ticks the test runs (boardTick),
// with no goroutine of its own.
func idlePump(m *mirrorFixture) *boardPump {
	return &boardPump{core: m.svc.newCore(), scope: m.scope(), every: time.Hour}
}

// A refresh lists the board whenever it last reconciled; the pass it
// leaves with candidates goes on at the pump's next tick though the
// reconcile interval has not passed: the last candidate is read then, and
// the board is listed once.
func TestARefreshsReconcilePassGoesOnAtTheNextTick(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 0, func(int) bool { return false })
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the first tick %+v; reconciled at %d", res, m.sync(testBoard).ReconciledAt)
	}
	n := reconcileBatches*readBatch + 1
	b.mu.Lock()
	for i := range n {
		b.listed = append(b.listed, tracker.IssueID(fmt.Sprint(20000+i)))
	}
	b.available = func(i int) bool { return i == n-1 }
	b.mu.Unlock()
	if res := m.svc.boardTick(p, true, Interactive); res.failed || !res.more {
		t.Fatalf("the refresh %+v; want More with the last candidate left", res)
	}
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more {
		t.Fatalf("the next tick %+v; want the pass ended", res)
	}
	last := b.listed[n-1]
	if rows, _, err := m.st.ListMirrorIssueRows(m.root(), string(last), ""); err != nil || len(rows) != 1 {
		t.Fatalf("the last candidate's rows %+v, %v; want its row read by the next tick", rows, err)
	}
	if m.p.count("BoardIssues") != 2 || !b.askedOnce() {
		t.Fatalf("%d listings; each candidate asked once %v; want the first tick's and the refresh's", m.p.count("BoardIssues"), b.askedOnce())
	}
}

// A refresh whose listing failed leaves its pass open: the pump's retry
// lists the board though the reconcile interval has not passed.
func TestARefreshsReconcileWhoseListingFailedListsAtTheRetry(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 1, func(int) bool { return true })
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the first tick %+v; reconciled at %d", res, m.sync(testBoard).ReconciledAt)
	}
	m.p.set(func(p *scripted) {
		list, failed := p.boardIssues, false
		p.boardIssues = func(bd tracker.BoardID, w time.Time) (tracker.Listing, error) {
			if !failed {
				failed = true
				return tracker.Listing{}, &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return list(bd, w)
		}
	})
	b.mu.Lock()
	b.listed = append(b.listed, "20001")
	b.mu.Unlock()
	if res := m.svc.boardTick(p, true, Interactive); !res.failed {
		t.Fatalf("the refresh %+v; want its listing failed", res)
	}
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more {
		t.Fatalf("the retry %+v; want the pass ended", res)
	}
	if rows, _, err := m.st.ListMirrorIssueRows(m.root(), "20001", ""); err != nil || len(rows) != 1 || m.p.count("BoardIssues") != 3 {
		t.Fatalf("20001's rows %+v, %v after %d listings; want the board listed by the retry", rows, err, m.p.count("BoardIssues"))
	}
}

// A reconcile batch whose refused row's retry failed keeps only that row
// in the pass: a peer its first read stored is not asked again.
func TestAReconcileRetryKeepsOnlyTheRefusedCandidates(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")}
	m.p.changedRows = pagedDelta(t, board[:1])
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	asked := map[tracker.IssueID]int{}
	rows := boardOf(board...)
	m.p.set(func(p *scripted) {
		// 10002 is listed with no row; 10001, read just now and not listed,
		// is unsettled: one batch holds both.
		p.boardIssues = listing(true, "10002")
		p.boardRows = func(b tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
			for _, id := range ids {
				asked[id]++
			}
			return rows(b, ids)
		}
	})
	st := m.svc.store
	m.svc.store = &removalBeforeWrite{Store: st, m: m, issue: "10009", every: true}
	holder := &boardPump{scope: m.scope()}
	if _, _, err := m.svc.syncBoard(t.Context(), holder, Background, func(*run) BoardDue { return BoardDue{Reconcile: true} }); err == nil {
		t.Fatal("the reconcile whose row was refused twice succeeded")
	}
	m.svc.store = st
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.More() || m.p.count("BoardIssues") != 1 {
		t.Fatalf("the retry %+v after %d listings; want the pass ended", out, m.p.count("BoardIssues"))
	}
	if asked["10001"] != 1 || asked["10002"] != 3 || !slices.Equal(m.rowIDs(testBoard), []string{"10001", "10002"}) {
		t.Fatalf("asked %v, rows %v; want 10001 asked once and 10002 stored by the retry", asked, m.rowIDs(testBoard))
	}
}

// A refresh whose delta has more of its walk to read leaves its reconcile
// waiting for the walk: the pump's next tick, once the walk ends, lists
// the board though the reconcile interval has not passed.
func TestARefreshsReconcileWaitingOnTheWalkRunsAtTheNextTick(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 1, func(int) bool { return true })
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the first tick %+v; reconciled at %d", res, m.sync(testBoard).ReconciledAt)
	}
	walked := 0
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			walked++
			return tracker.RowPage{Next: cursorAfter(fmt.Sprint("walk", walked)), More: walked <= deltaPages}, nil
		}
	})
	b.mu.Lock()
	b.listed = append(b.listed, "20001")
	b.mu.Unlock()
	if res := m.svc.boardTick(p, true, Interactive); res.failed || !res.more || walked != deltaPages || m.p.count("BoardIssues") != 1 {
		t.Fatalf("the refresh %+v walked %d pages after %d listings; want More, a tick's pages and the reconcile waiting", res, walked, m.p.count("BoardIssues"))
	}
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more || walked != deltaPages+1 {
		t.Fatalf("the next tick %+v walked %d pages; want the walk and the reconcile ended", res, walked)
	}
	if rows, _, err := m.st.ListMirrorIssueRows(m.root(), "20001", ""); err != nil || len(rows) != 1 || m.p.count("BoardIssues") != 2 {
		t.Fatalf("20001's rows %+v, %v after %d listings; want the board listed by the next tick", rows, err, m.p.count("BoardIssues"))
	}
}

// A board's subscribers are told once when its first reconcile is
// recorded, though the reconcile changed no row and cleared no failure:
// the delta stored the board's one row, and the pump's tick reconciles
// what it already holds. A later reconcile, which changes nothing, tells
// nothing.
func TestTheFirstReconcileIsToldThoughNoRowChanged(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a")}
	m.p.changedRows = pagedDelta(t, board)
	m.p.boardIssues = listing(true, "10001")
	m.p.boardRows = boardOf(board...)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if st := m.sync(testBoard); st.ReadAt == 0 || st.ReconciledAt != 0 {
		t.Fatalf("after the delta: %+v; want the board read and never reconciled", st)
	}
	p := idlePump(m)
	before := len(boardEventsOf(m, m.scope()))
	if res := m.svc.boardTick(p, false, Background); res.failed || res.more || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the tick %+v; reconciled at %d", res, m.sync(testBoard).ReconciledAt)
	}
	told := boardEventsOf(m, m.scope())[before:]
	if len(told) != 1 || told[0].Sync.ReconciledAt == 0 || len(told[0].Changed) != 0 || len(told[0].Removed) != 0 {
		t.Fatalf("the first reconcile told %+v; want one event that carries the reconcile and names no row", told)
	}
	if res := m.svc.boardTick(p, true, Interactive); res.failed || res.more {
		t.Fatalf("the refresh %+v", res)
	}
	if again := boardEventsOf(m, m.scope())[before+1:]; len(again) != 0 {
		t.Fatalf("a reconcile that changed nothing told %+v; want nothing", again)
	}
}
