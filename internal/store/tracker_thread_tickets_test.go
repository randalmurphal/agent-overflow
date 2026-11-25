package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

var webTicket = TicketRef{Kind: TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "10001"}

func webLink(threadID string) ThreadTicket {
	return ThreadTicket{ThreadID: threadID, Ticket: webTicket, IssueKey: "WEB-1", IssueTitle: "Fix the login page", Origin: TicketOriginStartWork, LinkedAt: 10}
}

func mustSetThreadTicket(t *testing.T, s *Store, link ThreadTicket, want bool) {
	t.Helper()
	if changed, err := s.SetThreadTicket(link); err != nil || changed != want {
		t.Fatalf("set %+v: changed %v, %v; want %v", link, changed, err, want)
	}
}

func mustGetThreadTicket(t *testing.T, s *Store, threadID string) ThreadTicket {
	t.Helper()
	link, ok, err := s.GetThreadTicket(threadID)
	if err != nil || !ok {
		t.Fatalf("get thread %s ticket: %v, %v", threadID, ok, err)
	}
	return link
}

func TestSetThreadTicketReportsChanges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "a")
	if _, ok, err := s.GetThreadTicket("a"); err != nil || ok {
		t.Fatalf("unlinked thread: %v, %v", ok, err)
	}

	link := webLink("a")
	mustSetThreadTicket(t, s, link, true)
	if got := mustGetThreadTicket(t, s, "a"); got != link {
		t.Fatalf("get = %+v; want %+v", got, link)
	}
	mustSetThreadTicket(t, s, link, false)

	// The same ticket seen again moves its key and title and keeps the
	// link's origin and time.
	again := link
	again.IssueKey, again.IssueTitle, again.Origin, again.LinkedAt = "APP-7", "Moved", TicketOriginAgent, 20
	mustSetThreadTicket(t, s, again, true)
	want := link
	want.IssueKey, want.IssueTitle = "APP-7", "Moved"
	if got := mustGetThreadTicket(t, s, "a"); got != want {
		t.Fatalf("after the same ticket again = %+v; want %+v", got, want)
	}
	mustSetThreadTicket(t, s, again, false)

	// Another ticket replaces the link whole.
	other := ThreadTicket{ThreadID: "a", Ticket: TicketRef{Kind: TrackerKindLinear, Site: "workspace-a", IssueID: "issue-1"},
		IssueKey: "ENG-1", Origin: TicketOriginManual, LinkedAt: 30}
	mustSetThreadTicket(t, s, other, true)
	if got := mustGetThreadTicket(t, s, "a"); got != other {
		t.Fatalf("after another ticket = %+v; want %+v", got, other)
	}

	if cleared, err := s.ClearThreadTicket("a"); err != nil || !cleared {
		t.Fatalf("clear = %v, %v", cleared, err)
	}
	if cleared, err := s.ClearThreadTicket("a"); err != nil || cleared {
		t.Fatalf("second clear = %v, %v", cleared, err)
	}
	if _, ok, err := s.GetThreadTicket("a"); err != nil || ok {
		t.Fatalf("after clear: %v, %v", ok, err)
	}
}

func TestSetThreadTicketRefusals(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "a")
	for name, edit := range map[string]func(*ThreadTicket){
		"tempo ticket":   func(l *ThreadTicket) { l.Ticket.Kind = TrackerKindTempo },
		"no site":        func(l *ThreadTicket) { l.Ticket.Site = "" },
		"no issue":       func(l *ThreadTicket) { l.Ticket.IssueID = "" },
		"no key":         func(l *ThreadTicket) { l.IssueKey = "" },
		"unknown origin": func(l *ThreadTicket) { l.Origin = "pasted" },
		"no time":        func(l *ThreadTicket) { l.LinkedAt = 0 },
		"no thread":      func(l *ThreadTicket) { l.ThreadID = "" },
	} {
		link := webLink("a")
		edit(&link)
		if _, err := s.SetThreadTicket(link); !errors.Is(err, ErrInvalidThreadTicket) {
			t.Errorf("%s: %v; want ErrInvalidThreadTicket", name, err)
		}
	}
	if _, err := s.SetThreadTicket(webLink("missing")); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing thread: %v; want sql.ErrNoRows", err)
	}
	if _, ok, err := s.GetThreadTicket("a"); err != nil || ok {
		t.Fatalf("a refusal linked the thread: %v, %v", ok, err)
	}

	mustCreateThread(t, s, "deleting")
	if err := s.BeginThreadDelete("deleting"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadTicket(webLink("deleting")); !errors.Is(err, ErrThreadDeleting) {
		t.Errorf("thread being deleted: %v; want ErrThreadDeleting", err)
	}
}

func TestThreadTicketReadsByTicketAndThreads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		mustCreateThread(t, s, id)
	}
	mustSetThreadTicket(t, s, webLink("b"), true)
	mustSetThreadTicket(t, s, webLink("a"), true)
	sameIDOtherSite := webLink("c")
	sameIDOtherSite.Ticket.Site = "https://other.example.com"
	mustSetThreadTicket(t, s, sameIDOtherSite, true)

	links, err := s.ListThreadsForTicket(webTicket)
	if err != nil || !slices.Equal(links, []ThreadTicket{webLink("a"), webLink("b")}) {
		t.Fatalf("threads of the ticket = %+v, %v", links, err)
	}

	byThread, err := s.ListThreadTickets([]string{"a", "c", "d", "a", "", "missing"})
	if err != nil || len(byThread) != 2 || byThread["a"] != webLink("a") || byThread["c"] != sameIDOtherSite {
		t.Fatalf("batch read = %+v, %v", byThread, err)
	}
	if byThread, err := s.ListThreadTickets(nil); err != nil || len(byThread) != 0 {
		t.Fatalf("batch read of none = %+v, %v", byThread, err)
	}

	// Seen again with a new key and title: every thread linked to that
	// ticket, and only those, change once.
	changed, err := s.RecordTicketSeen(webTicket, "APP-3", "Moved to APP")
	if err != nil || !slices.Equal(changed, []string{"a", "b"}) {
		t.Fatalf("seen = %v, %v; want a and b", changed, err)
	}
	if changed, err := s.RecordTicketSeen(webTicket, "APP-3", "Moved to APP"); err != nil || len(changed) != 0 {
		t.Fatalf("seen again = %v, %v; want no change", changed, err)
	}
	if got := mustGetThreadTicket(t, s, "b"); got.IssueKey != "APP-3" || got.IssueTitle != "Moved to APP" || got.Origin != TicketOriginStartWork || got.LinkedAt != 10 {
		t.Fatalf("b after seen = %+v", got)
	}
	if got := mustGetThreadTicket(t, s, "c"); got != sameIDOtherSite {
		t.Fatalf("the other site's link moved: %+v", got)
	}
	if _, err := s.RecordTicketSeen(webTicket, "", "x"); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Errorf("seen without a key: %v", err)
	}
	if _, err := s.RecordTicketSeen(TicketRef{Kind: TrackerKindTempo, Site: "s", IssueID: "1"}, "K-1", ""); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Errorf("seen of a Tempo ticket: %v", err)
	}
}

// A thread's delete takes its link, and so does its retirement to a holder,
// which then refuses a new one; the fork that keeps the holder has none.
func TestThreadTicketGoesWithItsThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, webLink("a"), true)
	if err := s.DeleteThread("a"); err != nil {
		t.Fatal(err)
	}
	if links, err := s.ListThreadsForTicket(webTicket); err != nil || len(links) != 0 {
		t.Fatalf("after the thread's delete = %+v, %v", links, err)
	}

	seedLinearSource(t, s, "S", 1)
	mustSetThreadTicket(t, s, webLink("S"), true)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, ok, err := s.GetThreadTicket("F"); err != nil || ok {
		t.Fatalf("the fork has a link: %v, %v", ok, err)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRow(`SELECT mode FROM threads WHERE id = 'S'`).Scan(&mode); err != nil || mode != "holder" {
		t.Fatalf("S after its delete: mode %q, %v; want a holder", mode, err)
	}
	if _, ok, err := s.GetThreadTicket("S"); err != nil || ok {
		t.Fatalf("the holder kept its link: %v, %v", ok, err)
	}
	if _, err := s.SetThreadTicket(webLink("S")); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("link of a holder: %v; want ErrInvalidThreadTicket", err)
	}
}

// The tracker tables are application records: a restore brings back the
// snapshot's rows.
func TestRestoreKeepsTheTrackerTables(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "linked")
	jira := mustCreateTrackerConnection(t, st, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	tempo := mustCreateTrackerConnection(t, st, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 2})
	boards := []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}, {ConnectionID: jira.ID, BoardID: "11"}}
	if _, _, err := replaceFromStored(st, "p1", boards); err != nil {
		t.Fatal(err)
	}
	mustSetThreadTicket(t, st, webLink("t1"), true)
	connections, err := st.ListTrackerConnections()
	if err != nil {
		t.Fatal(err)
	}

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearThreadTicket("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTrackerConnection(jira.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}

	if got, err := st.ListTrackerConnections(); err != nil || !slices.Equal(got, connections) || got[1].ID != tempo.ID {
		t.Fatalf("connections after restore = %+v, %v; want %+v", got, err, connections)
	}
	if got, err := st.ListProjectTrackerBoards("p1"); err != nil || !slices.Equal(got, boards) {
		t.Fatalf("boards after restore = %+v, %v", got, err)
	}
	if got := mustGetThreadTicket(t, st, "t1"); got != webLink("t1") {
		t.Fatalf("link after restore = %+v", got)
	}
}
