package trackerapp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// failingStore fails, while fail is set, the store calls only a board tick
// or a ticket read makes: recording a board's sync outcome, and reading
// the linked scope's sync state, which every ticket read starts with.
type failingStore struct {
	*store.Store
	fail atomic.Bool
}

var errStoreDown = errors.New("test: the store failed")

func (s *failingStore) RecordMirrorSyncAttempt(root int64, board string, a store.MirrorSyncAttempt) (store.MirrorSyncOutcome, bool, error) {
	if s.fail.Load() {
		return store.MirrorSyncOutcome{}, false, errStoreDown
	}
	return s.Store.RecordMirrorSyncAttempt(root, board, a)
}

func (s *failingStore) GetMirrorSync(root int64, board string, now int64) (store.MirrorSync, bool, error) {
	if board == "" && s.fail.Load() {
		return store.MirrorSync{}, false, errStoreDown
	}
	return s.Store.GetMirrorSync(root, board, now)
}

// A failure outside the tracker is told to the subscribers as one they can
// show, with no store text, once, and cleared when a read succeeds again:
// for a board's tick and for a ticket's read.
func TestAStoreFailureIsTold(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 1, true)
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	// The board's first tick may have the ticket rebuild its view: the
	// store is swapped once nothing uses it.
	quiet(t, m.svc)
	fs := &failingStore{Store: m.st}
	m.svc.store = fs
	fs.fail.Store(true)
	p := m.svc.boardPumpOf(m.scope())
	tp := m.svc.ticketPumpOf(m.conn.ID, issues[0])
	before, _, _ := m.events.snapshot()
	boards, events := len(before), m.issueEventCount()
	for range 2 {
		if res := m.svc.boardTick(p, false, Background); !res.failed {
			t.Fatalf("board tick %+v", res)
		}
		if res := m.svc.ticketTick(tp, Background, false); !res.failed || res.errOf(0) == nil {
			t.Fatalf("ticket read %+v", res)
		}
	}
	b, issueEvents, _ := m.events.snapshot()
	b, issueEvents = b[boards:], issueEvents[events:]
	if len(b) != 1 || b[0].Sync.Failure == nil || b[0].Sync.Failure.Kind != string(FailureInternal) {
		t.Fatalf("board events %+v", b)
	}
	if len(issueEvents) != 1 || issueEvents[0].Issue != issues[0] || issueEvents[0].Failure == nil || issueEvents[0].Failure.Kind != string(FailureInternal) {
		t.Fatalf("ticket events %+v", issueEvents)
	}
	for _, f := range []*FailureView{b[0].Sync.Failure, issueEvents[0].Failure} {
		if f.Message == "" || strings.Contains(f.Message, errStoreDown.Error()) {
			t.Fatalf("the failure's message %q", f.Message)
		}
	}
	if v := m.ticketView(t, subs[0].ID); v.Failure == nil {
		t.Fatal("the ticket's view does not carry the failure")
	}
	// A subscriber joining now is told the failure in its answer.
	joined := m.subscribe(t, testBoard)
	if joined.Failure == nil || joined.View.Sync.Failure == nil || joined.Failure.Kind != string(FailureInternal) {
		t.Fatalf("a joining board subscriber's answer %+v", joined.Failure)
	}
	ticket := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, issues[0]))
	if ticket.Failure == nil || ticket.Failure.Kind != string(FailureInternal) {
		t.Fatalf("a joining ticket subscriber's answer %+v", ticket.Failure)
	}
	fs.fail.Store(false)
	before, _, _ = m.events.snapshot()
	boards, events = len(before), m.issueEventCount()
	m.svc.boardTick(p, false, Background)
	m.svc.ticketTick(tp, Background, false)
	b, issueEvents, _ = m.events.snapshot()
	if b = b[boards:]; len(b) != 1 || b[0].Sync.Failure != nil {
		t.Fatalf("the board's recovery %+v", b)
	}
	if issueEvents = issueEvents[events:]; len(issueEvents) != 1 || issueEvents[0].Failure != nil {
		t.Fatalf("the ticket's recovery %+v", issueEvents)
	}
}

// A connection change while a ticket is read does not lose what the read
// stored: the funnel gets the root the read held, so the board whose card
// it changed is told.
func TestARenameDuringATicketReadStillTellsTheBoard(t *testing.T) {
	t.Parallel()
	m, st, issues, _ := newRoundFixture(t, 1, true)
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	boards, _, _ := m.events.snapshot()
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	m.p.set(func(p *scripted) {
		p.block = func(_ context.Context, name string) error {
			if name == "Cards" {
				_, err := m.svc.Rename(m.conn.ID, "Renamed connection")
				return err
			}
			return nil
		}
	})
	if res := m.svc.ticketTick(m.svc.ticketPumpOf(m.conn.ID, issues[0]), Background, false); res.failed {
		t.Fatalf("read %+v", res)
	}
	after, _, _ := m.events.snapshot()
	if got := after[len(boards):]; len(got) != 1 || !slices.Contains(got[0].Changed, issues[0]) {
		t.Fatalf("board events after the read %+v", got)
	}
}

// An open ticket whose bind names no row of the mirror (keepsLost) is not
// the issue's current bind, so the record keeps no body of its reads: the
// read fails loudly, a failure outside the tracker, the view is not
// loaded, and no copy of the read's document stands in for the record.
func TestAnOpenTicketWhoseBindIsNotTheIssuesStoresNoBody(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.p.set(func(p *scripted) {
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
		}
	})
	m.loseKeeps()
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "20001"))
	m.settledTickets(t)
	if _, _, ok := m.storedDetail(t, "20001"); ok {
		t.Fatal("the mirror holds the detail of a read under a bind that is not the issue's")
	}
	v := m.ticketView(t, sub.ID)
	if v.Loaded || v.Failure == nil || v.Failure.Kind != string(FailureInternal) {
		t.Fatalf("the ticket's view %+v; want not loaded, with a failure outside the tracker", v)
	}
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); !errors.Is(err, errBodyNotKept) {
		t.Fatalf("the refresh: %v; want the body not kept", err)
	}
}
