package app

import (
	"context"
	"slices"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A pull request row carries the tickets its title and head branch name,
// as the landing read finds them in this computer's mirrors: an issue the
// mirror holds with its ID and title, a key of a mirrored project it holds
// no issue of with its connection alone, and a key of no connected project
// not at all. The stored copy keeps them.
func TestAPullRequestRowCarriesTheTicketsItNames(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") })

	f.a.prUpdates.interval = time.Hour
	fake := newPRListFake(t, f.a)
	fake.set(func(pf *prListFake) {
		pf.projects = pf.projects[:1]
		pf.rows[prListRepoA.Key()] = []gitops.PRListRow{
			prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "WEB-1 Retry cursor", "web-2-and-web-1" }),
			prListRow(prListRepoA, 2, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "Fix WEB-77 and OPS-4", "main" }),
			prListRow(prListRepoA, 3, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "No ticket", "UTF-8" }),
		}
	})
	want := map[int][]store.KeyedTicket{
		1: {
			{Key: "WEB-1", ConnectionID: f.conn.ID, IssueID: "10101", Title: "First story"},
			{Key: "WEB-2", ConnectionID: f.conn.ID, IssueID: "10102", Title: "Second task"},
		},
		2: {{Key: "WEB-77", ConnectionID: f.conn.ID}},
		3: nil,
	}
	check := func(what string, rows []PRListItem) {
		t.Helper()
		if len(rows) != 3 {
			t.Fatalf("%s rows = %+v; want 3", what, rows)
		}
		for _, row := range rows {
			if !slices.Equal(row.Tickets, want[row.Ref.Number]) {
				t.Errorf("%s PR %d's tickets = %+v; want %+v", what, row.Ref.Number, row.Tickets, want[row.Ref.Number])
			}
		}
	}

	// The prefetch lands a read as the pump's tick does.
	if n, err := f.a.prefetchPRList(context.Background()); err != nil || n != 1 {
		t.Fatalf("prefetch = %d, %v", n, err)
	}
	stored, _, err := f.a.storedPRLists()
	if err != nil {
		t.Fatal(err)
	}
	check("the prefetched", stored[prListRepoA.Key()].Rows)

	events := capturePRList(t, f.a)
	subscribePRList(t, f.a)
	check("the tick's", prListProject(t, awaitPRListRead(t, events), "p-a").Rows)
}
