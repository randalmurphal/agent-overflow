package trackerapp

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A linked issue is left out of the cards read only while a board pump
// that runs keeps its card fresh: a mirrored board with no pump, a pump
// whose every handle is hidden, a pump whose reads fail and a pump that
// ended leave it to the cards read, which stores the newer card; a
// shown, healthy pump of its board takes it out.
func TestLinkedCardsSkipOnlyIssuesARunningPumpKeepsFresh(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	tickOK(t, m.svc, m.scope(), BoardDue{Structure: true, Delta: true})
	m.linkThread("thread-1", "10001")
	var mu sync.Mutex
	var asked [][]tracker.IssueID
	updated := int64(1_799_000_000_000)
	m.p.set(func(p *scripted) {
		p.cards = func(q []tracker.IssueID) (tracker.Cards, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, slices.Clone(q))
			updated += 60_000
			c := card("10001", updated)
			c.Title = "Renamed"
			return tracker.Cards{Cards: []tracker.Card{c}}, nil
		}
	})
	// cardsRead runs a cards tick and reports whether it read 10001, and
	// whether the mirror then holds the card it read.
	cardsRead := func(t *testing.T, when string) bool {
		t.Helper()
		mu.Lock()
		before := len(asked)
		mu.Unlock()
		out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
		mu.Lock()
		defer mu.Unlock()
		if out.Failure != nil || out.Ended {
			t.Fatalf("%s: cards tick %+v", when, out)
		}
		if len(asked) == before {
			return false
		}
		if !slices.Equal(asked[before], ids("10001")) || !slices.Equal(out.Changes().Cards, ids("10001")) {
			t.Fatalf("%s: asked %v, stored %+v", when, asked[before:], out.Changes())
		}
		return true
	}
	if !cardsRead(t, "no pump") {
		t.Fatal("with no pump on its board, the linked issue's card was not read")
	}
	sub := m.subscribe(t, testBoard)
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if cardsRead(t, "a shown pump") {
		t.Fatal("a shown pump's issue was read by the cards unit")
	}
	if err := m.svc.SetSubscriptionVisible(sub.ID, false); err != nil {
		t.Fatal(err)
	}
	if !cardsRead(t, "a hidden pump") {
		t.Fatal("with its board's pump hidden, the linked issue's card was not read")
	}
	if err := m.svc.SetSubscriptionVisible(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	var delta func(tracker.DeltaQuery) (tracker.RowPage, error)
	m.p.set(func(p *scripted) {
		delta = p.changedRows
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if !cardsRead(t, "a failing pump") {
		t.Fatal("with its board's pump failing, the linked issue's card was not read")
	}
	m.p.set(func(p *scripted) { p.changedRows = delta })
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if cardsRead(t, "a shown pump again") {
		t.Fatal("a shown pump's issue was read by the cards unit")
	}
	m.svc.ReleaseSubscription(sub.ID)
	if !cardsRead(t, "a released pump") {
		t.Fatal("after its board's pump ended, the linked issue's card was not read")
	}
}
