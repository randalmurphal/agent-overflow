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

// genCrossing runs a write's refresh, or a tick of board 2, that stores a
// change of 10001 and then waits in a tracker call; meanwhile the
// connection is turned off and on, or renamed, and a client subscribes to
// board 1 and to ticket 10001; then the run goes on and ends.
func genCrossing(t *testing.T, tick, rename bool) {
	f := newWriteFixture(t)
	ctx := t.Context()
	arrived, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	// hold waits for release the first time, whatever happens to the run's
	// context, so the run publishes after the crossing.
	hold := func() {
		if held.CompareAndSwap(false, true) {
			close(arrived)
			<-release
		}
	}
	var board1 BoardSubscription
	if rename {
		// The board's pump and the ticket's exist before the run: a rename
		// makes none.
		board1 = f.subscribe(t, testBoard)
		must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001"))
		f.events.issueEvents(t, 1)
		f.settledTickets(t)
	}
	done := make(chan struct{})
	if tick {
		// The delta stores 10001's changed card; the reconcile waits.
		f.state.set("2", writeRow("10001", 5, "m"))
		f.p.set(func(p *scripted) {
			p.block = func(_ context.Context, name string) error {
				if name == "BoardIssues" {
					hold()
				}
				return nil
			}
		})
		go func() {
			defer close(done)
			_, _ = f.svc.SyncBoard(context.WithoutCancel(ctx), Scope{Connection: f.conn.ID, Board: "2"}, BoardDue{Delta: true, Reconcile: true}, Background)
		}()
	} else {
		// The refresh removes 10001's row from board 1, which the tracker no
		// longer answers, then waits in its read of board 2.
		f.answer(tracker.Result{Parts: []tracker.Part{rankPart(testBoard, "10001", tracker.Applied())}})
		var first sync.Once
		f.p.set(func(p *scripted) {
			p.boardRows = func(b tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
				missing := false
				if b == testBoard {
					first.Do(func() { missing = true })
				}
				if missing {
					return tracker.Rows{Missing: slices.Clone(ids)}, nil
				}
				if b == "2" {
					hold()
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
		must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001"))
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
	inputs, nudged := tp.inputs, tp.nudged
	f.svc.subMu.Unlock()
	boards, _, _ := f.events.snapshot()
	before := len(boards)
	close(release)
	<-done
	f.svc.subMu.Lock()
	moved := tp.inputs != inputs || tp.nudged != nudged
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
