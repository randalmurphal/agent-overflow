package trackerapp

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// due are the issues whose history is due, the first page.
func (m *mirrorFixture) due() []string {
	m.t.Helper()
	due, _, err := m.st.ListMirrorHistoryDue(m.root(), "")
	if err != nil {
		m.t.Fatal(err)
	}
	out := make([]string, len(due))
	for i, d := range due {
		out[i] = d.IssueID
	}
	return out
}

func TestHistoryReadsDueIssuesWithTheirBoards(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	from := time.UnixMilli(1_798_000_000_000).UTC()
	final := time.UnixMilli(1_799_900_000_000).UTC()
	var asked []tracker.HistoryQuery
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		asked = append(asked, q)
		// 10001's history is complete only from from (a tracker that folds
		// an issue's first minutes); 10002 is not returned.
		return tracker.History{Issues: []tracker.IssueHistory{{
			Issue: "10001", CompleteFrom: from, CompleteTo: final,
			Status: []tracker.StatusChange{{Change: tracker.Change{At: from.Add(time.Hour), Author: "acc-user2"}, From: "s1", To: "s2"}},
		}}, Catalog: tracker.Catalog{Users: []tracker.User{user2}}}, nil
	}
	out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if out.Failure != nil || out.More() || !slices.Equal(out.Changes().History, ids("10001", "10002")) || !out.Changes().Catalog {
		t.Fatalf("history %+v", out)
	}
	if len(asked) != 1 || !slices.Equal(asked[0].Issues, ids("10001", "10002")) || !slices.Equal(asked[0].Boards, []tracker.BoardID{testBoard}) {
		t.Fatalf("asked %+v", asked)
	}
	if due := m.due(); len(due) != 0 {
		t.Fatalf("still due %v", due)
	}
	// Called again it reads nothing.
	if out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background)); !out.Changes().Empty() || len(asked) != 1 {
		t.Fatalf("second history %+v", out)
	}
	// A card updated after its history's final time is due again.
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{row("10001", 1_799_950_000_000, "a")}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if due := m.due(); !slices.Equal(due, []string{"10001"}) {
		t.Fatalf("due after an update %v", due)
	}
}

func TestHistoryStopsAtItsBound(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	n := historyBatch*historyBatches + 3
	var rows []tracker.Row
	for i := range n {
		id := fmt.Sprintf("%05d", 30000+i)
		rows = append(rows, row(id, 1_799_000_000_000, id))
	}
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{Rows: rows, Next: cursorAfter("x")}, nil
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	reads := 0
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		reads++
		if len(q.Issues) > historyBatch {
			t.Errorf("a read of %d issues", len(q.Issues))
		}
		var h tracker.History
		for _, id := range q.Issues {
			h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now()})
		}
		return h, nil
	}
	out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if !out.More() || reads != historyBatches || len(out.Changes().History) != n-3 {
		t.Fatalf("bounded history: more %v, %d reads, %d stored", out.More(), reads, len(out.Changes().History))
	}
	out = must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if out.More() || len(out.Changes().History) != 3 {
		t.Fatalf("finishing history %+v", out)
	}
}

// statusChanges is n status changes a minute apart from at, alternating
// between s1 and s2.
func statusChanges(at time.Time, n int) []tracker.StatusChange {
	out := make([]tracker.StatusChange, n)
	for i := range out {
		from, to := tracker.StatusID("s1"), tracker.StatusID("s2")
		if i%2 == 1 {
			from, to = to, from
		}
		out[i] = tracker.StatusChange{Change: tracker.Change{At: at.Add(time.Duration(i) * time.Minute)}, From: from, To: to}
	}
	return out
}

// One read's histories are stored in one store call, never one per
// issue.
func TestHistoryIsStoredInBatches(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var rows []tracker.Row
	for i := range historyBatch {
		id := fmt.Sprintf("%05d", 30000+i)
		rows = append(rows, row(id, 1_799_000_000_000, id))
	}
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{Rows: rows, Next: cursorAfter("x")}, nil
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	base := time.UnixMilli(1_790_000_000_000).UTC()
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		var h tracker.History
		for _, id := range q.Issues {
			h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(), Status: statusChanges(base, 100)})
		}
		return h, nil
	}
	cs := newCountingStore(m.st)
	m.svc.store = cs
	out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	// 50 issues of 100 changes: one write, whatever the store's own
	// transactions.
	if writes := cs.take()["ReplaceMirrorHistories"]; writes != 1 || len(out.Changes().History) != historyBatch {
		t.Fatalf("%d writes, %d stored: %+v", writes, len(out.Changes().History), out)
	}
	if due := m.due(); len(due) != 0 {
		t.Fatalf("still due %v", due)
	}
}

// An issue's whole history is stored, however long, in the read's one
// store call.
func TestHistoryStoresALongHistoryWhole(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_790_000_000_000).UTC()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	const n = 2600
	m.p.history = func(tracker.HistoryQuery) (tracker.History, error) {
		return tracker.History{Issues: []tracker.IssueHistory{{Issue: "10001", CompleteTo: m.now(), Status: statusChanges(base, n)}}}, nil
	}
	cs := newCountingStore(m.st)
	m.svc.store = cs
	out, err := m.svc.tickHistory(t.Context(), m.scope(), Background)
	if err != nil || out.Failure != nil || !slices.Equal(out.Changes().History, ids("10001")) {
		t.Fatalf("a long history: %+v, %v", out, err)
	}
	if writes := cs.take()["ReplaceMirrorHistories"]; writes != 1 {
		t.Fatalf("%d history writes", writes)
	}
	if due := m.due(); len(due) != 0 {
		t.Fatalf("still due %v", due)
	}
}

func TestHistoryFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.history = func(tracker.HistoryQuery) (tracker.History, error) {
		return tracker.History{}, &trackerapi.RateLimitedError{Tracker: "jira", Until: m.now().Add(time.Minute)}
	}
	out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if out.Failure == nil || out.Failure.Kind != FailureRateLimited || !out.RetryAt.Equal(m.now().Add(time.Minute)) {
		t.Fatalf("failed history %+v", out)
	}
	if due := m.due(); !slices.Equal(due, []string{"10001"}) {
		t.Fatalf("a failed read changed what is due: %v", due)
	}
	if st := m.sync(testBoard); st.Failure.Kind != store.MirrorFailureRateLimited {
		t.Fatalf("sync state %+v", st)
	}
}
