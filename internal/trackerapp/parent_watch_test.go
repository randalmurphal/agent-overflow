package trackerapp

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// heldLookup is a mirror store that holds each read of the mirror's cards
// naming issue, after it read them, until the test releases every one held
// and every later one (releaseAll).
type heldLookup struct {
	Store
	issue   string
	arrived chan struct{}
	mu      sync.Mutex
	free    bool
	held    []chan struct{}
}

func newHeldLookup(s Store, issue string) *heldLookup {
	return &heldLookup{Store: s, issue: issue, arrived: make(chan struct{}, 64)}
}

func (h *heldLookup) ListMirrorCards(root int64, ids []string) ([]store.MirrorCard, error) {
	out, err := h.Store.ListMirrorCards(root, ids)
	if !slices.Contains(ids, h.issue) {
		return out, err
	}
	h.mu.Lock()
	if h.free {
		h.mu.Unlock()
		return out, err
	}
	rel := make(chan struct{})
	h.held = append(h.held, rel)
	h.mu.Unlock()
	h.arrived <- struct{}{}
	<-rel
	return out, err
}

// next waits for the next read held.
func (h *heldLookup) next(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not read the mirror", what)
	}
}

// releaseAll lets every read held go on, and every later one pass.
func (h *heldLookup) releaseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.free = true
	for _, rel := range h.held {
		close(rel)
	}
	h.held = nil
}

// parentFixture is an open ticket of 10001 whose board also holds 10002,
// with the mirror's reads of 10002's card held (lookups) and the ticket's
// reads waiting for reads to close.
type parentFixture struct {
	*mirrorFixture
	st      *trackerState
	sub     TicketSubscription
	lookups *heldLookup
	// reads closes when letReads lets the ticket's reads go on.
	reads    chan struct{}
	letReads func()
}

func newParentFixture(t *testing.T) *parentFixture {
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	st := &trackerState{cards: map[tracker.IssueID]tracker.Card{}, comments: map[tracker.IssueID]string{},
		links: map[tracker.IssueID]tracker.IssueID{}, gone: map[tracker.IssueID]bool{}}
	for i, id := range []string{"10001", "10002"} {
		r := row(id, 1_799_000_000_000, fmt.Sprintf("a%03d", i))
		st.cards[r.Card.ID], st.comments[r.Card.ID] = r.Card, "First"
	}
	f := &parentFixture{mirrorFixture: m, st: st}
	f.tick(t)
	m.p.set(func(p *scripted) { p.detail, p.cards = st.detail, st.cardsRead })
	f.sub = must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001"))
	m.events.issueEvents(t, 1)
	m.settledTickets(t)

	f.lookups = newHeldLookup(m.svc.store, "10002")
	t.Cleanup(f.lookups.releaseAll)
	m.svc.store = f.lookups
	f.reads = make(chan struct{})
	var once sync.Once
	f.letReads = func() { once.Do(func() { close(f.reads) }) }
	t.Cleanup(f.letReads)
	m.p.set(func(p *scripted) {
		p.block = func(ctx context.Context, name string) error {
			if name == "Details" {
				select {
				case <-f.reads:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
	})
	return f
}

// tick has a board read store the tracker's state.
func (f *parentFixture) tick(t *testing.T) {
	t.Helper()
	f.st.mu.Lock()
	rows := []tracker.Row{{Card: f.st.cards["10001"], Order: "a000"}, {Card: f.st.cards["10002"], Order: "a001"}}
	f.st.mu.Unlock()
	f.tracks(rows)
	must[Outcome](t)(f.svc.tickDelta(t.Context(), f.scope(), Background))
}

// titleGoesBackIsTold waits for the ticket's view to catch up with its
// inputs, then sets the parent's title back to original: the ticket,
// which shows the parent now, is told, and its subscribers too, unless
// the view it installed was built from the parent's card before the
// rename (the same view again).
func (f *parentFixture) titleGoesBackIsTold(t *testing.T, original string) {
	t.Helper()
	// A read in flight or due takes the changes told meanwhile
	// (buildShown): let the reads run and wait for every one to end.
	f.letReads()
	quiet(t, f.svc)
	_, issues, _ := f.events.snapshot()
	told := len(issues)
	f.st.edit("10002", func(c *tracker.Card) { c.Title = original })
	f.tick(t)
	f.events.waitFor(t, "the ticket told its parent's title went back", func(_ []BoardEvent, is []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(is[told:], func(e IssueEvent) bool { return e.Issue == "10001" && e.Changed })
	})
	quiet(t, f.svc)
	if v := f.ticketView(t, f.sub.ID); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Title != original {
		t.Fatalf("the ticket shows its parent as %+v; want %q", v.Detail, original)
	}
}

// An open ticket whose issue was given a parent builds a view that shows
// the parent's card, read from the mirror; the parent's title changes
// after that read and before the view and its dependencies are installed,
// when the funnel judged the change by the ticket's old dependencies. The
// ticket still comes to show the parent's new title.
func TestAParentChangedBeforeTheTicketWatchesItShows(t *testing.T) {
	t.Parallel()
	f := newParentFixture(t)
	// The read the reparent makes due waits to the end: only rebuilds,
	// which read the mirror, build the ticket's view meanwhile.
	original := f.st.cards["10002"].Title
	f.st.edit("10001", func(c *tracker.Card) { c.Parent = "10002" })
	f.tick(t)
	f.lookups.next(t, "the ticket's rebuild")
	f.st.edit("10002", func(c *tracker.Card) { c.Title = "Parent renamed" })
	f.tick(t)
	_, before, _ := f.events.snapshot()
	f.lookups.releaseAll()
	f.events.waitFor(t, "the ticket told of its new parent", func(_ []BoardEvent, is []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(is[len(before):], func(e IssueEvent) bool { return e.Issue == "10001" && e.Changed })
	})
	f.titleGoesBackIsTold(t, original)
}

// The same, when a read of the ticket settles while the build the
// reparent made is held: the read builds nothing (a build is in flight),
// the parent's title changes, and the build in flight, whose inputs moved,
// builds again and shows it.
func TestAParentChangedWhileAReadSettlesDuringABuildShows(t *testing.T) {
	t.Parallel()
	f := newParentFixture(t)
	original := f.st.cards["10002"].Title
	f.st.edit("10001", func(c *tracker.Card) { c.Parent = "10002" })
	f.tick(t)
	f.lookups.next(t, "the ticket's rebuild")
	f.letReads()
	tp := f.svc.ticketPumpOf(f.conn.ID, "10001")
	if res := f.svc.readTickets(t.Context(), f.conn.ID, []*ticketPump{tp}, Background, true, false); res.failed {
		t.Fatalf("the read %+v", res)
	}
	f.st.edit("10002", func(c *tracker.Card) { c.Title = "Parent renamed" })
	f.tick(t)
	_, before, _ := f.events.snapshot()
	f.lookups.releaseAll()
	f.events.waitFor(t, "the build installing its view", func(_ []BoardEvent, is []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(is[len(before):], func(e IssueEvent) bool { return e.Issue == "10001" && e.Changed })
	})
	quiet(t, f.svc)
	if v := f.ticketView(t, f.sub.ID); v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Title != "Parent renamed" {
		t.Fatalf("the ticket shows its parent as %+v", v.Detail.Card.Parent)
	}
	f.titleGoesBackIsTold(t, original)
}
