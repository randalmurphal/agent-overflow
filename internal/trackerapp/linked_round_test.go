package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// historyDueFixture is a mirror fixture whose board holds n issues, each
// due a history read, on a tracker whose history read answers with none.
func historyDueFixture(t *testing.T, n int) *mirrorFixture {
	t.Helper()
	m := newMirrorFixture(t)
	rows := realisticRows(0, n, time.UnixMilli(1_799_000_000_000).UTC())
	var pages [][]tracker.Row
	for len(rows) > 0 {
		k := min(len(rows), 100)
		pages, rows = append(pages, rows[:k]), rows[k:]
	}
	m.p.changedRows = pagedDelta(t, pages...)
	for {
		if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); !out.More() {
			break
		}
	}
	m.p.set(func(p *scripted) {
		p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	})
	return m
}

// historyDue is how many issues the due listing lists, a unit's
// historyBatch*historyBatches at most, and whether more are due.
func (m *mirrorFixture) historyDue(t *testing.T) (int, bool) {
	t.Helper()
	due, more, err := m.st.ListMirrorHistoryDue(m.root(), m.svc.steady().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return len(due), more
}

// A linked round runs its connection's ticks while a unit reports More,
// each running only the units still owed: every due history is read in
// one round, in historyBatch issues a request, and the cards unit, which
// finished in the first tick, does not run again.
func TestALinkedRoundRunsWhileMoreRemains(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		due, requests int
		store         string
	}{
		// Ticks of at most historyBatches requests each: 2 for 201 issues,
		// 6 for 1,001. Each tick reads its sync state, lists the due issues
		// once and writes each batch; the first also reads the linked issues
		// and records the cards unit's read. The calls are store accessor
		// calls, each one statement or transaction.
		{201, 5, "GetMirrorSync:2 ListMirrorHistoryDue:2 ListMirrorOffBoardLinkedIssues:1 RecordMirrorSyncAttempt:1 ReplaceMirrorHistories:5"},
		{1001, 21, "GetMirrorSync:6 ListMirrorHistoryDue:6 ListMirrorOffBoardLinkedIssues:1 RecordMirrorSyncAttempt:1 ReplaceMirrorHistories:21"},
	} {
		t.Run(fmt.Sprint(c.due), func(t *testing.T) {
			t.Parallel()
			m := historyDueFixture(t, c.due)
			if listed, more := m.historyDue(t); listed != historyBatch*historyBatches || !more {
				t.Fatalf("%d issues listed due, more %v", listed, more)
			}
			cs := newCountingStore(m.st)
			m.svc.store = cs
			calls := len(m.p.callsInOrder())
			m.svc.linkedRound(t.Context())
			provider, stored := countsOf(m.p.callsInOrder()[calls:]), cs.take()
			t.Logf("a round over %d due: provider %s, store %s", c.due, describeCounts(provider), describeCounts(stored))
			if listed, more := m.historyDue(t); listed != 0 || more {
				t.Fatalf("%d issues left due, more %v", listed, more)
			}
			delete(stored, "ListTrackerConnections")
			if provider["History"] != c.requests || len(provider) != 1 || describeCounts(stored) != c.store {
				t.Errorf("a round over %d due: provider %s, store %s; pinned %d History and %s", c.due, describeCounts(provider), describeCounts(stored), c.requests, c.store)
			}
		})
	}
}

// A linked round stops at a failure, when its context ends, and when no
// client is connected any more, leaving the rest due for the next round.
func TestALinkedRoundStopsWhereItMust(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"failure", "cancel", "disconnect", "rate limit"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()
			m := historyDueFixture(t, 3*historyBatch*historyBatches)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			m.p.set(func(p *scripted) {
				p.history = func(tracker.HistoryQuery) (tracker.History, error) {
					if calls.Add(1) < historyBatches {
						return tracker.History{}, nil
					}
					switch stop {
					case "failure":
						return tracker.History{}, errors.New("test: refused")
					case "rate limit":
						return tracker.History{}, &trackerapi.RateLimitedError{Tracker: "jira", Status: 429, Until: time.Now().Add(time.Hour)}
					case "cancel":
						cancel()
					case "disconnect":
						m.events.disconnected.Store(true)
					}
					return tracker.History{}, nil
				}
			})
			m.svc.linkedRound(ctx)
			if got := int(calls.Load()); got != historyBatches {
				t.Fatalf("the round made %d history reads after it was to stop at %d", got, historyBatches)
			}
			if listed, _ := m.historyDue(t); listed == 0 {
				t.Fatal("nothing left due")
			}
		})
	}
}

// StartLinkedSync after a failed read of the connections leaves the loop
// running and reports the failure; a second call succeeds with the loop
// running. A connection change whose read fails leaves it running too.
func TestStartLinkedSyncAfterAFailedReadRuns(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	ls := &listFailStore{Store: m.st}
	m.svc.store = ls
	ls.fail.Store(true)
	if err := m.svc.StartLinkedSync(); err == nil {
		t.Fatal("a failed read of the connections reported no failure")
	}
	if m.svc.loopRunning() == nil {
		t.Fatal("no loop runs after the failed read")
	}
	ls.fail.Store(false)
	if err := m.svc.StartLinkedSync(); err != nil || m.svc.loopRunning() == nil {
		t.Fatalf("the second call: %v, running %v", err, m.svc.loopRunning() != nil)
	}
	// Turned off with the read failing, the loop keeps running; the next
	// change that reads the connections stops it.
	ls.fail.Store(true)
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	running := m.svc.loopRunning()
	if running == nil {
		t.Fatal("a failed read stopped the loop")
	}
	ls.fail.Store(false)
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	assertExited(t, running, "the loop with no connection on")
}

// listFailStore fails the read of the connections while fail is set.
type listFailStore struct {
	*store.Store
	fail atomic.Bool
}

func (s *listFailStore) ListTrackerConnections() ([]store.TrackerConnection, error) {
	if s.fail.Load() {
		return nil, errors.New("test: the connections could not be read")
	}
	return s.Store.ListTrackerConnections()
}
