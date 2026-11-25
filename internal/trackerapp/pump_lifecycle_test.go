package trackerapp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// boardCounts counts the sync state reads and records of each board: a
// tick makes one of each, so a board with as many records as reads has no
// tick in flight.
type boardCounts struct {
	*store.Store
	mu             sync.Mutex
	reads, records map[string]int
}

func (c *boardCounts) GetMirrorSync(root int64, board string, now int64) (store.MirrorSync, bool, error) {
	c.mu.Lock()
	c.reads[board]++
	c.mu.Unlock()
	return c.Store.GetMirrorSync(root, board, now)
}

func (c *boardCounts) RecordMirrorSyncAttempt(root int64, board string, a store.MirrorSyncAttempt) (store.MirrorSyncOutcome, bool, error) {
	defer func() {
		c.mu.Lock()
		c.records[board]++
		c.mu.Unlock()
	}()
	return c.Store.RecordMirrorSyncAttempt(root, board, a)
}

func (c *boardCounts) of(board string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[board] + c.records[board]
}

func (c *boardCounts) idle(board string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[board] == c.records[board]
}

// deltasOf counts the delta reads of each board.
type deltasOf struct {
	mu sync.Mutex
	n  map[tracker.BoardID]int
}

func (d *deltasOf) count(b tracker.BoardID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[b]
}

// A board every handle hides makes no request and no store call while
// another board keeps ticking, and catches up when shown again: its timer
// is not re-armed while it is hidden, so only the resume reads again.
func TestHiddenBoardCostsNothingAndCatchesUp(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: 20 * time.Millisecond})
	counts := &boardCounts{Store: m.st, reads: map[string]int{}, records: map[string]int{}}
	m.svc.store = counts
	m.tracks(nil)
	deltas := &deltasOf{n: map[tracker.BoardID]int{}}
	walk := m.p.changedRows
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			deltas.mu.Lock()
			deltas.n[q.Board]++
			deltas.mu.Unlock()
			return walk(q)
		}
	})
	hidden := m.subscribe(t, testBoard)
	m.subscribe(t, "2")
	clock := func(n int) {
		t.Helper()
		target := deltas.count("2") + n
		for deltas.count("2") < target {
			m.p.waitCall()
		}
	}
	clock(2)
	if err := m.svc.SetSubscriptionVisible(hidden.ID, false); err != nil {
		t.Fatal(err)
	}
	for !counts.idle(string(testBoard)) {
		clock(1) // a tick in flight at the vote ends
	}
	reads, stored := deltas.count(testBoard), counts.of(string(testBoard))
	clock(5)
	if deltas.count(testBoard) != reads || counts.of(string(testBoard)) != stored {
		t.Fatalf("a hidden board made %d requests and %d store calls over five ticks of a shown one",
			deltas.count(testBoard)-reads, counts.of(string(testBoard))-stored)
	}
	if err := m.svc.SetSubscriptionVisible(hidden.ID, true); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); deltas.count(testBoard) == reads; m.p.waitCall() {
		if time.Now().After(deadline) {
			t.Fatal("the board shown again did not catch up")
		}
	}
}

// heldCalls makes every call of names wait until its context ends,
// telling arrived of each; the others run.
func heldCalls(p *scripted, arrived chan<- struct{}, names ...string) {
	p.set(func(p *scripted) {
		p.block = func(ctx context.Context, called string) error {
			if !slices.Contains(names, called) {
				return nil
			}
			arrived <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
	})
}

// A connection turned off, removed, or off and on again ends its
// subscriptions before the call returns, with a tick and a refresh in
// flight: the refresh fails with ErrSubscriptionEnded, the subscribers are
// told once, the handles are gone, and the new mirror holds nothing of
// the old tick.
func TestConnectionChangesEndSubscriptions(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"off", "removed", "off then on"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			m.p.set(func(p *scripted) {
				p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
					return tracker.Detail{Card: card(string(id), 1)}, nil
				}
			})
			board := m.subscribe(t, testBoard)
			m.events.boardEvents(t, 1)
			arrived := make(chan struct{}, 4)
			heldCalls(m.p, arrived, "ChangedRows", "Details")
			ticket, err := m.svc.SubscribeTicket(m.conn.ID, "10001", untied)
			if err != nil {
				t.Fatal(err)
			}
			<-arrived // the ticket's first read
			p := m.svc.boardPumpOf(m.scope())
			refreshed := make(chan error, 1)
			go func() { refreshed <- m.svc.RefreshSubscription(context.Background(), board.ID) }()
			<-arrived // the refresh's delta
			switch change {
			case "removed":
				err = m.svc.Remove(m.conn.ID)
			default:
				_, err = m.svc.SetEnabled(m.conn.ID, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.exited:
			default:
				t.Fatal("the board's pump was running when the change returned")
			}
			if err := <-refreshed; !errors.Is(err, ErrSubscriptionEnded) {
				t.Fatalf("a refresh in flight: %v", err)
			}
			boards, issues, _ := m.events.snapshot()
			ended := slices.DeleteFunc(slices.Clone(boards), func(e BoardEvent) bool { return !e.Ended })
			if len(ended) != 1 || !boards[len(boards)-1].Ended {
				t.Fatalf("board events %+v; want one end, last", boards)
			}
			if len(issues) == 0 || !issues[len(issues)-1].Ended {
				t.Fatalf("issue events %+v; want the end", issues)
			}
			for _, id := range []string{board.ID, ticket.ID} {
				if err := m.svc.SetSubscriptionVisible(id, true); !errors.Is(err, ErrUnknownSubscription) {
					t.Fatalf("handle %s after the change: %v", id, err)
				}
			}
			if change != "off then on" {
				return
			}
			m.p.set(func(p *scripted) { p.block = nil })
			if _, err := m.svc.SetEnabled(m.conn.ID, true); err != nil {
				t.Fatal(err)
			}
			// The old tick stored nothing anywhere: no mirror until a new tick.
			if root := m.root(); root != 0 {
				t.Fatalf("turning the connection on again found mirror %d", root)
			}
			if _, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, untied); err != nil {
				t.Fatalf("a subscribe after turning it on again: %v", err)
			}
		})
	}
	t.Run("subscribe while off", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, untied); !errors.Is(err, ErrDisabled) {
			t.Fatalf("a board of a connection turned off: %v", err)
		}
		if _, err := m.svc.SubscribeTicket(m.conn.ID, "10001", untied); !errors.Is(err, ErrDisabled) {
			t.Fatalf("a ticket of a connection turned off: %v", err)
		}
	})
}

// A board no project lists any more ends its subscription at its next
// tick, telling the subscribers once.
func TestUnlistedBoardEndsItsSubscription(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	sub := m.subscribe(t, "2")
	m.events.boardEvents(t, 1)
	m.listBoards(m.conn.ID, testBoard)
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); !errors.Is(err, ErrSubscriptionEnded) {
		t.Fatalf("a refresh of an unlisted board: %v", err)
	}
	b, _, _ := m.events.snapshot()
	if !b[len(b)-1].Ended || slices.ContainsFunc(b[:len(b)-1], func(e BoardEvent) bool { return e.Ended }) {
		t.Fatalf("events %+v; want one end, last", b)
	}
}

// A changed card of an open ticket's issue stored by a board's tick makes
// the ticket read at once; its view changing is told once.
func TestAChangedCardWakesTheOpenTicket(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	held := card("10001", 1_799_000_000_000)
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	m.p.updatedMovesWithDetail(cardsAnswering(&held))
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{Card: held}, nil }
	})
	board := m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	ticket, err := m.svc.SubscribeTicket(m.conn.ID, "10001", untied)
	if err != nil || ticket.Loaded {
		t.Fatalf("ticket answer %+v, %v", ticket, err)
	}
	if e := m.events.issueEvents(t, 1)[0]; e.Issue != "10001" || !e.Changed || e.Seq <= ticket.Seq {
		t.Fatalf("the first read's event %+v", e)
	}
	held.Updated, held.Title = held.Updated.Add(time.Minute), "Renamed"
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	reads := m.p.count("Details")
	if err := m.svc.RefreshSubscription(t.Context(), board.ID); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, m.p, "Details", reads+1)
	issues := m.events.issueEvents(t, 2)
	if issues[1].Issue != "10001" || !issues[1].Changed {
		t.Fatalf("the woken read's event %+v", issues[1])
	}
	v, err := m.svc.TicketDetail(ticket.ID)
	if err != nil || v.Detail == nil || v.Detail.Card.Title != "Renamed" {
		t.Fatalf("the ticket's detail %+v, %v", v, err)
	}
}

// On a tracker whose update time does not move with a comment, an open
// ticket picks a new comment up at its next round with no card change,
// from the record of an issue the mirror keeps by its board or for the
// ticket alone. The test runs each round itself.
func TestAnOpenTicketPicksUpANewComment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		issue   tracker.IssueID
		onBoard bool
	}{
		{"kept by its board", "10001", true},
		{"kept for the ticket", "20001", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			issue := tc.issue
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			if tc.onBoard {
				m.subscribe(t, testBoard)
				m.events.boardEvents(t, 1)
			}
			var mu sync.Mutex
			comments := []tracker.Comment{{ID: "1", Author: "acc-user1", Body: "First", Created: time.UnixMilli(1_799_000_000_000)}}
			m.p.set(func(p *scripted) {
				p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
					mu.Lock()
					defer mu.Unlock()
					d := tracker.Detail{Card: card(string(id), 1_799_000_000_000)}
					d.Comments.Comments = slices.Clone(comments)
					return d, nil
				}
			})
			sub, err := m.svc.SubscribeTicket(m.conn.ID, issue, untied)
			if err != nil {
				t.Fatal(err)
			}
			m.events.issueEvents(t, 1)
			m.settledTickets(t)
			// Rounds that find no change.
			m.round(t)
			m.round(t)
			if n := m.p.count("Details"); n != 3 {
				t.Fatalf("the opening and two rounds read %d details", n)
			}
			if _, issues, _ := m.events.snapshot(); len(issues) != 1 {
				t.Fatalf("reads that found no change told %+v", issues)
			}
			mu.Lock()
			comments = append(comments, tracker.Comment{ID: "2", Author: "acc-user1", Body: "Second", Created: time.UnixMilli(1_799_000_100_000)})
			mu.Unlock()
			m.round(t)
			if e := m.events.issueEvents(t, 2)[1]; e.Issue != issue || !e.Changed {
				t.Fatalf("the new comment's event %+v", e)
			}
			v, err := m.svc.TicketDetail(sub.ID)
			if err != nil || v.Detail == nil || len(v.Detail.Comments) != 2 {
				t.Fatalf("the ticket's detail %+v: %v", v, err)
			}
		})
	}
}

// A subscribe's answer and the events around it order: a change stored
// after the answer read the mirror has an event numbered above the
// answer, and one stored after the answer took its number but before it
// read is in the answer, its event numbered above it too and harmless.
func TestSubscribeAnswerOrdersWithEvents(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	rows := []tracker.Row{row("10001", 1_799_000_000_000, "a")}
	m.tracks(rows)
	first := m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)

	// A tick held at its delta read while a second subscribe answers.
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	m.p.set(func(p *scripted) {
		p.block = func(ctx context.Context, name string) error {
			if name == "ChangedRows" {
				arrived <- struct{}{}
				<-release
			}
			return nil
		}
	})
	rows = append(rows, row("10002", 1_799_000_000_001, "b"))
	m.tracks(rows)
	refreshed := make(chan error, 1)
	go func() { refreshed <- m.svc.RefreshSubscription(context.Background(), first.ID) }()
	<-arrived
	second := m.subscribe(t, testBoard)
	if hasRow(second.View, "10002") {
		t.Fatal("the answer shows a row stored after it")
	}
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	after := m.events.boardEvents(t, 2)[1]
	if after.Seq <= second.Seq || !slices.Contains(after.Changed, "10002") {
		t.Fatalf("event %+v after answer %d", after, second.Seq)
	}

	// A change stored between the answer's number and its read.
	m.p.set(func(p *scripted) { p.block = nil })
	rows = append(rows, row("10003", 1_799_000_000_002, "c"))
	m.tracks(rows)
	m.svc.store = &readHook{Store: m.st, before: func() {
		must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	}}
	third := m.subscribe(t, testBoard)
	m.svc.store = m.st
	if !hasRow(third.View, "10003") {
		t.Fatal("the answer misses a row stored before its read")
	}
	b, _, _ := m.events.snapshot()
	last := b[len(b)-1]
	if last.Seq <= third.Seq || !slices.Contains(last.Changed, "10003") {
		t.Fatalf("event %+v around answer %d", last, third.Seq)
	}
	// Applying it to the answer is harmless: the patch gives the state the
	// answer shows.
	held := heldFromView(*third.View, []Section{{Kind: SectionBacklog}})
	patch := must[BoardPatch](t)(m.svc.BoardPatch(m.scope(), held.sectionList(), last.Changed, held.held()))
	patched := applyPatch(held, last.Changed, patch)
	if !patched.sameRows(held) {
		t.Fatalf("an event the answer covers changed it:\n%v\n%v", patched.describe(), held.describe())
	}
	for i := 1; i < len(b); i++ {
		if b[i].Seq <= b[i-1].Seq {
			t.Fatalf("events out of order: %+v", b)
		}
	}
}

// readHook runs before once before the first board view read.
type readHook struct {
	*store.Store
	once   sync.Once
	before func()
}

func (h *readHook) ReadMirrorBoardView(root int64, board string, regroup store.MirrorRegroup, sections []store.MirrorSection) (store.MirrorBoardView, bool, error) {
	h.once.Do(h.before)
	return h.Store.ReadMirrorBoardView(root, board, regroup, sections)
}

func hasRow(v *BoardView, id string) bool {
	if v == nil {
		return false
	}
	for _, sec := range v.Sections {
		for _, r := range sec.Rows {
			if r.Card.ID == id {
				return true
			}
		}
	}
	return false
}

// Stop with pumps and the loop running joins them all and refuses what
// follows.
func TestStopJoinsPumpsAndLoop(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: 5 * time.Millisecond, Ticket: 5 * time.Millisecond, Linked: 5 * time.Millisecond})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.p.set(func(p *scripted) {
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			return tracker.Detail{Card: card(string(id), 1)}, nil
		}
	})
	answerCardsFromDetails(m.p)
	m.subscribe(t, testBoard)
	m.subscribe(t, "2")
	if _, err := m.svc.SubscribeTicket(m.conn.ID, "10001", untied); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, m.p, "ChangedRows", 6)
	m.svc.subMu.Lock()
	var exited []<-chan struct{}
	for _, p := range m.svc.subs.boards {
		exited = append(exited, p.exited)
	}
	for _, p := range m.svc.subs.tickets {
		exited = append(exited, p.exited)
	}
	for _, rd := range m.svc.subs.rounds {
		exited = append(exited, rd.done)
	}
	if len(m.svc.subs.rounds) != 1 {
		t.Errorf("%d ticket rounds for one shown ticket", len(m.svc.subs.rounds))
	}
	exited = append(exited, m.svc.subs.loop.done)
	m.svc.subMu.Unlock()
	m.svc.Stop()
	for i, c := range exited {
		select {
		case <-c:
		default:
			t.Fatalf("goroutine %d of %d runs after Stop", i, len(exited))
		}
	}
	if _, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, untied); !errors.Is(err, ErrStopped) {
		t.Fatalf("a subscribe after Stop: %v", err)
	}
	if b, _, _ := m.events.snapshot(); slices.ContainsFunc(b, func(e BoardEvent) bool { return e.Ended }) {
		t.Fatal("Stop told the subscribers their subscriptions ended")
	}
}
