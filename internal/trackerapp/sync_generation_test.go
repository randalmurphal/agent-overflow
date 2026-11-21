package trackerapp

import (
	"errors"
	"sync"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// gateStore runs gate, once, before the first GetMirrorSync after it is
// set: a test holds a run at its first read of the sync state with it.
type gateStore struct {
	*store.Store
	mu   sync.Mutex
	gate func()
}

func (g *gateStore) GetMirrorSync(root int64, board string, now int64) (store.MirrorSync, bool, error) {
	g.mu.Lock()
	gate := g.gate
	g.gate = nil
	g.mu.Unlock()
	if gate != nil {
		gate()
	}
	return g.Store.GetMirrorSync(root, board, now)
}

// A run that began before the connection was turned off and on, and
// whose first read of the sync state finds its root gone, ends there: it
// takes no root of the later mirror, makes no tracker call and stores
// nothing, whether it is a board tick, a linked tick or a ticket read.
// Its provider and settlement stamp are the earlier connection's.
func TestARunCrossingATurnOffAtItsFirstReadEnds(t *testing.T) {
	t.Parallel()
	for _, caller := range []string{"board tick", "linked tick", "ticket read"} {
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			rows := []tracker.Row{row("10001", 1_799_000_000_000, "a")}
			m.p.changedRows = pagedDelta(t, rows)
			m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
				return detailOfCard(card(string(id), 1_799_000_000_000)), nil
			}
			m.linkThread("thread-1", "10002")
			// The first tick holds the connection on its first root.
			tickOK(t, m.svc, m.scope(), BoardDue{Structure: true, Delta: true})
			gs := &gateStore{Store: m.st}
			m.svc.store = gs
			arrived, release := make(chan struct{}), make(chan struct{})
			gs.gate = func() {
				close(arrived)
				<-release
			}
			calls := len(m.p.callsInOrder())
			type result struct {
				out Outcome
				err error
			}
			done := make(chan result, 1)
			go func() {
				var res result
				switch caller {
				case "board tick":
					res.out, res.err = m.svc.SyncBoard(t.Context(), m.scope(), BoardDue{Delta: true, Reconcile: true}, Background)
				case "linked tick":
					res.out, res.err = m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Cards: true, History: true}, Background)
				default:
					_, _, res.err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Background)
				}
				done <- res
			}()
			<-arrived
			must[Connection](t)(m.svc.SetEnabled(m.conn.ID, false))
			must[Connection](t)(m.svc.SetEnabled(m.conn.ID, true))
			close(release)
			res := <-done
			switch {
			case caller == "ticket read" && !errors.Is(res.err, ErrDisabled):
				t.Fatalf("the ticket read answered %v; want ErrDisabled", res.err)
			case caller != "ticket read" && (res.err != nil || !res.out.Ended || len(res.out.Ran) != 0):
				t.Fatalf("the tick answered %+v, %v; want Ended with no unit run", res.out, res.err)
			}
			if got := m.p.callsInOrder()[calls:]; len(got) != 0 {
				t.Fatalf("the run made tracker calls %v after the turn off", got)
			}
			if root := m.root(); root != 0 {
				t.Fatalf("the run made the later mirror's root %d", root)
			}
		})
	}
}
