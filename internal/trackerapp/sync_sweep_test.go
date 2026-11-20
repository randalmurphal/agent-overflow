package trackerapp

import (
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// sweepFixture is a mirror fixture whose connection links 10005, 10006
// and 10007, their cards stored, and whose store counts its calls.
func sweepFixture(t *testing.T) (*mirrorFixture, *countingStore) {
	t.Helper()
	m := newMirrorFixture(t)
	for i, issue := range ids("10005", "10006", "10007") {
		m.linkThread("thread-"+string(rune('a'+i)), issue)
	}
	m.p.cards = cardsOf
	if out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background)); len(out.Changes().Cards) != 3 {
		t.Fatalf("seeding tick %+v", out)
	}
	cs := newCountingStore(m.st)
	m.svc.store = cs
	return m, cs
}

// cardsOf answers a card read with a card of each issue asked.
func cardsOf(asked []tracker.IssueID) (tracker.Cards, error) {
	var out tracker.Cards
	for _, id := range asked {
		out.Cards = append(out.Cards, card(string(id), 1_799_000_000_000))
	}
	return out, nil
}

// sweep unlinks threadID's issue and runs the mirror's cleanup, which
// removes the issue and advances the root's sweep.
func (m *mirrorFixture) sweep(t *testing.T, threadID string) {
	t.Helper()
	before := m.sync("").Sweep
	if _, err := m.st.ClearThreadTicket(threadID); err != nil {
		t.Fatal(err)
	}
	if err := m.st.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	if after := m.sync("").Sweep; after == before {
		t.Fatalf("the cleanup swept nothing: sweep %d", after)
	}
}

// A card write the store refuses because a sweep ran during the read is
// read again once, after the sweep is read again: the tick succeeds.
func TestCardsReadAgainAfterASweep(t *testing.T) {
	t.Parallel()
	m, cs := sweepFixture(t)
	reads := 0
	m.p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			if reads++; reads == 1 {
				m.sweep(t, "thread-b")
			}
			return cardsOf(asked)
		}
	})
	cs.take()
	out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	calls := cs.take()
	if out.Failure != nil || reads != 2 || calls["UpsertMirrorCards"] != 2 || calls["GetMirrorSync"] != 2 {
		t.Fatalf("tick %+v, %d reads, store calls %v", out, reads, calls)
	}
	cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10005", "10006", "10007"}))
	var held []string
	for _, c := range cards {
		held = append(held, c.IssueID)
	}
	if !slices.Equal(held, []string{"10005", "10007"}) {
		t.Fatalf("the mirror holds %v", held)
	}
	// A second sweep during the read again is an error, not a third read.
	reads = 0
	m.p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			reads++
			m.sweep(t, []string{"thread-a", "thread-c"}[reads-1])
			return cardsOf(asked)
		}
	})
	if _, err := m.svc.tickCards(t.Context(), m.conn.ID, Background); !errors.Is(err, store.ErrTrackerCacheSwept) || reads != 2 {
		t.Fatalf("a tick swept twice: %v after %d reads", err, reads)
	}
}

// A detail read through that a sweep overtook is read again once.
func TestDetailReadsAgainAfterASweep(t *testing.T) {
	t.Parallel()
	m, _ := sweepFixture(t)
	reads := 0
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		if reads++; reads == 1 {
			m.sweep(t, "thread-b")
		}
		return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
	}
	v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10005", Interactive)
	if err != nil || reads != 2 || v.Card.ID != "10005" {
		t.Fatalf("detail %+v after %d reads, %v", v.Card, reads, err)
	}
	if d, ok, err := m.st.GetMirrorDetail(m.root(), "10005"); err != nil || !ok || len(d.Data) == 0 {
		t.Fatalf("stored detail %+v, %v, %v", d, ok, err)
	}
}

// A detail check whose card write a sweep overtook reads the card again
// once, and serves the stored detail of the issue it still keeps.
func TestADetailCheckReadsItsCardAgainAfterASweep(t *testing.T) {
	t.Parallel()
	m, _ := sweepFixture(t)
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
	}
	m.openDetail(t, "10005")
	reads := 0
	m.p.updatedMovesWithDetail(func(asked []tracker.IssueID) (tracker.Cards, error) {
		if reads++; reads == 1 {
			m.sweep(t, "thread-b")
		}
		return cardsOf(asked)
	})
	v, _ := m.openDetail(t, "10005")
	if reads != 2 || v.Card.ID != "10005" || m.p.count("Detail") != 1 {
		t.Fatalf("detail %+v after %d card reads, %d detail reads", v.Card, reads, m.p.count("Detail"))
	}
}
