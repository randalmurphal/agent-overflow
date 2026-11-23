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

// The crossings of an open ticket's reads and view rebuilds. A view is
// built from the ticket's document, ordered by when reads began, and from
// what the mirror holds of other issues, ordered by the change funnel;
// whichever crosses which, the view told last is the newest document with
// the newest linked card, and no view is told that an earlier told view
// already outdated.

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

// crossFixture is an open ticket, 10001, that the mirror does not keep, so
// its pump holds its content, linking 10002, which the mirror keeps, on a
// tracker that reads every ticket in full. rename stores a new title of
// 10002 as a board read does; describe sets the description the tracker
// answers for 10001; failing fails its reads.
type crossFixture struct {
	*mirrorFixture
	sub     TicketSubscription
	hold    *cardsHold
	linked  tracker.Card
	desc    atomic.Value
	failing atomic.Bool
	// before is the issue events told before the test's crossing.
	before int
}

func newCrossFixture(t *testing.T) *crossFixture {
	t.Helper()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	f := &crossFixture{mirrorFixture: m, linked: card("10002", 1_799_000_000_000)}
	f.desc.Store("v1")
	m.tracks([]tracker.Row{{Card: f.linked, Order: "b"}})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			if f.failing.Load() {
				return tracker.Detail{}, errors.New("test: the tracker refused")
			}
			d := detailOfCard(card("10001", 1_799_000_000_000))
			d.Description = f.desc.Load().(string)
			return d, nil
		}
	})
	answerCardsFromDetails(m.p)
	f.sub = must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001"))
	m.events.issueEvents(t, 1)
	m.settledTickets(t)
	f.hold = &cardsHold{Store: m.st, ended: make(chan struct{})}
	t.Cleanup(func() { close(f.hold.ended) })
	m.svc.store = f.hold
	f.before = m.issueEventCount()
	return f
}

// rename stores title as 10002's, as a board read does.
func (f *crossFixture) rename(t *testing.T, title string) {
	t.Helper()
	f.linked.Title, f.linked.Updated = title, f.linked.Updated.Add(time.Minute)
	f.tracks([]tracker.Row{{Card: f.linked, Order: "b"}})
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

// A document another read stores in the mirror while the pump holds the
// ticket's: the view is the document whose read began later. A newer
// mirror's copy is shown at once, with no tracker call, and the pump lets
// its own go; an older one, stored by a read that began before the pump's,
// changes nothing.
func TestAHeldTicketShowsTheNewerOfItsCopyAndTheMirrors(t *testing.T) {
	t.Parallel()
	// held is the fixture once a board keeps 10001, whose document the pump
	// still holds: no read of it since was an opening.
	held := func(t *testing.T) *crossFixture {
		f := newCrossFixture(t)
		f.tracks([]tracker.Row{{Card: card("10001", 1_799_000_000_000), Order: "a"}, {Card: f.linked, Order: "b"}})
		must[Outcome](t)(f.svc.tickDelta(t.Context(), f.scope(), Background))
		quiet(t, f.svc)
		if !f.pumpHolds() {
			t.Fatal("the pump does not hold the ticket's document")
		}
		f.before = f.issueEventCount()
		return f
	}
	changed := func(f *crossFixture) int {
		n := 0
		for _, e := range f.issueEventsSince(f.before) {
			if e.Changed {
				n++
			}
		}
		return n
	}
	t.Run("the mirror's is newer", func(t *testing.T) {
		t.Parallel()
		f := held(t)
		f.desc.Store("v2")
		calls := len(f.p.callsInOrder())
		v, ch, err := f.svc.RefreshIssueDetail(t.Context(), f.conn.ID, "10001", Interactive)
		if err != nil || v.Description != "v2" || !slices.Contains(ch.Details, tracker.IssueID("10001")) {
			t.Fatalf("the refresh %+v, %+v, %v", v.Description, ch, err)
		}
		quiet(t, f.svc)
		if got := f.ticketView(t, f.sub.ID); !got.Loaded || got.Detail.Description != "v2" {
			t.Fatalf("the open ticket shows %+v", got.Detail)
		}
		if f.pumpHolds() {
			t.Fatal("the pump still holds a copy of the document the mirror holds")
		}
		if n := changed(f); n != 1 {
			t.Fatalf("%d views told; want 1", n)
		}
		if got := describeCounts(countsOf(f.p.callsInOrder()[calls:])); got != "Details:1" {
			t.Fatalf("the refresh and the rebuild called %s", got)
		}
	})
	t.Run("the mirror's is older", func(t *testing.T) {
		t.Parallel()
		f := held(t)
		// The refresh begins and is answered v1, the round reads v2 and the
		// pump holds it, then the refresh stores its answer.
		call := holdNext(f.p, "Details")
		done := make(chan error, 1)
		go func() {
			_, _, err := f.svc.RefreshIssueDetail(context.Background(), f.conn.ID, "10001", Interactive)
			done <- err
		}()
		<-call.arrived
		f.desc.Store("v2")
		if res := f.round(t); res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(call.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, d, ok := f.storedDetail(t, "10001"); !ok || d.Description != "v1" {
			t.Fatalf("the mirror's copy %+v, %v", d.Description, ok)
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

// pumpHolds reports whether 10001's pump holds its document.
func (f *crossFixture) pumpHolds() bool {
	tp := f.svc.ticketPumpOf(f.conn.ID, "10001")
	f.svc.subMu.Lock()
	defer f.svc.subMu.Unlock()
	return tp.check != nil && tp.check.held != nil && tp.held != nil
}

// A rebuild that crosses a read: the read built its view with the old
// linked card, a rebuild told the new one with the old document, then the
// read settles its new document. A read that crosses a rebuild: the
// rebuild built from the old document, the read settles a new one first.
// Two rebuilds: the earlier one built with a card a later change replaced.
// A rebuild that crosses a failed read: the failure moves no document.
func TestViewBuildsCrossing(t *testing.T) {
	t.Parallel()
	t.Run("a rebuild crossing a read", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.desc.Store("v2")
		done := make(chan ticketsResult, 1)
		go func() { done <- f.round(t) }()
		<-held.arrived
		events := f.issueEventCount()
		f.rename(t, "Renamed")
		f.events.issueEvents(t, events+1)
		close(held.release)
		if res := <-done; res.failed {
			t.Fatalf("the read %+v", res)
		}
		// The rename's view, then the new document's: never the read's
		// view of the old linked card.
		f.shows(t, "v2", "Renamed", 2)
	})
	t.Run("a read crossing a rebuild", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "Renamed")
		<-held.arrived
		f.desc.Store("v2")
		if res := f.round(t); res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(held.release)
		f.shows(t, "v2", "Renamed", 1)
	})
	t.Run("two rebuilds", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "Renamed")
		<-held.arrived
		f.rename(t, "Renamed again")
		close(held.release)
		f.shows(t, "v1", "Renamed again", 1)
	})
	t.Run("a rebuild crossing a failed read", func(t *testing.T) {
		t.Parallel()
		f := newCrossFixture(t)
		held := f.hold.arm()
		f.rename(t, "Renamed")
		<-held.arrived
		f.failing.Store(true)
		if res := f.round(t); !res.failed {
			t.Fatalf("the read %+v", res)
		}
		close(held.release)
		f.shows(t, "v1", "Renamed", 1)
	})
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

// A rebuild that left the mirror's newer copy to a read of the ticket in
// flight builds it once that read ends, however it ends: stopped before
// the tracker answered, or settled behind a read that began later. The
// pump is hidden, so only the read's settlement rebuilds.
func TestARebuildLeftToAReadBuildsWhenTheReadEnds(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*mirrorFixture, *trackerState, *ticketPump) {
		m, st, issues, subs := newRoundFixture(t, 1, false)
		if err := m.svc.SetSubscriptionVisible(subs[0].ID, false); err != nil {
			t.Fatal(err)
		}
		return m, st, m.pumpOf(issues[0])
	}
	// another stores body as the ticket's document by a read outside the
	// pump, and rebuilds the view while the pump's read is in flight.
	another := func(t *testing.T, m *mirrorFixture, st *trackerState, tp *ticketPump, body string) {
		st.comment(tp.key.issue, body)
		if _, _, err := m.svc.RefreshIssueDetail(t.Context(), m.conn.ID, tp.key.issue, Interactive); err != nil {
			t.Fatal(err)
		}
		events := m.issueEventCount()
		m.svc.rebuildTicket(tp)
		if n := m.changedSince(events); n != 0 {
			t.Fatalf("a rebuild told %d views of another read's document while a read was in flight", n)
		}
	}
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		m, st, tp := setup(t)
		call := holdNext(m.p, "Details")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan ticketsResult, 1)
		go func() { done <- m.svc.readTickets(ctx, m.conn.ID, []*ticketPump{tp}, Background, false, false) }()
		<-call.arrived
		events := m.issueEventCount()
		another(t, m, st, tp, "Second")
		cancel()
		<-done
		if n := m.changedSince(events); n != 1 {
			t.Fatalf("%d views told once the read stopped; want the mirror's copy", n)
		}
	})
	t.Run("settled behind a later read", func(t *testing.T) {
		t.Parallel()
		m, st, tp := setup(t)
		call := holdNext(m.p, "Details")
		done := make(chan ticketsResult, 1)
		go func() { done <- m.svc.readTickets(t.Context(), m.conn.ID, []*ticketPump{tp}, Background, false, false) }()
		<-call.arrived
		events := m.issueEventCount()
		st.comment(tp.key.issue, "Second")
		if res := m.svc.readTickets(t.Context(), m.conn.ID, []*ticketPump{tp}, Background, true, false); res.failed {
			t.Fatalf("the later read %+v", res)
		}
		another(t, m, st, tp, "Third")
		close(call.release)
		<-done
		if n := m.changedSince(events); n != 2 {
			t.Fatalf("%d views told; want the later read's and the mirror's copy", n)
		}
	})
}
