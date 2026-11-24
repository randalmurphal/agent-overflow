package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The subscription-state model: subscribers that join at random moments
// of random reads, failures of the tracker and of the store, recoveries,
// hides and shows on two tickets and two boards of a connection, and
// random changes of every input of a ticket's view (its document, its
// card, its history, the card of the issue it links, a user's name,
// whether the mirror keeps it, and the cleanup deleting the detail of one
// it stopped keeping), each applying the events after its answer
// as a client does (an event that says the view changed is applied by
// reading TicketDetail), hold after every step, once the pumps are quiet,
// what TicketDetail answers for each shown ticket (whether it is loaded,
// and its view), and end in the state a fresh subscribe's answer gives:
// the failure, and for a ticket whether it is loaded and its view.
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
// sequence number of the last answer or event it applied, and the state
// it holds: the failure, and for a ticket whether it is loaded and a
// digest of its view.
type modelSub struct {
	ticket  tracker.IssueID
	board   tracker.BoardID
	handle  string
	applied uint64
	failure *FailureView
	loaded  bool
	digest  uint64
}

// modelView is a ticket's view as its loaded flag and a digest.
func modelView(s *Service, v TicketView) (bool, uint64) {
	if !v.Loaded {
		return false, 0
	}
	return true, s.digestOf(*v.Detail)
}

// heldView is tp's view as TicketDetail answers it, read with no side
// effect: the view the pump holds, else one built from the mirror's copy,
// not loaded when neither holds one.
func heldView(t *testing.T, s *Service, tp *ticketPump) (bool, uint64) {
	s.subMu.Lock()
	held := tp.held
	s.subMu.Unlock()
	if held != nil {
		return true, s.digestOf(held.view)
	}
	root, err := s.readRoot(tp.key.connection)
	if errors.Is(err, ErrNotMirrored) {
		return false, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.leave()
	_, d, ok, err := s.storedDetail(root, tp.key.issue)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return false, 0
	}
	row := must[store.TrackerConnection](t)(s.viewRow(tp.key.connection))
	v, _, err := s.detailView(root, row, d)
	if err != nil {
		t.Fatal(err)
	}
	return modelView(s, TicketView{Loaded: true, Detail: &v})
}

func subscriptionModel(t *testing.T, r *rand.Rand) {
	m, st, issues, _ := newRoundFixture(t, 2, r.IntN(2) == 0)
	boards := []tracker.BoardID{testBoard, "2"}
	ctx := t.Context()
	// The first ticket links the second: its view shows that card.
	st.mu.Lock()
	st.links[issues[0]] = issues[1]
	st.mu.Unlock()
	m.svc.ticketTick(m.svc.ticketPumpOf(m.conn.ID, issues[0]), Interactive, true)
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
	kept := map[tracker.IssueID]bool{issues[0]: true, issues[1]: true}
	// deliver has the boards' reads deliver the kept issues' cards as the
	// tracker holds them.
	deliver := func() {
		var rows []tracker.Row
		st.mu.Lock()
		for i, id := range issues {
			if kept[id] {
				rows = append(rows, tracker.Row{Card: st.cards[id], Order: tracker.OrderKey(fmt.Sprintf("a%03d", i))})
			}
		}
		st.mu.Unlock()
		m.tracks(rows)
	}
	// syncBoard runs units of a board's tick. The store failing is the
	// model's to make; any other error fails the test.
	syncBoard := func(scope Scope, due BoardDue) {
		if _, err := m.svc.syncTick(ctx, scope, due, Background); err != nil && !errors.Is(err, errStoreDown) {
			t.Fatal(err)
		}
	}
	var subs []*modelSub
	subscribe := func() {
		s := &modelSub{}
		if r.IntN(2) == 0 {
			s.ticket = issues[r.IntN(len(issues))]
			a := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, s.ticket))
			s.handle, s.applied, s.failure = a.ID, a.Seq, a.Failure
			s.loaded, s.digest = modelView(m.svc, a.TicketView)
		} else {
			s.board = boards[r.IntN(len(boards))]
			scope := Scope{Connection: m.conn.ID, Board: s.board}
			started := m.svc.boardPumpOf(scope) == nil
			a := must[BoardSubscription](t)(m.svc.SubscribeBoard(m.project(), scope, []Section{{Kind: SectionBacklog}}))
			s.handle, s.applied, s.failure = a.ID, a.Seq, a.Failure
			if started {
				// A new pump's first tick runs on its own; a refresh, which
				// the pump serves after it, returns once both passed what
				// they stored through the funnel. Its failure is the model's
				// to make.
				_ = m.svc.RefreshSubscription(ctx, a.ID)
			}
		}
		subs = append(subs, s)
	}
	// apply has each subscriber apply the events after the last it applied.
	apply := func() {
		boardEvents, issueEvents, _ := m.events.snapshot()
		for _, s := range subs {
			changed, last := false, s.applied
			for _, e := range boardEvents {
				if s.board != "" && e.Scope.Board == s.board && e.Seq > s.applied {
					s.failure, last = e.Sync.Failure, max(last, e.Seq)
				}
			}
			for _, e := range issueEvents {
				if s.ticket != "" && e.Issue == s.ticket && e.Seq > s.applied {
					s.failure, last = e.Failure, max(last, e.Seq)
					changed = changed || e.Changed
				}
			}
			s.applied = last
			if changed {
				s.loaded, s.digest = modelView(m.svc, must[TicketView](t)(m.svc.TicketDetail(s.handle)))
			}
		}
	}
	// current compares, once the pumps are quiet, what each subscriber of
	// a shown ticket applied with what TicketDetail would answer now
	// (heldView), asked with no side effect.
	current := func(step int) {
		quiet(t, m.svc)
		apply()
		for _, s := range subs {
			tp := m.svc.ticketPumpOf(m.conn.ID, s.ticket)
			if tp == nil {
				continue
			}
			m.svc.subMu.Lock()
			shown := tp.shownLocked()
			m.svc.subMu.Unlock()
			if !shown {
				continue
			}
			loaded, digest := heldView(t, m.svc, tp)
			if loaded != s.loaded || digest != s.digest {
				t.Fatalf("step %d, ticket %s: the subscriber holds a view loaded %v (%x); the service holds loaded %v (%x)", step, s.ticket, s.loaded, s.digest, loaded, digest)
			}
		}
	}
	read := func() {
		switch r.IntN(3) {
		case 0:
			if p := m.svc.boardPumpOf(Scope{Connection: m.conn.ID, Board: boards[r.IntN(len(boards))]}); p != nil {
				m.svc.boardTick(p, false, Background)
			}
		case 1:
			if tp := m.svc.ticketPumpOf(m.conn.ID, issues[r.IntN(len(issues))]); tp != nil {
				m.svc.ticketTick(tp, Background, false)
			}
		default:
			if rd := m.svc.roundOf(m.conn.ID); rd != nil {
				m.svc.roundTick(rd)
			}
		}
	}
	for step := range 80 {
		board := Scope{Connection: m.conn.ID, Board: boards[r.IntN(len(boards))]}
		issue := issues[r.IntN(len(issues))]
		op := r.IntN(17)
		// Logged, shown only when the model fails.
		t.Logf("step %d: op %d, issue %s, board %s", step, op, issue, board.Board)
		switch {
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
			// The card changes, and a board's read stores it.
			st.edit(issue, func(c *tracker.Card) { c.Title = fmt.Sprint("Title ", r.IntN(100)) })
			deliver()
			syncBoard(board, BoardDue{Delta: true})
		case op == 6:
			// The document changes and no card does.
			st.comment(issue, fmt.Sprint("Comment ", r.IntN(100)))
		case op == 7:
			// A history read moves the status times of the issues due one.
			at := time.UnixMilli(1_798_000_000_000 + int64(r.IntN(1000))*1000).UTC()
			m.p.set(func(p *scripted) {
				p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
					var h tracker.History
					for _, id := range q.Issues {
						h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(),
							Status: []tracker.StatusChange{{Change: tracker.Change{At: at}, From: "s2", To: "s1"}}})
					}
					return h, nil
				}
			})
			syncBoard(board, BoardDue{History: true})
		case op == 8:
			// A user is renamed, read with a board's structure.
			name := fmt.Sprint("User 1 v", step)
			m.p.set(func(p *scripted) {
				p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
					s := webStructure(b)
					s.Users = []tracker.User{user1}
					s.Users[0].Name = name
					return s, nil
				}
			})
			syncBoard(board, BoardDue{Structure: true})
		case op == 9:
			// The issue leaves both boards, or comes back to them.
			kept[issue] = !kept[issue]
			deliver()
			for _, b := range boards {
				syncBoard(Scope{Connection: m.conn.ID, Board: b}, BoardDue{Reconcile: true})
			}
		case op == 10:
			// The cleanup deletes what the mirror no longer keeps, the
			// detail of an issue that left included, telling no one.
			if err := m.svc.FinishMirrorCleanup(); err != nil {
				t.Fatal(err)
			}
		default:
			read()
		}
		current(step)
	}
	for len(subs) < 4 {
		subscribe()
	}
	// The last reads, every subscriber shown: every target read once more,
	// the failures as they stand, which also governs any read still in
	// flight.
	for _, s := range subs {
		if err := m.svc.SetSubscriptionVisible(s.handle, true); err != nil {
			t.Fatal(err)
		}
	}
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
	quiet(t, m.svc)
	apply()
	want := FailureKind("")
	switch {
	case fs.fail.Load():
		want = FailureInternal
	case trackerFail.Load():
		want = FailureTracker
	}
	for _, s := range subs {
		target := fmt.Sprint("ticket ", s.ticket, " board ", s.board)
		var fresh *FailureView
		if s.ticket != "" {
			a := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, s.ticket))
			fresh = a.Failure
			if loaded, digest := modelView(m.svc, a.TicketView); loaded != s.loaded || digest != s.digest {
				t.Errorf("%s: the subscriber holds a view loaded %v (%x); a fresh answer gives loaded %v (%x)", target, s.loaded, s.digest, loaded, digest)
			}
		} else {
			fresh = must[BoardSubscription](t)(m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: s.board}, nil)).Failure
		}
		if !sameFailure(s.failure, fresh) {
			t.Errorf("%s: the subscriber applied %+v; a fresh answer gives %+v", target, s.failure, fresh)
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
