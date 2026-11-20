package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

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

func (s *failingStore) RecordMirrorSyncAttempt(root int64, board string, a store.MirrorSyncAttempt) (bool, error) {
	if s.fail.Load() {
		return false, errStoreDown
	}
	return s.Store.RecordMirrorSyncAttempt(root, board, a)
}

func (s *failingStore) GetMirrorSync(root int64, board string) (store.MirrorSync, bool, error) {
	if board == "" && s.fail.Load() {
		return store.MirrorSync{}, false, errStoreDown
	}
	return s.Store.GetMirrorSync(root, board)
}

// A failure outside the tracker is told to the subscribers as one they can
// show, with no store text, once, and cleared when a read succeeds again:
// for a board's tick and for a ticket's read.
func TestAStoreFailureIsTold(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 1, true)
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
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
		if res := m.svc.ticketTick(tp, Background, false); !res.failed || res.err == nil {
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

// Which of the mirror and the pump holds an open ticket's content is
// decided on every successful read: a read that finds the same document
// after the mirror stopped keeping the issue keeps the ticket's view.
func TestAnOpenTicketKeepsItsViewWhenTheMirrorDropsTheIssue(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 1, false)
	m.tracks(nil)
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.storedDetail(t, string(issues[0])); ok {
		t.Fatal("the mirror still holds the detail")
	}
	events := m.issueEventCount()
	m.round(t)
	if v := m.ticketView(t, subs[0].ID); !v.Loaded || v.Detail.Card.ID != string(issues[0]) {
		t.Fatalf("the ticket's view after the mirror dropped it %+v", v)
	}
	if got := m.issueEventsSince(events); len(got) != 0 {
		t.Fatalf("the same document told %+v", got)
	}
	// Kept again and refreshed, the mirror holds the document again and the
	// pump lets its own copy go.
	m.tracks([]tracker.Row{row(string(issues[0]), 1_799_000_000_000, "a000")})
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if err := m.svc.RefreshSubscription(t.Context(), subs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.storedDetail(t, string(issues[0])); !ok {
		t.Fatal("the refresh stored no detail")
	}
	tp := m.svc.ticketPumpOf(m.conn.ID, issues[0])
	m.svc.subMu.Lock()
	held := tp.held != nil
	m.svc.subMu.Unlock()
	if held {
		t.Fatal("the pump still holds a copy of the document the mirror holds")
	}
	if v := m.ticketView(t, subs[0].ID); !v.Loaded {
		t.Fatalf("the ticket's view from the mirror %+v", v)
	}
}

// The subscription-state model: subscribers that join at random moments
// of random reads, failures of the tracker and of the store, recoveries,
// hides and shows on two tickets and two boards of a connection, each
// applying the events after its answer, end in the state a fresh
// subscribe's answer gives, which is the state the last reads left.
func TestSubscriptionStateMatchesAFreshSubscribe(t *testing.T) {
	t.Parallel()
	for seed := range uint64(12) {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			t.Parallel()
			subscriptionModel(t, rand.New(rand.NewPCG(seed, 7)))
		})
	}
}

// modelSub is one subscriber of the model: its target, its handle, the
// sequence number of its answer and the failure the answer told.
type modelSub struct {
	ticket  tracker.IssueID
	board   tracker.BoardID
	handle  string
	seq     uint64
	failure *FailureView
}

func subscriptionModel(t *testing.T, r *rand.Rand) {
	m, st, issues, _ := newRoundFixture(t, 2, r.IntN(2) == 0)
	boards := []tracker.BoardID{testBoard, "2"}
	fs := &failingStore{Store: m.st}
	m.svc.store = fs
	var trackerFail atomic.Bool
	m.p.set(func(p *scripted) {
		p.block = func(context.Context, string) error {
			if trackerFail.Load() {
				return errors.New("test: the tracker refused")
			}
			return nil
		}
	})
	var subs []*modelSub
	subscribe := func() {
		s := &modelSub{}
		if r.IntN(2) == 0 {
			s.ticket = issues[r.IntN(len(issues))]
			a, err := m.svc.SubscribeTicket(m.conn.ID, s.ticket)
			if err != nil {
				t.Fatal(err)
			}
			s.handle, s.seq, s.failure = a.ID, a.Seq, a.Failure
		} else {
			s.board = boards[r.IntN(len(boards))]
			a, err := m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: s.board}, []Section{{Kind: SectionBacklog}})
			if err != nil {
				t.Fatal(err)
			}
			s.handle, s.seq, s.failure = a.ID, a.Seq, a.Failure
		}
		subs = append(subs, s)
	}
	read := func() {
		if r.IntN(2) == 0 {
			if p := m.svc.boardPumpOf(Scope{Connection: m.conn.ID, Board: boards[r.IntN(len(boards))]}); p != nil {
				m.svc.boardTick(p, false, Background)
			}
			return
		}
		if tp := m.svc.ticketPumpOf(m.conn.ID, issues[r.IntN(len(issues))]); tp != nil {
			m.svc.ticketTick(tp, Background, false)
		}
	}
	for range 60 {
		switch op := r.IntN(10); {
		case op < 2 && len(subs) < 10:
			subscribe()
		case op == 2 && len(subs) > 0:
			s := subs[r.IntN(len(subs))]
			if err := m.svc.SetSubscriptionVisible(s.handle, r.IntN(2) == 0); err != nil {
				t.Fatal(err)
			}
		case op == 3:
			fs.fail.Store(!fs.fail.Load())
		case op == 4:
			trackerFail.Store(!trackerFail.Load())
		case op == 5:
			st.edit(issues[r.IntN(len(issues))], func(c *tracker.Card) { c.Title = fmt.Sprint("Title ", r.IntN(100)) })
		default:
			read()
		}
	}
	for len(subs) < 4 {
		subscribe()
	}
	// The last reads: every target read once more, the failures as they
	// stand, which also governs any read still in flight.
	for _, b := range boards {
		if p := m.svc.boardPumpOf(Scope{Connection: m.conn.ID, Board: b}); p != nil {
			m.svc.boardTick(p, false, Background)
		}
	}
	for _, issue := range issues {
		if tp := m.svc.ticketPumpOf(m.conn.ID, issue); tp != nil {
			m.svc.ticketTick(tp, Background, false)
		}
	}
	want := FailureKind("")
	switch {
	case fs.fail.Load():
		want = FailureInternal
	case trackerFail.Load():
		want = FailureTracker
	}
	for _, s := range subs {
		var freshSeq uint64
		var fresh *FailureView
		if s.ticket != "" {
			a := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, s.ticket))
			freshSeq, fresh = a.Seq, a.Failure
		} else {
			a := must[BoardSubscription](t)(m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: s.board}, nil))
			freshSeq, fresh = a.Seq, a.Failure
		}
		applied := s.failure
		boardEvents, issueEvents, _ := m.events.snapshot()
		for _, e := range boardEvents {
			if s.board != "" && e.Scope.Board == s.board && e.Seq > s.seq && e.Seq <= freshSeq {
				applied = e.Sync.Failure
			}
		}
		for _, e := range issueEvents {
			if s.ticket != "" && e.Issue == s.ticket && e.Seq > s.seq && e.Seq <= freshSeq {
				applied = e.Failure
			}
		}
		target := fmt.Sprint("ticket ", s.ticket, " board ", s.board)
		if !sameFailure(applied, fresh) {
			t.Errorf("%s: the subscriber applied %+v; a fresh answer gives %+v", target, applied, fresh)
		}
		if got := failureKind(fresh); got != want {
			t.Errorf("%s: the answer's failure %+v; the last reads failed with %q", target, fresh, want)
		}
	}
}

func failureKind(f *FailureView) FailureKind {
	if f == nil {
		return ""
	}
	return FailureKind(f.Kind)
}
