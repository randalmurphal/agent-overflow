package store

import (
	"errors"
	"slices"
	"testing"
)

// linkState is the state threadID's row shows for its link.
func linkState(t *testing.T, s *Store, threadID string) string {
	t.Helper()
	row, err := s.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TicketLink == nil {
		return ""
	}
	return row.TicketLink.State
}

// A card write copies the card to the links of its ticket only when it
// moves something a link shows: an identical card writes no link and
// names no thread, a changed one names the thread, an older card the
// mirror supersedes copies nothing, and the status's name and group come
// from the root's catalog.
func TestCardWritesCopyToLinksOnlyWhatMoves(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	linkIssue(t, s, "other-issue", "10002")
	write := func(card MirrorCard) MirrorChanges {
		t.Helper()
		changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, MirrorCatalog{}, nextMirrorRead(s, root))
		if err != nil {
			t.Fatal(err)
		}
		return changes
	}

	if got := write(titledCard("10001", 5, "Fix the login page")); len(got.Threads) != 0 {
		t.Fatalf("an identical card named %v", got.Threads)
	}
	data := titledCard("10001", 6, "Fix the login page")
	data.Data = []byte(`{"title":"Issue 10001","description":"more"}`)
	if got := write(data); !slices.Equal(got.Cards, []string{"10001"}) || len(got.Threads) != 0 {
		t.Fatalf("a card that moves nothing a link shows = %+v; want the card stored and no thread", got)
	}

	moved := titledCard("10001", 7, "Fix the login page on mobile")
	moved.StatusID = "3"
	if got := write(moved); !slices.Equal(got.Threads, []string{"a"}) {
		t.Fatalf("a moved card named %v; want a", got.Threads)
	}
	link := mustGetThreadTicket(t, s, "a")
	if link.IssueTitle != "Fix the login page on mobile" || link.StatusID != "3" || link.StatusName != "In Progress" || link.StatusGroup != MirrorGroupInProgress {
		t.Fatalf("link after the move = %+v", link)
	}

	older := titledCard("10001", 6, "Old title")
	if got := write(older); !slices.Equal(got.Superseded, []string{"10001"}) || len(got.Threads) != 0 {
		t.Fatalf("an older card = %+v; want it superseded and no thread", got)
	}
	if got := mustGetThreadTicket(t, s, "a"); got != link {
		t.Fatalf("link after an older card = %+v; want %+v", got, link)
	}

	unknown := titledCard("10001", 8, "Fix the login page on mobile")
	unknown.StatusID = "77"
	if got := write(unknown); !slices.Equal(got.Threads, []string{"a"}) {
		t.Fatalf("a card of an unknown status named %v", got.Threads)
	}
	if got := mustGetThreadTicket(t, s, "a"); got.StatusID != "77" || got.StatusName != "" || got.StatusGroup != 0 {
		t.Fatalf("link of a status the catalog lacks = %+v; want no name and no group", got)
	}
}

// The row page and the ticket writers copy to links as the card upsert
// does: all three go through storeMirrorCardsTx.
func TestEveryCardWriterCopiesToLinks(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))

	row := MirrorRow{Card: titledCard("10001", 6, "From the board"), OrderKey: "a"}
	page := webPage("10", row)
	page.ReadAt = nextMirrorRead(s, root)
	if got := mustWriteRowPage(t, s, root, page, nil); !slices.Equal(got.Threads, []string{"a"}) {
		t.Fatalf("a row page named %v", got.Threads)
	}
	if got := mustGetThreadTicket(t, s, "a"); got.IssueTitle != "From the board" {
		t.Fatalf("link after a row page = %+v", got)
	}

	ticket := MirrorTicket{Card: titledCard("10001", 7, "From the ticket"), Body: []byte(`{}`)}
	read := nextMirrorRead(s, root)
	w, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{ticket}, MirrorCatalog{}, read, read)
	if err != nil || !slices.Equal(w.Changes.Threads, []string{"a"}) {
		t.Fatalf("a ticket write named %+v, %v", w.Changes, err)
	}
	if got := mustGetThreadTicket(t, s, "a"); got.IssueTitle != "From the ticket" {
		t.Fatalf("link after a ticket write = %+v", got)
	}
}

// A read that did not return a linked issue marks the link not found,
// unless the mirror holds a card from a read that began after it; the
// identical card read again makes the link current.
func TestMissingIssueMarksItsLinksUntilARead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustCreateThread(t, s, "a")
	early := nextMirrorRead(s, root)
	w := readLink(s, root, "a", titledCard("10001", 5, "Fix the login page"))
	w.Read = mirrorReadOf(root, early.rootOrder+1)
	mustSetThreadTicket(t, s, w)
	if got, err := s.MarkMirrorLinksMissing(root, early, []string{"10001"}); err != nil || len(got) != 0 {
		t.Fatalf("a read from before the card's = %v, %v; want no link marked", got, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkCurrent {
		t.Fatalf("state = %q; want current", state)
	}
	if got, err := s.MarkMirrorLinksMissing(root, nextMirrorRead(s, root), []string{"10001", "10009"}); err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a later read = %v, %v; want a marked", got, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkNotFound {
		t.Fatalf("state = %q; want not found", state)
	}
	if got, err := s.MarkMirrorLinksMissing(root, nextMirrorRead(s, root), []string{"10001"}); err != nil || len(got) != 0 {
		t.Fatalf("marked again = %v, %v; want no change", got, err)
	}
	if _, err := s.MarkMirrorLinksMissing(root, MirrorRead{}, []string{"10001"}); err == nil {
		t.Fatal("a mark with no read was taken")
	}

	changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{titledCard("10001", 5, "Fix the login page")}, MirrorCatalog{}, nextMirrorRead(s, root))
	if err != nil || !slices.Equal(changes.Threads, []string{"a"}) {
		t.Fatalf("the identical card read again = %+v, %v; want a", changes, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkCurrent {
		t.Fatalf("state = %q; want current", state)
	}
}

// A link shows current or not found while its root's connection stays
// attached; otherwise its state is that of the site's connections: not
// checked while one is on, turned off when all are off, no connection
// when none is left. Its connection is its root's, else the oldest one on.
func TestThreadRowLinkStateFollowsTheConnections(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustCreateThread(t, s, "plain")
	if row, err := s.GetThread("plain"); err != nil || row.TicketLink != nil {
		t.Fatalf("an unlinked thread's link = %+v, %v", row.TicketLink, err)
	}
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	connection := func() string {
		row, err := s.GetThread("a")
		if err != nil {
			t.Fatal(err)
		}
		return row.TicketLink.ConnectionID
	}
	if state, id := linkState(t, s, "a"), connection(); state != TicketLinkCurrent || id != c.ID {
		t.Fatalf("linked = %q on %q; want current on %q", state, id, c.ID)
	}
	if row, err := s.GetThread("plain"); err != nil || row.TicketLink != nil {
		t.Fatalf("an unlinked thread beside a link = %+v, %v", row.TicketLink, err)
	}

	// TicketConnection names the connection a new link reads through.
	site := func(wantID, wantState string) {
		t.Helper()
		if id, state, err := s.TicketConnection(webTicket); err != nil || id != wantID || state != wantState {
			t.Fatalf("ticket connection = %q, %q, %v; want %q, %q", id, state, err, wantID, wantState)
		}
	}
	site(c.ID, TicketLinkNotChecked)
	if _, _, err := s.TicketConnection(TicketRef{Kind: TrackerKindTempo, Site: mirrorSite, IssueID: "1"}); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("ticket connection of a Tempo ticket: %v", err)
	}
	if _, err := s.DetachMirror(c.ID); err != nil {
		t.Fatal(err)
	}
	if state, id := linkState(t, s, "a"), connection(); state != TicketLinkNotChecked || id != c.ID {
		t.Fatalf("detached = %q on %q; want not checked on %q", state, id, c.ID)
	}
	mustExec(t, s.db, `UPDATE tracker_connections SET enabled = 0 WHERE id = ?`, c.ID)
	if state, id := linkState(t, s, "a"), connection(); state != TicketLinkTurnedOff || id != "" {
		t.Fatalf("off = %q on %q; want turned off on none", state, id)
	}
	site("", TicketLinkTurnedOff)
	second := mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user2@example.com", 20))
	if state, id := linkState(t, s, "a"), connection(); state != TicketLinkNotChecked || id != second.ID {
		t.Fatalf("another on = %q on %q; want not checked on %q", state, id, second.ID)
	}
	site(second.ID, TicketLinkNotChecked)
	for _, id := range []string{second.ID, c.ID} {
		if _, err := s.DeleteTrackerConnection(id); err != nil {
			t.Fatal(err)
		}
	}
	if state, id := linkState(t, s, "a"), connection(); state != TicketLinkNoConnection || id != "" {
		t.Fatalf("removed = %q on %q; want no connection on none", state, id)
	}
	site("", TicketLinkNoConnection)
}

// The issues a mirror holds that no link, board or open ticket keeps any
// more are listed by connection and root; a kept one, one the mirror does
// not hold, and one of another site are not.
func TestListUnkeptLinkedIssues(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "gone", "10001")
	linkIssue(t, s, "kept", "10002")
	linkIssue(t, s, "also", "10002")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5), webCard("10002", 5)}, MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gone", "kept"} {
		if _, _, err := s.ClearThreadTicket(id); err != nil {
			t.Fatal(err)
		}
	}
	ticket := func(site, id string) TicketRef { return TicketRef{Kind: TrackerKindJira, Site: site, IssueID: id} }
	got, err := s.ListUnkeptLinkedIssues([]TicketRef{ticket(mirrorSite, "10001"), ticket(mirrorSite, "10002"), ticket(mirrorSite, "10009"),
		ticket("https://other.example.com", "10001"), ticket(mirrorSite, "10001")})
	if want := []MirrorIssueRef{{ConnectionID: c.ID, Root: root, IssueID: "10001"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("unkept = %+v, %v; want %+v", got, err, want)
	}
	if got, err := s.ListUnkeptLinkedIssues(nil); err != nil || got != nil {
		t.Fatalf("unkept of none = %+v, %v", got, err)
	}
}
