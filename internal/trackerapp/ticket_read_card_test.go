package trackerapp

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// sweptStore counts calls and, while the test sets it, answers one
// details' write as swept, as the store does when a cleanup ran since the
// run captured the root's sweep.
type sweptStore struct {
	*countingStore
	swept atomic.Bool
}

func (s *sweptStore) StoreMirrorDetails(root, sweep int64, docs []store.MirrorDetailDoc, openedAt, readAt int64) (store.MirrorDetailWrites, error) {
	if s.swept.CompareAndSwap(true, false) {
		s.count("StoreMirrorDetails")
		return store.MirrorDetailWrites{}, store.ErrTrackerCacheSwept
	}
	return s.countingStore.StoreMirrorDetails(root, sweep, docs, openedAt, readAt)
}

// A full read stores its card before its document: when the document's
// write fails, the mirror's card is the one the read found and the read
// reports it stored, and the next read stores the document.
func TestAFullReadWhoseDocumentIsNotStoredLeavesItsCardStored(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 1, false)
	m.round(t)
	ts := &ticketStore{Store: m.st}
	m.svc.store = ts
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	ts.failDetails.Store(true)
	res, err := m.svc.readDetails(t.Context(), m.conn.ID, []*ticketRead{{issue: issues[0], full: true}}, Background, 0)
	if !errors.Is(err, errStoreDown) || !slices.Contains(res.ch.Cards, issues[0]) {
		t.Fatalf("the read whose document was refused stored %+v, %v; want its card, and the store's failure", res.ch, err)
	}
	if c := must[IssueView](t)(m.svc.Issue(m.conn.ID, issues[0])).Card; c.Title != "Renamed" {
		t.Fatalf("the mirror's card is titled %q after the read that found it renamed", c.Title)
	}
	ts.failDetails.Store(false)
	if res := m.round(t); res.failed {
		t.Fatalf("round %+v", res)
	}
	if v := m.ticketView(t, subs[0].ID); v.Detail.Card.Title != "Renamed" {
		t.Fatalf("the ticket shows %q", v.Detail.Card.Title)
	}
}

// A round's check stores a changed ticket's card, so the full read that
// follows stores the document alone; when a cleanup ran in between and the
// read is made again, it stores its card too.
func TestAFullReadMadeAgainAfterASweepStoresItsCard(t *testing.T) {
	t.Parallel()
	for _, swept := range []bool{false, true} {
		t.Run(map[bool]string{false: "no sweep", true: "swept"}[swept], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 1, true)
			m.round(t)
			ss := &sweptStore{countingStore: newCountingStore(m.st)}
			m.svc.store = ss
			st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
			ss.swept.Store(swept)
			if res := m.round(t); res.failed {
				t.Fatalf("round %+v", res)
			}
			calls := ss.take()
			want := map[bool][2]int{false: {1, 1}, true: {2, 2}}[swept]
			if got := [2]int{calls["UpsertMirrorCards"], calls["StoreMirrorDetails"]}; got != want {
				t.Fatalf("%d card writes and %d document writes; want %d and %d", got[0], got[1], want[0], want[1])
			}
			if v := m.ticketView(t, subs[0].ID); v.Detail.Card.Title != "Renamed" {
				t.Fatalf("the ticket shows %q", v.Detail.Card.Title)
			}
		})
	}
}
