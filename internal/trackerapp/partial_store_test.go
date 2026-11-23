package trackerapp

import (
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// partialStore fails a history write and a reconcile after part of each
// was stored, as the store does when a later transaction of one call
// fails: what committed comes back with the error.
type partialStore struct{ *store.Store }

func (s *partialStore) ReplaceMirrorHistories(root, sweep int64, hs []store.MirrorHistory, catalog store.MirrorCatalog,
	read store.MirrorRead) ([]store.MirrorHistoryResult, bool, error) {
	res, changed, err := s.Store.ReplaceMirrorHistories(root, sweep, hs[:1], catalog, read)
	if err != nil {
		return res, changed, err
	}
	return res, changed, errStoreDown
}

func (s *partialStore) ReconcileMirrorBoard(root int64, board string, lifetime int64, listed []string, complete bool, read store.MirrorRead, lag time.Duration) (store.MirrorReconcile, error) {
	res, err := s.Store.ReconcileMirrorBoard(root, board, lifetime, listed, complete, read, lag)
	if err != nil {
		return res, err
	}
	return store.MirrorReconcile{Removed: res.Removed}, errStoreDown
}

// A store call that failed after it stored part changed the mirror: the
// tick fails and still names what was stored, so its subscribers are told.
func TestATickTellsWhatAFailedStoreCallStored(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	began := time.UnixMilli(1_798_500_000_000).UTC()
	m.p.set(func(p *scripted) {
		p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
			var h tracker.History
			for _, id := range q.Issues {
				h.Issues = append(h.Issues, tracker.IssueHistory{
					Issue: id, CompleteTo: m.now(),
					Status: []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}},
				})
			}
			return h, nil
		}
	})
	m.svc.store = &partialStore{Store: m.st}

	out, err := m.svc.tickHistory(t.Context(), m.scope(), Background)
	if !errors.Is(err, errStoreDown) || !slices.Equal(out.Changes().History, ids("10001")) {
		t.Fatalf("history %+v, %v; want the failure and the one history stored", out, err)
	}

	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	// Past the overlap, the listing judges the rows itself.
	m.pass(tracker.DeltaOverlap + time.Millisecond)
	out, err = m.svc.tickReconcile(t.Context(), m.scope(), Background)
	if !errors.Is(err, errStoreDown) || !slices.Equal(out.Changes().Removed, ids("10002")) {
		t.Fatalf("reconcile %+v, %v; want the failure and the one row removed", out, err)
	}
}
