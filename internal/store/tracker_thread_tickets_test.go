package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
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
	mustExec(t, s.db, `INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		link.ThreadID, link.Ticket.Kind, link.Ticket.Site, link.Ticket.IssueID, link.IssueKey, link.IssueTitle,
		link.StatusID, link.StatusName, link.StatusGroup, link.Origin, link.LinkedAt)
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
	return ThreadTicketWrite{ThreadID: threadID, Origin: TicketOriginManual, LinkedAt: 10, Root: root, IssueID: card.IssueID, Card: card,
		Read: nextMirrorRead(s, root), Catalog: webStructure().Catalog, Sweep: currentSweep(s, root)}
}

// heldLink links threadID to the card of issue root's mirror holds.
func heldLink(root int64, threadID, issue string) ThreadTicketWrite {
	return ThreadTicketWrite{ThreadID: threadID, Origin: TicketOriginManual, LinkedAt: 10, Root: root, IssueID: issue}
}

func mustSetThreadTicket(t *testing.T, s *Store, w ThreadTicketWrite) ThreadTicketSet {
	t.Helper()
	set, err := s.SetThreadTicket(w)
	if err != nil {
		t.Fatalf("link %s to %s: %v", w.ThreadID, w.IssueID, err)
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
		StatusID: "1", StatusName: "To Do", StatusGroup: MirrorGroupToDo, Origin: TicketOriginManual, LinkedAt: 10}
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
		StatusID: "1", StatusName: "To Do", StatusGroup: "todo", Origin: TicketOriginManual, LinkedAt: 10,
		State: TicketLinkCurrent, ConnectionID: c.ID}
	if row.TicketLink == nil || *row.TicketLink != wantRow {
		t.Fatalf("thread row link = %+v\nwant %+v", row.TicketLink, wantRow)
	}

	if set := mustSetThreadTicket(t, s, heldLink(root, "a", "10001")); set.Changed || set.Replaced != nil {
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

	cleared, err := s.ClearThreadTicket("a")
	if err != nil || !cleared.Had || cleared.Ticket != (TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10002"}) {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	if again, err := s.ClearThreadTicket("a"); err != nil || again.Had {
		t.Fatalf("second clear = %+v, %v", again, err)
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
			w := heldLink(root, "a", "10001")
			w.Root = 0
			return w
		}, is(ErrInvalidThreadTicket)},
		{"no issue", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			return heldLink(root, "a", "")
		}, is(ErrInvalidThreadTicket)},
		{"read card of another issue", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10009", 5, "New"))
			w.IssueID = "10008"
			return w
		}, is(ErrInvalidThreadTicket)},
		{"held link with a card", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := heldLink(root, "a", "10001")
			w.Card = titledCard("10001", 5, "Fix the login page")
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
			if _, err := s.ClearThreadTicket("swept"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			return w
		}, is(ErrTrackerCacheSwept)},
		{"issue the mirror does not hold", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			return heldLink(root, "a", "10009")
		}, is(ErrThreadTicketNotHeld)},
		{"issue the mirror's latest read missed", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			mustCreateThread(t, s, "b")
			if _, err := s.MarkMirrorIssuesMissing(root, nextMirrorRead(s, root), []string{"10001"}); err != nil {
				t.Fatal(err)
			}
			return heldLink(root, "b", "10001")
		}, is(ErrThreadTicketNotHeld)},
		{"read card a later absence outranks", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := readLink(s, root, "a", titledCard("10001", 5, "Fix the login page"))
			if _, err := s.MarkMirrorIssuesMissing(root, mirrorReadOf(root, nextMirrorRead(s, root).rootOrder+1), []string{"10001"}); err != nil {
				t.Fatal(err)
			}
			return w
		}, is(ErrThreadTicketNotHeld)},
		{"connection turned off", func(t *testing.T, s *Store, root int64) ThreadTicketWrite {
			w := heldLink(root, "a", "10001")
			if _, _, err := s.SetTrackerConnectionEnabled(mustConnectionOf(t, s, root), false); err != nil {
				t.Fatal(err)
			}
			return w
		}, is(ErrThreadTicketNotReader)},
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
	for i, id := range []string{"a", "b", "c", "d", "ask", "side"} {
		mustCreateThread(t, s, id)
		mustExec(t, s.db, `UPDATE threads SET updated_at = ? WHERE id = ?`, i+1, id)
	}
	// An ask's hidden copy, and a person's side chat before Keep, which no
	// request owns; both carry their source's link.
	mustExec(t, s.db, `UPDATE threads SET mode = 'scratch' WHERE id IN ('ask', 'side')`)
	mustExec(t, s.db, `INSERT INTO scratch_threads(thread_id, source_thread_id, return_mode, created_at, request_token) VALUES ('ask', 'a', 'chat', 1, 'token')`)
	mustExec(t, s.db, `INSERT INTO scratch_threads(thread_id, source_thread_id, return_mode, created_at) VALUES ('side', 'a', 'chat', 1)`)
	seedThreadTicket(t, s, webLink("a"))
	seedThreadTicket(t, s, webLink("b"))
	seedThreadTicket(t, s, webLink("ask"))
	seedThreadTicket(t, s, webLink("side"))
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
	// Keep makes the side chat an ordinary thread, with the link it carried.
	if _, err := s.PromoteScratchThread("side"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListThreadsForTicket(webTicket); err != nil || len(rows) != 3 || rows[0].ID != "side" {
		t.Fatalf("threads of the ticket after Keep = %+v, %v; want side first", rows, err)
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
	if got, err := s.ListThreadsLinkedToSite(TrackerKindJira, mirrorSite); err != nil || !slices.Equal(got, []string{"a", "ask", "b", "side"}) {
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
// snapshot's rows, who a person said each forge account is among them,
// and a link reads its copy as not checked, since the restore emptied
// the mirror.
func TestRestoreKeepsTheTrackerTables(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "linked")
	jira := mustCreateTrackerConnection(t, st, jiraConnection(mirrorSite, "user1@example.com", 1))
	tempo := mustCreateTrackerConnection(t, st, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 2})
	boards := []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}, {ConnectionID: jira.ID, BoardID: "11"}}
	if _, _, err := replaceFromStored(st, "p1", boards); err != nil {
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
	pair(t, st, jira.ID, "user2", "acc-user2", "User 2")
	pair(t, st, jira.ID, "ci", "", "")
	pairings, err := st.ListTrackerForgePairings(jira.ID)
	if err != nil || len(pairings) != 2 {
		t.Fatalf("pairings before the snapshot = %+v, %v", pairings, err)
	}
	if row, err := st.GetThread("t1"); err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkCurrent {
		t.Fatalf("link before the snapshot = %+v, %v", row.TicketLink, err)
	}

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearThreadTicket("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTrackerConnection(tempo.ID); err != nil {
		t.Fatal(err)
	}
	// A pairing changed, one removed and one added since the snapshot.
	pair(t, st, jira.ID, "user2", "acc-user3", "User 3")
	pair(t, st, jira.ID, "later", "acc-user4", "User 4")
	if _, err := st.DeleteTrackerForgePairing(jira.ID, "github.com", "ci", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}

	// The copy inserts the Tempo row, on, after its Jira row (a Tempo row
	// is always created after its parent), so its trigger advances the
	// Jira row's time source generation past the snapshot's: harmless, as
	// the restore empties the mirror and no root of before it remains.
	connections[0].TimeSourceGen++
	if got, err := st.ListTrackerConnections(); err != nil || !slices.Equal(got, connections) || got[1].ID != tempo.ID {
		t.Fatalf("connections after restore = %+v, %v; want %+v", got, err, connections)
	}
	if got, err := st.ListProjectTrackerBoards("p1"); err != nil || !slices.Equal(got, boards) {
		t.Fatalf("boards after restore = %+v, %v", got, err)
	}
	if got := mustGetThreadTicket(t, st, "t1"); got != link {
		t.Fatalf("link after restore = %+v; want %+v", got, link)
	}
	if got, err := st.ListTrackerForgePairings(jira.ID); err != nil || !slices.Equal(got, pairings) {
		t.Fatalf("pairings after restore = %+v, %v; want %+v", got, err, pairings)
	}
	newRoot, _, err := st.GetOrCreateMirrorRoot(jira.ID)
	if err != nil || newRoot == root {
		t.Fatalf("root after restore = %d, %v; want a new one, not %d", newRoot, err, root)
	}
	if row, err := st.GetThread("t1"); err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkNotChecked {
		t.Fatalf("link after restore = %+v, %v; want not checked", row.TicketLink, err)
	}
}

// A root ID is this database's: restored into another database, whose
// own roots count from its own sequence, a link reads its site's reader's
// root alone, so a root the destination makes for another site with the
// ID the link's mirror had is never read for it. The ticket and its last
// seen key, title and status stay.
func TestRestoreFromAnotherDatabaseReadsNoOtherSitesRoot(t *testing.T) {
	src := snapshotTestStore(t)
	seedSnapshotFixture(t, src, "t1", "linked")
	siteA := mustCreateTrackerConnection(t, src, jiraConnection(mirrorSite, "user1@example.com", 1))
	siteB := mustCreateTrackerConnection(t, src, jiraConnection("https://other.example.com", "user1@example.com", 2))
	root, _, err := src.GetOrCreateMirrorRoot(siteA.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustSetThreadTicket(t, src, readLink(src, root, "t1", titledCard("10001", 5, "Fix the login page")))
	seen := mustGetThreadTicket(t, src, "t1")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}

	dst := snapshotTestStore(t)
	if _, err := dst.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	if other, _, err := dst.GetOrCreateMirrorRoot(siteB.ID); err != nil || other != root {
		t.Fatalf("the destination's first root = %d, %v; this test needs it to be %d, the root the link saw", other, err, root)
	}
	row, err := dst.GetThread("t1")
	if err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkNotChecked || row.TicketLink.ConnectionID != siteA.ID {
		t.Fatalf("restored link row = %+v, %v; want not checked through %s", row.TicketLink, err, siteA.ID)
	}
	if got := mustGetThreadTicket(t, dst, "t1"); got != seen {
		t.Fatalf("restored link = %+v; want %+v", got, seen)
	}
}

// The link column of a thread row reads by key: its guard that some
// thread has a link is a scalar subquery the row loop does not depend on,
// so it runs once per statement, and the link's row, its reader's root,
// card, absence and status, and the site's connections are each one key
// lookup per thread. A list computes every link once in its links CTE,
// with the same lookups per link, and probes it once per thread; a list of
// part of the catalog reads the column per row.
func TestThreadLinkColumnReadsByKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// The subqueries' numbers are the planner's and differ between the two
	// statements; their order and lookups are the contract.
	numbered := regexp.MustCompile(`SUBQUERY \d+`)
	plan := func(query string) []string {
		var lines []string
		for line := range strings.SplitSeq(strings.TrimSpace(explainMirror(t, s, query)), "\n") {
			lines = append(lines, numbered.ReplaceAllString(strings.TrimSpace(line), "SUBQUERY"))
		}
		return lines
	}
	link := []string{
		"SEARCH rr USING COVERING INDEX sqlite_autoindex_tracker_cache_roots_1 (connection_id=?) LEFT-JOIN",
		"CORRELATED SCALAR SUBQUERY",
		"SEARCH reader USING INDEX idx_tracker_connections_site (kind=? AND site=?)",
		"SEARCH i USING INDEX sqlite_autoindex_tracker_cache_issues_1 (root_id=? AND issue_id=?) LEFT-JOIN",
		"SEARCH m USING PRIMARY KEY (root_id=? AND issue_id=?) LEFT-JOIN",
		"SEARCH st USING PRIMARY KEY (root_id=? AND status_id=?) LEFT-JOIN",
		"CORRELATED SCALAR SUBQUERY",
		"SEARCH c USING INDEX idx_tracker_connections_site (kind=? AND site=?)",
		"CORRELATED SCALAR SUBQUERY",
		"SEARCH reader USING INDEX idx_tracker_connections_site (kind=? AND site=?)",
	}
	row := slices.Concat([]string{
		"SCAN threads USING COVERING INDEX sqlite_autoindex_threads_1",
		"SCALAR SUBQUERY",
		"SCAN thread_tickets USING COVERING INDEX sqlite_autoindex_thread_tickets_1",
		"CORRELATED SCALAR SUBQUERY",
		"SEARCH tt USING INDEX sqlite_autoindex_thread_tickets_1 (thread_id=?)",
	}, link)
	if got := plan(`SELECT ` + threadTicketLinkColumn + ` FROM threads`); !slices.Equal(got, row) {
		t.Fatalf("link column plan:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(row, "\n"))
	}
	list := slices.Concat([]string{"MATERIALIZE links", "SCAN tt"}, link, []string{
		"SCAN threads USING COVERING INDEX sqlite_autoindex_threads_1",
		"BLOOM FILTER ON links (thread_id=?)",
		"SEARCH links USING AUTOMATIC COVERING INDEX (thread_id=?) LEFT-JOIN",
	})
	got := plan(`WITH links AS MATERIALIZED (` + threadTicketLinksSQL + `) SELECT links.link FROM threads LEFT JOIN links ON links.thread_id = threads.id`)
	if !slices.Equal(got, list) {
		t.Fatalf("list links plan:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(list, "\n"))
	}
	withItems := listThreadsWithItemsQuery()
	for _, query := range []string{listThreadsQuery, withItems} {
		if !strings.Contains(query, `LEFT JOIN links ON links.thread_id = threads.id`) || strings.Count(query, "links") != 4 {
			t.Fatalf("a whole-catalog list names its links CTE other than once:\n%s", query)
		}
	}
	// A list of part of the catalog reads its rows' links per row: its plan
	// holds the column's probe of the link by thread and no links CTE.
	project, _ := listProjectThreadsQuery("p")
	archive := listArchivedThreadsQuery()
	for name, query := range map[string]string{"the project list": project, "the archive list": archive} {
		got := plan(query)
		if slices.Contains(got, "MATERIALIZE links") || !slices.Contains(got, "SEARCH tt USING INDEX sqlite_autoindex_thread_tickets_1 (thread_id=?)") {
			t.Fatalf("%s plan:\n%s\nwant the link probed by thread and no links CTE", name, strings.Join(got, "\n"))
		}
	}
}

// A link names its status's group as the wire does, and a carried link's
// name is stored as the group's number: each group both ways, no group as
// "", and a number no group has as "".
func TestTheLinksStatusGroupIsItsName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for n := 0; n <= MirrorGroupLeftOut; n++ {
		want := ""
		if n < len(MirrorGroupNames) {
			want = MirrorGroupNames[n]
		}
		var got string
		if err := s.reader().QueryRow(`SELECT `+mirrorGroupNameSQL(`?`), n).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("the name of group %d = %q, want %q", n, got, want)
		}
		if back, ok := mirrorGroupNumber(got); !ok || (got != "" && back != n) {
			t.Fatalf("the number of %q = %d, %v; want %d", got, back, ok, n)
		}
	}
	if _, ok := mirrorGroupNumber("left_out"); ok {
		t.Fatal("a name no status group has was read as one")
	}
}
