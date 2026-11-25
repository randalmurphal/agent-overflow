package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var webTicket = TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10001"}

// webLink is a link of threadID to webTicket that names no mirror root.
func webLink(threadID string) ThreadTicket {
	return ThreadTicket{ThreadID: threadID, Ticket: webTicket, IssueKey: "WEB-1", IssueTitle: "Fix the login page", Origin: TicketOriginStartWork, LinkedAt: 10}
}

// seedThreadTicket writes link's row as it stands, for a test of what
// reads, keeps or sweeps by links rather than of the link's writer.
func seedThreadTicket(t *testing.T, s *Store, link ThreadTicket) {
	t.Helper()
	mustExec(t, s.db, `INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		link.ThreadID, link.Ticket.Kind, link.Ticket.Site, link.Ticket.IssueID, link.IssueKey, link.IssueTitle,
		link.StatusID, link.StatusName, link.StatusGroup, link.Origin, link.LinkedAt, link.SeenRoot, link.Missing)
}

func mustGetThreadTicket(t *testing.T, s *Store, threadID string) ThreadTicket {
	t.Helper()
	link, ok, err := s.GetThreadTicket(threadID)
	if err != nil || !ok {
		t.Fatalf("get thread %s ticket: %v, %v", threadID, ok, err)
	}
	return link
}

// titledCard is webCard of issue id with title.
func titledCard(id string, updated int64, title string) MirrorCard {
	c := webCard(id, updated)
	c.Title = title
	return c
}

// readLink links threadID to card as a read of root just returned it,
// with board 10's catalog.
func readLink(s *Store, root int64, threadID string, card MirrorCard) ThreadTicketWrite {
	return ThreadTicketWrite{ThreadID: threadID, Origin: TicketOriginManual, LinkedAt: 10, Root: root, Card: card,
		Read: nextMirrorRead(s, root), Catalog: webStructure().Catalog, Sweep: currentSweep(s, root)}
}

// heldLink links threadID to the card root's mirror holds.
func heldLink(root int64, threadID string, card MirrorCard) ThreadTicketWrite {
	return ThreadTicketWrite{ThreadID: threadID, Origin: TicketOriginManual, LinkedAt: 10, Root: root, Card: card}
}

func mustSetThreadTicket(t *testing.T, s *Store, w ThreadTicketWrite) ThreadTicketSet {
	t.Helper()
	set, err := s.SetThreadTicket(w)
	if err != nil {
		t.Fatalf("link %s to %s: %v", w.ThreadID, w.Card.IssueID, err)
	}
	return set
}

// A link takes a read's card into the link and the mirror in one write;
// the same card from the mirror changes nothing; the same ticket again
// keeps the link's origin and time; another ticket replaces it, naming the
// one it replaced; a clear names what it cleared.
func TestSetThreadTicketLinksReplacesAndClears(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustCreateThread(t, s, "a")
	seedThreadTicket(t, s, ThreadTicket{ThreadID: mustThread(t, s, "other"), Ticket: webTicket, IssueKey: "WEB-1", Origin: TicketOriginManual, LinkedAt: 1})

	set := mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	if !set.Changed || set.Replaced != nil || !slices.Equal(set.Cards.Cards, []string{"10001"}) || !slices.Equal(set.Cards.Threads, []string{"other"}) {
		t.Fatalf("first link = %+v; want a change and the other link of the ticket named", set)
	}
	want := ThreadTicket{ThreadID: "a", Ticket: webTicket, IssueKey: "WEB-10001", IssueTitle: "Fix the login page",
		StatusID: "1", StatusName: "To Do", StatusGroup: MirrorGroupToDo, Origin: TicketOriginManual, LinkedAt: 10, SeenRoot: root}
	if got := mustGetThreadTicket(t, s, "a"); got != want {
		t.Fatalf("link = %+v\nwant %+v", got, want)
	}
	if cards := mustMirrorCards(t, s, root, "10001"); len(cards) != 1 || cards[0].UpdatedAt != 5 {
		t.Fatalf("mirror cards = %+v; want the read's", cards)
	}
	row, err := s.GetThread("a")
	if err != nil {
		t.Fatal(err)
	}
	wantRow := ThreadTicketLink{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10001", Key: "WEB-10001", Title: "Fix the login page",
		StatusID: "1", StatusName: "To Do", StatusGroup: MirrorGroupToDo, Origin: TicketOriginManual, LinkedAt: 10,
		State: TicketLinkCurrent, ConnectionID: c.ID}
	if row.TicketLink == nil || *row.TicketLink != wantRow {
		t.Fatalf("thread row link = %+v\nwant %+v", row.TicketLink, wantRow)
	}

	if set := mustSetThreadTicket(t, s, heldLink(root, "a", titledCard("10001", 5, "Fix the login page"))); set.Changed || set.Replaced != nil {
		t.Fatalf("the same card from the mirror = %+v; want no change", set)
	}

	again := readLink(s, root, "a", titledCard("10001", 6, "Fix the login page on mobile"))
	again.Origin, again.LinkedAt = TicketOriginAgent, 20
	if set := mustSetThreadTicket(t, s, again); !set.Changed || set.Replaced != nil {
		t.Fatalf("the same ticket again = %+v", set)
	}
	want.IssueTitle = "Fix the login page on mobile"
	if got := mustGetThreadTicket(t, s, "a"); got != want {
		t.Fatalf("after the same ticket again = %+v\nwant %+v", got, want)
	}

	set = mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10002", 5, "Another")))
	if !set.Changed || set.Replaced == nil || *set.Replaced != webTicket {
		t.Fatalf("another ticket = %+v; want it to name %+v", set, webTicket)
	}
	if got := mustGetThreadTicket(t, s, "a"); got.Ticket.IssueID != "10002" || got.Origin != TicketOriginManual || got.LinkedAt != 10 {
		t.Fatalf("after another ticket = %+v", got)
	}

	cleared, had, err := s.ClearThreadTicket("a")
	if err != nil || !had || cleared != (TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10002"}) {
		t.Fatalf("clear = %+v, %v, %v", cleared, had, err)
	}
	if _, had, err := s.ClearThreadTicket("a"); err != nil || had {
		t.Fatalf("second clear = %v, %v", had, err)
	}
	if row, err := s.GetThread("a"); err != nil || row.TicketLink != nil {
		t.Fatalf("thread row after clear = %+v, %v", row.TicketLink, err)
	}
}

// Each refusal leaves the stored link and the mirror as they were.
func TestSetThreadTicketRefusalsChangeNothing(t *testing.T) {
	t.Parallel()
	isTransfer := func(err error) bool {
		var transfer *ThreadTransferError
		return errors.As(err, &transfer)
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	for _, c := range []struct {
		name    string
		write   func(t *testing.T, s *Store, root int64) ThreadTicketWrite
		refused func(error) bool
	}{
		{"unknown origin", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			w.Origin = "pasted"
			return w
		}, is(ErrInvalidThreadTicket)},
		{"no time", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			w.LinkedAt = 0
			return w
		}, is(ErrInvalidThreadTicket)},
		{"no root", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := heldLink(root, "a", titledCard("10009", 5, "New"))
			w.Root = 0
			return w
		}, is(ErrInvalidThreadTicket)},
		{"card with no key", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			card := titledCard("10009", 5, "New")
			card.Key = ""
			return readLink(s, root, "a", card)
		}, is(ErrInvalidTrackerCache)},
		{"read with no sweep", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			w.Sweep = 0
			return w
		}, is(ErrInvalidTrackerCache)},
		{"missing thread", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			return readLink(s, root, "missing", titledCard("10009", 5, "New"))
		}, is(sql.ErrNoRows)},
		{"thread being deleted", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			mustCreateThread(t, s, "deleting")
			if err := s.BeginThreadDelete("deleting"); err != nil {
				t.Fatal(err)
			}
			return readLink(s, root, "deleting", titledCard("10009", 5, "New"))
		}, is(ErrThreadDeleting)},
		{"thread moving away", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			seedTransfer(t, s, "a", "outgoing", "move", "preparing", 0)
			return readLink(s, root, "a", titledCard("10009", 5, "New"))
		}, isTransfer},
		{"holder", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			seedLinearSource(t, s, "S", 1)
			mustPointerFork(t, s, "S", "F", ForkCut{})
			if err := s.DeleteThread("S"); err != nil {
				t.Fatal(err)
			}
			return readLink(s, root, "S", titledCard("10009", 5, "New"))
		}, is(ErrInvalidThreadTicket)},
		{"detached mirror", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			if _, err := s.DetachMirror(mustConnectionOf(t, s, root)); err != nil {
				t.Fatal(err)
			}
			return w
		}, is(ErrTrackerCacheGone)},
		{"swept since the read began", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			linkIssue(t, s, "swept", "10008")
			if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10008", 5)}, MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ClearThreadTicket("swept"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			return w
		}, is(ErrTrackerCacheSwept)},
		{"mirror card moved", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			return heldLink(root, "a", titledCard("10001", 4, "Fix the login page"))
		}, is(ErrThreadTicketCardMoved)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustCreateThread(t, s, "a")
			mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
			w := c.write(t, s, root)
			before, _ := storedTicket(t, s, "a")
			mirror := mirrorIssueRows(t, s)
			if _, err := s.SetThreadTicket(w); !c.refused(err) {
				t.Fatalf("set = %v; want refused", err)
			}
			if got, ok := storedTicket(t, s, "a"); !ok || got != before {
				t.Fatalf("stored link = %+v, %v; want %+v", got, ok, before)
			}
			if _, ok := storedTicket(t, s, w.ThreadID); w.ThreadID != "a" && ok {
				t.Fatalf("thread %s was linked", w.ThreadID)
			}
			if got := mirrorIssueRows(t, s); !slices.Equal(got, mirror) {
				t.Fatalf("mirror cards = %v\nwant %v", got, mirror)
			}
		})
	}
}

// mustThread makes thread id and returns it.
func mustThread(t *testing.T, s *Store, id string) string {
	t.Helper()
	mustCreateThread(t, s, id)
	return id
}

// mirrorIssueRows is every mirror card row, each as one string.
func mirrorIssueRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT root_id || ':' || issue_id || ':' || issue_key || ':' || status_id || ':' || updated_at || ':' || read_at
		FROM tracker_cache_issues ORDER BY root_id, issue_id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

// mustConnectionOf is the connection root belongs to.
func mustConnectionOf(t *testing.T, s *Store, root int64) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT connection_id FROM tracker_cache_roots WHERE id = ?`, root).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The thread rows of a ticket's threads come newest first, an ask's copy
// left out; the batch read is keyed by thread; the threads of a site are
// those of its tracker kind and site.
func TestThreadTicketReadsByTicketThreadsAndSite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i, id := range []string{"a", "b", "c", "d", "ask"} {
		mustCreateThread(t, s, id)
		mustExec(t, s.db, `UPDATE threads SET updated_at = ? WHERE id = ?`, i+1, id)
	}
	mustExec(t, s.db, `UPDATE threads SET mode = 'scratch' WHERE id = 'ask'`)
	mustExec(t, s.db, `INSERT INTO scratch_threads(thread_id, source_thread_id, return_mode, created_at, request_token) VALUES ('ask', 'a', 'chat', 1, 'token')`)
	seedThreadTicket(t, s, webLink("a"))
	seedThreadTicket(t, s, webLink("b"))
	seedThreadTicket(t, s, webLink("ask"))
	sameIDOtherSite := webLink("c")
	sameIDOtherSite.Ticket.Site = "https://other.example.com"
	seedThreadTicket(t, s, sameIDOtherSite)

	rows, err := s.ListThreadsForTicket(webTicket)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
		if r.TicketLink == nil || r.TicketLink.IssueID != "10001" {
			t.Fatalf("row %s link = %+v", r.ID, r.TicketLink)
		}
	}
	if !slices.Equal(ids, []string{"b", "a"}) {
		t.Fatalf("threads of the ticket = %v; want b, a", ids)
	}
	if _, err := s.ListThreadsForTicket(TicketRef{Kind: TrackerKindTempo, Site: "s", IssueID: "1"}); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("threads of a Tempo ticket: %v", err)
	}

	byThread, err := s.ListThreadTickets([]string{"a", "c", "d", "a", "", "missing"})
	if err != nil || len(byThread) != 2 || byThread["a"] != webLink("a") || byThread["c"] != sameIDOtherSite {
		t.Fatalf("batch read = %+v, %v", byThread, err)
	}
	if byThread, err := s.ListThreadTickets(nil); err != nil || len(byThread) != 0 {
		t.Fatalf("batch read of none = %+v, %v", byThread, err)
	}
	if got, err := s.ListThreadsLinkedToSite(TrackerKindJira, mirrorSite); err != nil || !slices.Equal(got, []string{"a", "ask", "b"}) {
		t.Fatalf("threads of the site = %v, %v", got, err)
	}
	if got, err := s.ListThreadsLinkedToSite(TrackerKindLinear, mirrorSite); err != nil || len(got) != 0 {
		t.Fatalf("threads of another kind = %v, %v", got, err)
	}
}

// A thread's delete takes its link, and so does its retirement to a
// holder, which then refuses a new one; a fork starts with its source's
// link, and keeps it when its source becomes a holder.
func TestThreadTicketGoesWithItsThreadAndItsForks(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	if err := s.DeleteThread("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedTicket(t, s, "a"); ok {
		t.Fatal("the deleted thread's link stayed")
	}

	seedLinearSource(t, s, "S", 1)
	mustSetThreadTicket(t, s, readLink(s, root, "S", titledCard("10001", 5, "Fix the login page")))
	source := mustGetThreadTicket(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	fork := mustGetThreadTicket(t, s, "F")
	fork.ThreadID = "S"
	if fork != source {
		t.Fatalf("fork link = %+v; want its source's %+v", fork, source)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRow(`SELECT mode FROM threads WHERE id = 'S'`).Scan(&mode); err != nil || mode != "holder" {
		t.Fatalf("S after its delete: mode %q, %v; want a holder", mode, err)
	}
	if _, ok := storedTicket(t, s, "S"); ok {
		t.Fatal("the holder kept its link")
	}
	if _, ok, err := s.GetThreadTicket("F"); err != nil || !ok {
		t.Fatalf("the fork lost its link: %v, %v", ok, err)
	}
	if _, err := s.SetThreadTicket(readLink(s, root, "S", titledCard("10001", 5, "Fix the login page"))); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("link of a holder: %v; want ErrInvalidThreadTicket", err)
	}
}

// A fork of an unlinked thread has no link, and a fork that keeps nothing
// of its source still starts with the link.
func TestForkCopiesTheLinkWhateverItsCut(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedLinearSource(t, s, "plain", 1)
	mustPointerFork(t, s, "plain", "plain-fork", ForkCut{})
	if _, ok := storedTicket(t, s, "plain-fork"); ok {
		t.Fatal("a fork of an unlinked thread has a link")
	}
	seedLinearSource(t, s, "S", 1)
	seedThreadTicket(t, s, webLink("S"))
	none := -1
	mustPointerFork(t, s, "S", "empty", ForkCut{ThroughTurn: &none})
	got, ok := storedTicket(t, s, "empty")
	if want := webLink("empty"); !ok || got != want {
		t.Fatalf("empty fork link = %+v, %v; want %+v", got, ok, want)
	}
}

// The tracker tables are application records: a restore brings back the
// snapshot's rows, and a link whose copy named a mirror root reads as not
// checked, since the restore emptied the mirror and no root ID returns.
func TestRestoreKeepsTheTrackerTables(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "linked")
	jira := mustCreateTrackerConnection(t, st, jiraConnection(mirrorSite, "user1@example.com", 1))
	tempo := mustCreateTrackerConnection(t, st, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 2})
	boards := []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}, {ConnectionID: jira.ID, BoardID: "11"}}
	if _, _, err := st.ReplaceProjectTrackerBoards("p1", boards); err != nil {
		t.Fatal(err)
	}
	root, _, err := st.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustSetThreadTicket(t, st, readLink(st, root, "t1", titledCard("10001", 5, "Fix the login page")))
	link := mustGetThreadTicket(t, st, "t1")
	connections, err := st.ListTrackerConnections()
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.GetThread("t1"); err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkCurrent {
		t.Fatalf("link before the snapshot = %+v, %v", row.TicketLink, err)
	}

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ClearThreadTicket("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTrackerConnection(tempo.ID); err != nil {
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
	if got := mustGetThreadTicket(t, st, "t1"); got != link {
		t.Fatalf("link after restore = %+v; want %+v", got, link)
	}
	newRoot, _, err := st.GetOrCreateMirrorRoot(jira.ID)
	if err != nil || newRoot == root {
		t.Fatalf("root after restore = %d, %v; want a new one, not %d", newRoot, err, root)
	}
	if row, err := st.GetThread("t1"); err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkNotChecked {
		t.Fatalf("link after restore = %+v, %v; want not checked", row.TicketLink, err)
	}
}

// The link column of a thread row reads by key: its guard that some
// thread has a link is a scalar subquery the row loop does not depend on,
// so it runs once per statement, and the link's row, root and connections
// are each one key lookup per thread.
func TestThreadLinkColumnReadsByKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(explainMirror(t, s, `SELECT `+threadTicketLinkColumn+` FROM threads`)), "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	want := []string{
		"SCAN threads USING COVERING INDEX sqlite_autoindex_threads_1",
		"SCALAR SUBQUERY 1",
		"SCAN thread_tickets USING COVERING INDEX sqlite_autoindex_thread_tickets_1",
		"CORRELATED SCALAR SUBQUERY 4",
		"SEARCH tt USING INDEX sqlite_autoindex_thread_tickets_1 (thread_id=?)",
		"SEARCH seen USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN",
		"CORRELATED SCALAR SUBQUERY 2",
		"SEARCH c USING INDEX idx_tracker_connections_site (kind=? AND site=?)",
		"CORRELATED SCALAR SUBQUERY 3",
		"SEARCH c USING INDEX idx_tracker_connections_site (kind=? AND site=?)",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("link column plan:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
