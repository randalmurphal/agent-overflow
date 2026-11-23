package trackerapp

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// foldedIDs are the 10,000 changed issues the fold benchmarks gather.
const foldedIDs = 10_000

// BenchmarkLinkedCardsAllChanged10000 is a cards tick of a connection
// whose 10,000 linked issues all changed: 50 pages of the store's listing,
// 100 cards reads and writes, and the fold of what they stored.
func BenchmarkLinkedCardsAllChanged10000(b *testing.B) {
	svc, p, scope := benchService(b)
	st := svc.store.(*store.Store)
	conn, err := st.GetTrackerConnection(scope.Connection)
	if err != nil {
		b.Fatal(err)
	}
	project := store.Project{ID: "project-1", Path: "/tmp/web", Name: "web", CreatedAt: 1, UpdatedAt: 1}
	if _, err := st.CreateProject(project); err != nil {
		b.Fatal(err)
	}
	for i := range foldedIDs {
		id := fmt.Sprint(40000 + i)
		if err := st.CreateThread(store.Thread{ID: "thread-" + id, ProjectID: project.ID, Title: "Thread", Provider: "claude",
			Model: "test-model", WorkspacePath: project.Path, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			b.Fatal(err)
		}
		if _, err := st.SetThreadTicket(store.ThreadTicket{ThreadID: "thread-" + id,
			Ticket: store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: id}, IssueKey: "WEB-1",
			Origin: store.TicketOriginManual, LinkedAt: 1}); err != nil {
			b.Fatal(err)
		}
	}
	updated := time.UnixMilli(1_799_000_000_000).UTC()
	p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
		out := tracker.Cards{Cards: make([]tracker.Card, len(asked))}
		for i, id := range asked {
			out.Cards[i] = realisticRow(0, updated).Card
			out.Cards[i].ID = id
		}
		return out, nil
	}
	b.ReportAllocs()
	for b.Loop() {
		updated = updated.Add(time.Second)
		out, err := svc.SyncLinked(b.Context(), scope.Connection, LinkedDue{Cards: true}, Background)
		if err != nil || out.Failure != nil || len(out.Changes().Cards) != foldedIDs {
			b.Fatalf("cards tick %d changed, %+v, %v", len(out.Changes().Cards), out.Failure, err)
		}
	}
}

// foldPages are the changes of 50 pages of 200 issues each, as the cards
// unit gathers them.
func foldPages() []Changes {
	pages := make([]Changes, foldedIDs/200)
	for i := range pages {
		ids := make([]tracker.IssueID, 200)
		for j := range ids {
			ids[j] = tracker.IssueID(fmt.Sprint(40000 + i*200 + j))
		}
		pages[i] = Changes{Cards: ids}
	}
	return pages
}

// BenchmarkFoldByMerge10000 folds 10,000 changed IDs a page at a time
// with Changes.add, merging each page into everything gathered.
func BenchmarkFoldByMerge10000(b *testing.B) {
	pages := foldPages()
	b.ReportAllocs()
	for b.Loop() {
		var out Changes
		for _, p := range pages {
			out.add(p)
		}
		if len(out.Cards) != foldedIDs {
			b.Fatal(len(out.Cards))
		}
	}
}

// BenchmarkFoldByLog10000 folds the same pages with a changeLog: appended
// and ordered once.
func BenchmarkFoldByLog10000(b *testing.B) {
	pages := foldPages()
	b.ReportAllocs()
	for b.Loop() {
		var log changeLog
		for _, p := range pages {
			log.add(p)
		}
		if len(log.changes().Cards) != foldedIDs {
			b.Fatal("short")
		}
	}
}

// A changeLog folds reads that overlap and come in any order to what
// merging them one by one gives: each list sorted without repeats.
func TestAChangeLogFoldsAsMergingDoes(t *testing.T) {
	t.Parallel()
	reads := []Changes{
		{Cards: ids("10003", "10001"), Rows: ids("10001"), History: ids("10002")},
		{Cards: ids("10002", "10001"), Added: ids("10004"), Removed: ids("10005"), Catalog: true},
		{},
		{Rows: ids("10001", "10000"), Details: ids("10007"), Dropped: ids("10006", "10006"), Structure: true},
	}
	var log changeLog
	var merged Changes
	for _, r := range reads {
		log.add(r)
		merged.add(Changes{Cards: ordered(slices.Clone(r.Cards)), Rows: ordered(slices.Clone(r.Rows)), Added: r.Added, Removed: r.Removed,
			History: r.History, Details: r.Details, Dropped: ordered(slices.Clone(r.Dropped)), Catalog: r.Catalog, Structure: r.Structure})
	}
	if got := log.changes(); !reflect.DeepEqual(got, merged) {
		t.Fatalf("the log folded %+v; merging gives %+v", got, merged)
	}
}
