package trackerapp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The crossings of an open ticket's reads and view builds. Every view is
// built from the record, and a build installs only a view begun after the
// last change told to the ticket (buildTickets): whichever crosses which,
// the view told last is the record's, and no view is told that a change
// told during its build outdated.

// cardsHold holds the next view build at its read of the mirror's cards,
// once armed, after the read returned: the build has its snapshot of the
// linked card and has not installed its view. A hold ends with the test
// (ended), so a failure before the release leaves no build waiting.
type cardsHold struct {
	*store.Store
	ended   chan struct{}
	mu      sync.Mutex
	armed   *heldCall
	pending bool
}

func (h *cardsHold) arm() *heldCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed = &heldCall{arrived: make(chan struct{}), release: make(chan struct{})}
	h.pending = true
	return h.armed
}

func (h *cardsHold) ListMirrorCards(root int64, ids []string) ([]store.MirrorCard, error) {
	cards, err := h.Store.ListMirrorCards(root, ids)
	h.mu.Lock()
	held := h.pending
	h.pending = false
	c := h.armed
	h.mu.Unlock()
	if held {
		close(c.arrived)
		select {
		case <-c.release:
		case <-h.ended:
		}
	}
	return cards, err
}

// crossFixture is an open ticket, 10001, linking the issue link names
// (10002 at first, or 10003), both of which a board keeps, on a tracker
// that reads every ticket in full. rename stores a new title of the linked
// issue as a board read does; describe sets the description the tracker
// answers for 10001; failing fails its reads.
type crossFixture struct {
	*mirrorFixture
	sub     TicketSubscription
	hold    *cardsHold
	linked  map[tracker.IssueID]tracker.Card
	link    atomic.Value
	desc    atomic.Value
	failing atomic.Bool
	// before is the issue events told before the test's crossing.
	before int
}

func newCrossFixture(t *testing.T) *crossFixture {
	t.Helper()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	f := &crossFixture{mirrorFixture: m, linked: map[tracker.IssueID]tracker.Card{
		"10002": card("10002", 1_799_000_000_000), "10003": card("10003", 1_799_000_000_000)}}
	f.desc.Store("v1")
	f.link.Store(tracker.IssueID("10002"))
	f.board()
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			if f.failing.Load() {
				return tracker.Detail{}, errors.New("test: the tracker refused")
			}
			// A history final for the card's update: the ticket shows the
			// time in its status from its document, so no round reads the
			// mirror's histories for it (roundHistory).
			d := enteredAtStatusBegan(detailOfCard(card("10001", 1_799_000_000_000)))
			d.HistoryTo = d.Card.Updated
			d.Description = f.desc.Load().(string)
			d.Links[0].Issue.ID = f.link.Load().(tracker.IssueID)
			return d, nil
		}
	})
	answerCardsFromDetails(m.p)
	f.sub = must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied))
	m.events.issueEvents(t, 1)
	m.settledTickets(t)
	quiet(t, m.svc)
	f.hold = &cardsHold{Store: m.st, ended: make(chan struct{})}
	t.Cleanup(func() { close(f.hold.ended) })
	m.svc.store = f.hold
	f.before = m.issueEventCount()
	return f
}

// board has a board read store the linked issues' cards.
func (f *crossFixture) board() {
	f.tracks([]tracker.Row{{Card: f.linked["10002"], Order: "b"}, {Card: f.linked["10003"], Order: "c"}})
}

// rename stores title as the linked issue id's, as a board read does.
func (f *crossFixture) rename(t *testing.T, id tracker.IssueID, title string) {
	t.Helper()
	c := f.linked[id]
	c.Title, c.Updated = title, c.Updated.Add(time.Minute)
	f.linked[id] = c
	f.board()
	must[Outcome](t)(f.svc.tickDelta(t.Context(), f.scope(), Background))
}

// shows fails the test unless the view told last shows desc and the
// linked title, after changes views told since the crossing began: each
// view the crossing made newer, and no other.
func (f *crossFixture) shows(t *testing.T, desc, title string, changes int) {
	t.Helper()
	// The pump takes a queued nudge only between builds: once it took this
	// one, a build it was making has settled. The nudge's own build finds
	// nothing new.
	tp := f.svc.ticketPumpOf(f.conn.ID, "10001")
	nudge(tp.viewChanged)
	deadline := time.Now().Add(20 * time.Second)
	for len(tp.viewChanged) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pump did not take its nudge")
		}
		time.Sleep(time.Millisecond)
	}
	quiet(t, f.svc)
	v := f.ticketView(t, f.sub.ID)
	if !v.Loaded || v.Detail.Description != desc || v.Detail.Links[0].Issue.Title != title {
		t.Fatalf("the view shows %q linking %q; want %q linking %q", v.Detail.Description, v.Detail.Links[0].Issue.Title, desc, title)
	}
	var told []IssueEvent
	for _, e := range f.issueEventsSince(f.before) {
		if e.Changed {
			told = append(told, e)
		}
	}
	if len(told) != changes {
		t.Fatalf("%d views told %+v; want %d", len(told), told, changes)
	}
}

// A read of the open ticket that no pump makes (IssueDetail) stores what
// it found, which the ticket is told as any change: a newer document is
// shown at once with no tracker call of the ticket's own; an older one,
// from a read that began before the round's, is superseded in the record
// and changes nothing.
func TestADirectReadOfAnOpenTicketReachesItThroughTheRecord(t *testing.T) {
	t.Parallel()
	changed := func(f *crossFixture) int {
		n := 0
		for _, e := range f.issueEventsSince(f.before) {
			if e.Changed {
				n++
			}
		}
		return n
	}
	t.Run("a newer document", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		f.desc.Store("v2")
		calls := len(f.p.callsInOrder())
		v, ch, err := f.svc.issueDetail(t.Context(), f.conn.ID, "10001", Interactive, true)
		if err != nil || v.Description != "v2" || !slices.Contains(ch.Details, tracker.IssueID("10001")) {
			t.Fatalf("the read %+v, %+v, %v", v.Description, ch, err)
		}
		quiet(t, f.svc)
		if got := f.ticketView(t, f.sub.ID); !got.Loaded || got.Detail.Description != "v2" {
			t.Fatalf("the open ticket shows %+v", got.Detail)
		}
		if n := changed(f); n != 1 {
			t.Fatalf("%d views told; want 1", n)
		}
		if got := describeCounts(countsOf(f.p.callsInOrder()[calls:])); got != "Details:1" {
			t.Fatalf("the read and the build called %s", got)
		}
	})
	t.Run("an older document", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		// The direct read begins and is answered v1, the round reads v2,
		// then the direct read stores its answer.
		call := holdNext(f.p, "Details")
		done := make(chan error, 1)
		go func() {
			_, _, err := f.svc.issueDetail(context.Background(), f.conn.ID, "10001", Interactive, true)
			done <- err
		}()
		waitClosed(t, "the held call", call.arrived)
		f.desc.Store("v2")
		if res := f.round(t); res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(call.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, d, ok := f.storedDetail(t, "10001"); !ok || d.Description != "v2" {
			t.Fatalf("the record's body %+v, %v", d.Description, ok)
		}
		quiet(t, f.svc)
		if got := f.ticketView(t, f.sub.ID); !got.Loaded || got.Detail.Description != "v2" {
			t.Fatalf("the open ticket shows %+v", got.Detail)
		}
		if n := changed(f); n != 1 {
			t.Fatalf("%d views told; want 1, the round's", n)
		}
	})
}

// A build that crosses a change: a board read renames the linked issue
// while the read's build is between its read of the record and its
// install, so that build installs nothing and the ticket is built again;
// a read that crosses a nudge's build likewise; two changes, the second
// during the first one's build; a build that crosses a failed read, whose
// failure goes out with the build's view. In each, one view is told, the
// record's.
func TestViewBuildsCrossing(t *testing.T) {
	t.Parallel()
	t.Run("a change crossing a read's build", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.desc.Store("v2")
		done := make(chan ticketsResult, 1)
		go func() { done <- f.round(t) }()
		waitClosed(t, "the held build", held.arrived)
		f.rename(t, "10002", "Renamed")
		close(held.release)
		if res := <-done; res.failed {
			t.Fatalf("the read %+v", res)
		}
		f.shows(t, "v2", "Renamed", 1)
	})
	t.Run("a read crossing a nudge's build", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "10002", "Renamed")
		waitClosed(t, "the held build", held.arrived)
		f.desc.Store("v2")
		if res := f.round(t); res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(held.release)
		f.shows(t, "v2", "Renamed", 1)
	})
	t.Run("two changes", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "10002", "Renamed")
		waitClosed(t, "the held build", held.arrived)
		f.rename(t, "10002", "Renamed again")
		close(held.release)
		f.shows(t, "v1", "Renamed again", 1)
	})
	t.Run("a build crossing a failed read", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "10002", "Renamed")
		waitClosed(t, "the held build", held.arrived)
		f.failing.Store(true)
		if res := f.round(t); !res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(held.release)
		f.shows(t, "v1", "Renamed", 1)
		if v := f.ticketView(t, f.sub.ID); v.Failure == nil {
			t.Fatal("the failed read's failure was not told")
		}
	})
}

// A build whose view shows an issue the told view does not (the ticket
// now links 10003) is told every card change of the connection while it
// is in flight: 10003 renamed between the build's read of the record and
// its install is not missed though the change named no issue the told
// view shows, and the ticket is built again.
func TestABuildIsToldAChangeToAnIssueItAddsToTheView(t *testing.T) {
	t.Parallel()
	f := newCrossFixture(t)
	held := f.hold.arm()
	f.link.Store(tracker.IssueID("10003"))
	done := make(chan ticketsResult, 1)
	go func() { done <- f.round(t) }()
	waitClosed(t, "the held build", held.arrived)
	f.rename(t, "10003", "Renamed")
	close(held.release)
	if res := <-done; res.failed {
		t.Fatalf("the read %+v", res)
	}
	f.shows(t, "v1", "Renamed", 1)
}

// A board read that changes the linked issue while the round reads the
// ticket, after the ticket's read failed: the change waits for the read,
// which builds the view as it settles, so one event carries the recovery
// with the new view, never the new view with the failure the read clears.
// A build of the connection's views made meanwhile (another ticket's
// nudge, a pane shown again) leaves the ticket to its read.
func TestAChangeDuringAReadIsToldWithTheReadsOutcome(t *testing.T) {
	t.Parallel()
	f := newCrossFixture(t)
	f.failing.Store(true)
	if res := f.round(t); !res.failed {
		t.Fatalf("the failing read %+v", res)
	}
	f.failing.Store(false)
	quiet(t, f.svc)
	events := f.issueEventCount()
	call := holdNext(f.p, "Details")
	done := make(chan ticketsResult, 1)
	go func() { done <- f.round(t) }()
	waitClosed(t, "the held call", call.arrived)
	f.rename(t, "10002", "Renamed")
	tp := f.svc.ticketPumpOf(f.conn.ID, "10001")
	f.svc.subMu.Lock()
	owed := tp.dirty != tp.built
	f.svc.subMu.Unlock()
	if !owed {
		t.Fatal("the change was not counted")
	}
	f.svc.buildShown(f.conn.ID)
	if got := f.issueEventsSince(events); len(got) != 0 {
		t.Fatalf("a build while the read is in flight told %+v", got)
	}
	close(call.release)
	if res := <-done; res.failed {
		t.Fatalf("the read %+v", res)
	}
	quiet(t, f.svc)
	got := f.issueEventsSince(events)
	if len(got) != 1 || !got[0].Changed || got[0].Failure != nil {
		t.Fatalf("the read and the change told %+v; want one event with the new view and no failure", got)
	}
	if v := f.ticketView(t, f.sub.ID); v.Detail.Links[0].Issue.Title != "Renamed" {
		t.Fatalf("the view links %q", v.Detail.Links[0].Issue.Title)
	}
}

// changedSince are the issue events after the first n that told a view.
func (m *mirrorFixture) changedSince(n int) int {
	changed := 0
	for _, e := range m.issueEventsSince(n) {
		if e.Changed && e.Failure == nil {
			changed++
		}
	}
	return changed
}

// A change told while a read of the hidden ticket is in flight is built
// once that read ends, however it ends: stopped before the tracker
// answered, or settled behind a read that began later. The pump is hidden
// and the read in flight, so a nudge builds nothing; the read's
// settlement builds it.
func TestAChangeToldDuringAReadIsBuiltWhenTheReadEnds(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*mirrorFixture, *trackerState, *ticketPump) {
		m, st, issues, subs := newRoundFixture(t, 1, false)
		if err := m.svc.SetSubscriptionVisible(subs[0].ID, false); err != nil {
			t.Fatal(err)
		}
		return m, st, m.pumpOf(issues[0])
	}
	// another stores body as the ticket's document by a read outside the
	// pump, and has a nudge build the connection's views while the pump's
	// read is in flight.
	another := func(t *testing.T, m *mirrorFixture, st *trackerState, tp *ticketPump, body string) {
		st.comment(tp.key.issue, body)
		if _, _, err := m.svc.issueDetail(t.Context(), m.conn.ID, tp.key.issue, Interactive, true); err != nil {
			t.Fatal(err)
		}
		events := m.issueEventCount()
		m.svc.buildShown(m.conn.ID)
		if n := m.changedSince(events); n != 0 {
			t.Fatalf("a nudge told %d views while a read was in flight", n)
		}
	}
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		m, st, tp := setup(t)
		call := holdNext(m.p, "Details")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan ticketsResult, 1)
		go func() { done <- m.svc.readTickets(ctx, m.conn.ID, []*ticketPump{tp}, Background, false, false) }()
		waitClosed(t, "the held call", call.arrived)
		events := m.issueEventCount()
		another(t, m, st, tp, "Second")
		cancel()
		<-done
		if n := m.changedSince(events); n != 1 {
			t.Fatalf("%d views told once the read stopped; want the record's", n)
		}
	})
	t.Run("settled behind a later read", func(t *testing.T) {
		t.Parallel()
		m, st, tp := setup(t)
		call := holdNext(m.p, "Details")
		done := make(chan ticketsResult, 1)
		go func() { done <- m.svc.readTickets(t.Context(), m.conn.ID, []*ticketPump{tp}, Background, false, false) }()
		waitClosed(t, "the held call", call.arrived)
		events := m.issueEventCount()
		st.comment(tp.key.issue, "Second")
		if res := m.svc.readTickets(t.Context(), m.conn.ID, []*ticketPump{tp}, Background, true, false); res.failed {
			t.Fatalf("the later read %+v", res)
		}
		another(t, m, st, tp, "Third")
		close(call.release)
		<-done
		if n := m.changedSince(events); n != 2 {
			t.Fatalf("%d views told; want the later read's and the record's", n)
		}
	})
}
