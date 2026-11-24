package trackerapp

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A write's refresh racing a tick of the same board that read the board
// before the write: whichever stores last, the mirror ends with the rows
// as the later read saw them, and the tick's other changes land.
func TestAWriteRacingATickOfTheSameBoard(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	var clock atomic.Int64
	clock.Store(1_800_000_000_000)
	f.svc.now = func() time.Time { return time.UnixMilli(clock.Add(1)) }
	f.state.set(testBoard, writeRow("10002", 1, "x"))
	arrived := make(chan struct{})
	release := make(chan struct{})
	f.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			page, err := f.state.changedRows(q)
			close(arrived)
			<-release
			return page, err
		}
	})
	ticked := make(chan error, 1)
	go func() {
		out, err := f.svc.tickDelta(t.Context(), f.scope(), Background)
		if err == nil && out.Failure != nil {
			t.Errorf("tick failure %+v", out.Failure)
		}
		ticked <- err
	}()
	<-arrived
	before := len(f.p.callsInOrder())
	f.answer(tracker.Result{Parts: []tracker.Part{rankPart(testBoard, "10001", tracker.Applied())}})
	f.state.set(testBoard, writeRow("10001", 2, "z"))
	if _, err := f.svc.Rank(t.Context(), f.conn.ID, RankRequest{Board: "1", Issues: []string{"10001"}, Next: "10003"}); err != nil {
		t.Fatal(err)
	}
	if got := f.orderOf(t, testBoard, "10001"); got != "z" {
		t.Fatalf("after the write, 10001's order is %q", got)
	}
	close(release)
	if err := <-ticked; err != nil {
		t.Fatal(err)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Rank", "BoardRows", "BoardRows"}) {
		t.Fatalf("calls %v", got)
	}
	if got := f.orderOf(t, testBoard, "10001"); got != "z" {
		t.Fatalf("the tick's earlier read overwrote the write's: 10001's order is %q", got)
	}
	if got := f.orderOf(t, testBoard, "10002"); got != "x" {
		t.Fatalf("the tick's other change did not land: 10002's order is %q", got)
	}
}

// A write's changes reach the funnel once: each subscribed board holding
// the changed issue hears of it in one event, and the issue's open
// ticket, which the refresh read, is settled by that read and not nudged
// to read again.
func TestAWriteReachesTheFunnelOnce(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, testBoard)
	f.subscribe(t, "2")
	hasScope := func(b []BoardEvent, board tracker.BoardID) bool {
		return slices.ContainsFunc(b, func(e BoardEvent) bool { return e.Scope.Board == board })
	}
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	// The pumps' first ticks end before the tracker changes, so only the
	// write reads the change. A first tick that finds nothing new tells
	// nothing, so no event marks its end.
	idleBoards(t, f.svc, f.p)
	f.svc.subMu.Lock()
	tp := f.svc.subs.tickets[ticketKey{connection: f.conn.ID, issue: "10001"}]
	f.svc.subMu.Unlock()
	boards, issues, _ := f.events.snapshot()
	nb, ni := len(boards), len(issues)
	details := len(f.p.detailsRead())
	nudged := func() uint64 {
		f.svc.subMu.Lock()
		defer f.svc.subMu.Unlock()
		return tp.nudged
	}
	before := nudged()
	f.answer(appliedEdit)
	f.state.set(testBoard, writeRow("10001", 1, "a"))
	if _, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}}); err != nil {
		t.Fatal(err)
	}
	if nudged() != before {
		t.Fatal("the ticket the write read was nudged to read again")
	}
	f.events.waitFor(t, "the write's events", func(b []BoardEvent, i []IssueEvent, _ []error) bool {
		return hasScope(b[nb:], testBoard) && hasScope(b[nb:], "2") && len(i) > ni
	})
	// A nudge, had there been one, reads within the wait.
	f.p.waitCall()
	boards, issues, _ = f.events.snapshot()
	if len(boards[nb:]) != 2 {
		t.Fatalf("board events %+v", boards[nb:])
	}
	for _, e := range boards[nb:] {
		if !slices.Equal(e.Changed, []tracker.IssueID{"10001"}) || e.ReadAgain {
			t.Errorf("board event %+v", e)
		}
	}
	if len(issues[ni:]) != 1 || !issues[ni].Changed || issues[ni].Issue != "10001" {
		t.Fatalf("issue events %+v", issues[ni:])
	}
	if got := f.p.detailsRead()[details:]; len(got) != 1 {
		t.Fatalf("details read %v", got)
	}
}

// A refresh that could not read an open ticket tells the ticket the
// failure, then its view with the card the refresh stored over the body
// it held, and the connection's round retries the read on its schedule
// with no round due: the ticket recovers on its own.
func TestAFailedRefreshOfAnOpenTicketIsRetried(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.svc.cadence.RetryBase = time.Millisecond
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	var failing atomic.Bool
	failing.Store(true)
	f.p.set(func(p *scripted) {
		detail := p.detail
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			if failing.CompareAndSwap(true, false) {
				return tracker.Detail{}, &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return detail(id)
		}
	})
	f.answer(appliedEdit)
	f.state.set(testBoard, writeRow("10001", 1, "a"))
	if _, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}}); err != nil {
		t.Fatal(err)
	}
	got := f.events.issueEvents(t, 4)[1:]
	if got[0].Failure == nil || got[0].Failure.Kind != string(FailureTransient) || got[0].Changed ||
		got[1].Failure == nil || !got[1].Changed || got[2].Failure != nil || !got[2].Changed {
		t.Fatalf("events %+v", got)
	}
}

// crossingStore holds a ticket's view rebuild and the round's read that
// crosses it: once armed, the rebuild's read of the mirror's copy waits for
// the round's read to store its document, and that read, once stored,
// waits for release before it settles.
type crossingStore struct {
	*store.Store
	armed   atomic.Bool
	stored  chan struct{}
	release chan struct{}
}

func (s *crossingStore) GetMirrorBody(root int64, issue string) (store.MirrorBody, bool, error) {
	if s.armed.Load() {
		<-s.stored
	}
	return s.Store.GetMirrorBody(root, issue)
}

func (s *crossingStore) StoreMirrorTickets(root, sweep int64, tickets []store.MirrorTicket, catalog store.MirrorCatalog, opened, read store.MirrorRead) (store.MirrorTicketsWrite, error) {
	out, err := s.Store.StoreMirrorTickets(root, sweep, tickets, catalog, opened, read)
	if s.armed.Load() && slices.ContainsFunc(tickets, func(t store.MirrorTicket) bool { return t.Body != nil }) {
		close(s.stored)
		<-s.release
	}
	return out, err
}

// A write's refresh that failed to read an open ticket leaves its view
// behind the card the refresh stored, and the round's retry reads the
// ticket while that view is rebuilt. The rebuild leaves the retry's
// document to the retry, so the ticket is never told the new document with
// the failure the retry ended: it is told the failure, then the new
// document with none.
func TestARebuildLeavesTheDocumentToTheReadInFlight(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.svc.cadence.RetryBase = time.Millisecond
	cs := &crossingStore{Store: f.st, stored: make(chan struct{}), release: make(chan struct{})}
	f.svc.store = cs
	must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001"))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	var failing atomic.Bool
	failing.Store(true)
	f.p.set(func(p *scripted) {
		detail := p.detail
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			if failing.CompareAndSwap(true, false) {
				return tracker.Detail{}, &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return detail(id)
		}
	})
	f.answer(appliedEdit)
	f.state.set(testBoard, writeRow("10001", 1, "a"))
	cs.armed.Store(true)
	// The write settles its ticket and rebuilds its view before it returns.
	if _, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}}); err != nil {
		t.Fatal(err)
	}
	close(cs.release)
	f.events.waitFor(t, "the retry's event", func(_ []BoardEvent, i []IssueEvent, _ []error) bool {
		return len(i) > 2 && i[len(i)-1].Failure == nil
	})
	quiet(t, f.svc)
	_, got, _ := f.events.snapshot()
	if got = got[1:]; len(got) != 2 || got[0].Failure == nil || got[0].Changed || got[1].Failure != nil || !got[1].Changed {
		t.Fatalf("events %+v", got)
	}
}
