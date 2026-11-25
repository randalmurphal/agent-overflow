package trackerapp

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker"
)

// A run that stored changes under a mirror a turn off detached tells no
// subscription of the mirror made after the turn on: not a board's, not a
// ticket's, whether the run is a write's refresh or a board tick. A
// rename, which keeps the mirror, still tells a subscriber that joined
// after the run began.
func TestAChangeOfADetachedMirrorReachesNoLaterSubscription(t *testing.T) {
	t.Parallel()
	for _, run := range []string{"a write's refresh", "a board tick"} {
		for _, cross := range []string{"off and on", "rename"} {
			t.Run(run+", "+cross, func(t *testing.T) {
				t.Parallel()
				genCrossing(t, run == "a board tick", cross == "rename")
			})
		}
	}
}

// genCrossing runs a write's refresh, or a tick of board 2, that waits in
// the tracker call whose answer changes 10001; meanwhile the connection is
// turned off and on, or renamed, and a client subscribes to board 1 and to
// ticket 10001; then the run stores the answer, which tells its change as
// it commits (run.told), and ends.
func genCrossing(t *testing.T, tick, rename bool) {
	f := newWriteFixture(t)
	ctx := t.Context()
	arrived, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	// hold waits for release the first time, whatever happens to the run's
	// context, so the run stores and publishes after the crossing.
	hold := func() {
		if held.CompareAndSwap(false, true) {
			close(arrived)
			<-release
		}
	}
	var board1 BoardSubscription
	if rename {
		// The board's pump and the ticket's exist before the run: a rename
		// makes none. The pump's first tick has run once a refresh it serves
		// after it returned: a first tick still to run would read the
		// tracker state the run is given and store its change first.
		board1 = f.subscribe(t, testBoard)
		if err := f.svc.RefreshSubscription(ctx, board1.ID); err != nil {
			t.Fatal(err)
		}
		must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001", true, untied))
		f.events.issueEvents(t, 1)
		f.settledTickets(t)
	}
	done := make(chan struct{})
	if tick {
		// The delta, which answers 10001's changed card, waits.
		f.state.set("2", writeRow("10001", 5, "m"))
		f.p.set(func(p *scripted) {
			p.block = func(_ context.Context, name string) error {
				if name == "ChangedRows" {
					hold()
				}
				return nil
			}
		})
		go func() {
			defer close(done)
			_, _ = f.svc.syncTick(context.WithoutCancel(ctx), Scope{Connection: f.conn.ID, Board: "2"}, BoardDue{Delta: true, Reconcile: true}, Background)
		}()
	} else {
		// The refresh's read of board 1, which no longer answers 10001's
		// row and so removes it, waits.
		f.answer(tracker.Result{Parts: []tracker.Part{rankPart(testBoard, "10001", tracker.Applied())}})
		var first sync.Once
		f.p.set(func(p *scripted) {
			p.boardRows = func(b tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
				missing := false
				if b == testBoard {
					first.Do(func() { missing = true })
				}
				if missing {
					hold()
					return tracker.Rows{Missing: slices.Clone(ids)}, nil
				}
				return f.state.boardRows(b, ids)
			}
		})
		go func() {
			defer close(done)
			_, _ = f.svc.Rank(ctx, f.conn.ID, RankRequest{Board: string(testBoard), Issues: []string{"10001"}, Prev: "10003"})
		}()
	}
	<-arrived
	if rename {
		must[Connection](t)(f.svc.Rename(f.conn.ID, "Renamed connection"))
		f.subscribe(t, testBoard)
	} else {
		must[Connection](t)(f.svc.SetEnabled(f.conn.ID, false))
		must[Connection](t)(f.svc.SetEnabled(f.conn.ID, true))
		// The new mirror holds 10001 on board 1; the new board pump's first
		// tick has run once a refresh it serves after it returned.
		tickOK(t, f.svc, f.scope(), BoardDue{Structure: true, Delta: true})
		board1 = f.subscribe(t, testBoard)
		if err := f.svc.RefreshSubscription(ctx, board1.ID); err != nil {
			t.Fatal(err)
		}
		must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001", true, untied))
		f.settledTickets(t)
	}
	if tick {
		// A subscription on the ticking board: its pump's first tick waits
		// for the run's, so on a new mirror what it is told before that
		// tick recorded a read (Sync.ReadAt still zero) is the run's.
		f.subscribe(t, "2")
	}
	tp := f.svc.ticketPumpOf(f.conn.ID, "10001")
	f.svc.subMu.Lock()
	dirty := tp.dirty
	f.svc.subMu.Unlock()
	boards, _, _ := f.events.snapshot()
	before := len(boards)
	close(release)
	<-done
	f.svc.subMu.Lock()
	moved := tp.dirty != dirty
	f.svc.subMu.Unlock()
	boards, _, _ = f.events.snapshot()
	var told []BoardEvent
	for _, e := range boards[before:] {
		// After a rename board 2's pump ticks the mirror the run stored
		// into, which tells the run's change no more.
		run := e.Scope.Board == testBoard || e.Scope.Board == "2" && (rename || e.Sync.ReadAt == 0)
		if run && (slices.Contains(e.Changed, "10001") || slices.Contains(e.Removed, "10001") || e.ReadAgain) {
			told = append(told, e)
		}
	}
	onBoard := func(b tracker.BoardID) bool {
		return slices.ContainsFunc(told, func(e BoardEvent) bool { return e.Scope.Board == b })
	}
	switch {
	case rename && !onBoard(testBoard):
		t.Fatal("after a rename, the board's subscribers were not told the run's change")
	case rename && tick && !onBoard("2"):
		t.Fatal("after a rename, the ticking board's new subscriber was not told the run's change")
	case rename && tick && !moved:
		t.Fatal("after a rename, the open ticket was not told the run's change")
	case !rename && len(told) > 0:
		t.Fatalf("a subscriber of the new mirror was told the old mirror's change: %+v", told)
	case !rename && moved:
		t.Fatal("the new mirror's open ticket was told the old mirror's change")
	}
}

// A change told under the mirror generation a turn off detached reaches
// no board or ticket pump of the mirror made after the turn on, and the
// same change told under the current generation reaches both: a write
// that committed before the turn off can be told after it, since a run
// tells each write as it returns (run.told).
func TestAChangeToldUnderAnEarlierGenerationReachesNoLaterPump(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, testBoard)
	oldGen := f.svc.boardPumpOf(f.scope()).gen
	oldRoot := f.root()
	must[Connection](t)(f.svc.SetEnabled(f.conn.ID, false))
	must[Connection](t)(f.svc.SetEnabled(f.conn.ID, true))
	tickOK(t, f.svc, f.scope(), BoardDue{Structure: true, Delta: true})
	f.subscribe(t, testBoard)
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001", true, untied))
	f.settledTickets(t)
	quiet(t, f.svc)
	bp, tp := f.svc.boardPumpOf(f.scope()), f.svc.ticketPumpOf(f.conn.ID, "10001")
	if bp.gen == oldGen {
		t.Fatal("the turn on kept the mirror generation")
	}
	tell := func(root int64, gen uint64) (boardTold, ticketTold bool) {
		boards, _, _ := f.events.snapshot()
		before := len(boards)
		f.svc.subMu.Lock()
		dirty := tp.dirty
		f.svc.subMu.Unlock()
		ch := Changes{Cards: ids("10001")}
		f.svc.publish(root, gen, f.conn.ID, ch, ownOf(f.scope(), ch), nil)
		quiet(t, f.svc)
		boards, _, _ = f.events.snapshot()
		f.svc.subMu.Lock()
		defer f.svc.subMu.Unlock()
		return slices.ContainsFunc(boards[before:], func(e BoardEvent) bool {
			return slices.Contains(e.Changed, "10001") || e.ReadAgain
		}), tp.dirty != dirty
	}
	if board, ticket := tell(oldRoot, oldGen); board || ticket {
		t.Fatalf("a change told under the earlier generation reached the board %v, the ticket %v", board, ticket)
	}
	if board, ticket := tell(f.root(), bp.gen); !board || !ticket {
		t.Fatalf("a change told under the current generation reached the board %v, the ticket %v; want both", board, ticket)
	}
}
