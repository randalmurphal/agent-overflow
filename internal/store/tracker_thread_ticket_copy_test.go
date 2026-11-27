package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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

// A card write that stores a card copies it to the links of its ticket
// and names their threads, whatever of the card changed; an identical
// card and an older card the mirror supersedes copy nothing and name no
// thread; the status's name and group come from the root's catalog; a
// link to another issue is not named.
func TestCardWritesCopyTheCardsTheyStore(t *testing.T) {
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
	if got := write(data); !slices.Equal(got.Cards, []string{"10001"}) || !slices.Equal(got.Threads, []string{"a"}) {
		t.Fatalf("a card that moves nothing a link shows = %+v; want the card stored and a named", got)
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

// A read that did not return a linked issue makes the link not found,
// unless the mirror holds a card from a read that began after it; a mark
// of an issue already not found names no thread; an identical card from
// an earlier read leaves it not found, and one from a later read makes the
// link current again, clearing the absence. An absence of an issue no
// link keeps goes with the cleanup.
func TestMissingIssueMarksItsLinksUntilARead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustCreateThread(t, s, "a")
	early := nextMirrorRead(s, root)
	w := readLink(s, root, "a", titledCard("10001", 5, "Fix the login page"))
	w.Read = mirrorReadOf(root, early.rootOrder+1)
	mustSetThreadTicket(t, s, w)
	if got, err := s.MarkMirrorIssuesMissing(root, early, []string{"10001"}); err != nil || len(got) != 0 {
		t.Fatalf("a read from before the card's = %v, %v; want no link marked", got, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkCurrent {
		t.Fatalf("state = %q; want current", state)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_missing`, "0")
	missed := nextMirrorRead(s, root)
	if got, err := s.MarkMirrorIssuesMissing(root, missed, []string{"10001", "10009"}); err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a later read = %v, %v; want a marked", got, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkNotFound {
		t.Fatalf("state = %q; want not found", state)
	}
	if got, err := s.MarkMirrorIssuesMissing(root, mirrorReadOf(root, missed.rootOrder+2), []string{"10001"}); err != nil || len(got) != 0 {
		t.Fatalf("marked again = %v, %v; want no change", got, err)
	}
	if _, err := s.MarkMirrorIssuesMissing(root, MirrorRead{}, []string{"10001"}); err == nil {
		t.Fatal("a mark with no read was taken")
	}

	card := []MirrorCard{titledCard("10001", 5, "Fix the login page")}
	changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), card, MirrorCatalog{}, mirrorReadOf(root, missed.rootOrder+1))
	if err != nil || len(changes.Threads) != 0 || linkState(t, s, "a") != TicketLinkNotFound {
		t.Fatalf("the identical card from a read before the latest absence = %+v, %v, %s; want no thread, not found", changes, err, linkState(t, s, "a"))
	}
	changes, err = s.UpsertMirrorCards(root, currentSweep(s, root), card, MirrorCatalog{}, nextMirrorRead(s, root))
	if err != nil || !slices.Equal(changes.Threads, []string{"a"}) || len(changes.Cards) != 0 {
		t.Fatalf("the identical card read again = %+v, %v; want a and no card stored", changes, err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkCurrent {
		t.Fatalf("state = %q; want current", state)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issue_missing`, "10009")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_missing`, "0")
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
		if _, err := s.ClearThreadTicket(id); err != nil {
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

// Two accounts of one site: only the site's link reader, its lowest
// connection ID turned on, writes, keeps and shows the site's links.
// Unchanged cards from both mirrors, alternating, the other's kept by a
// ticket a person has open through it, name no thread and leave the link
// current through the reader; the other mirror's card of another key, its
// missing read, and its linked loop move nothing of the link, and that
// loop polls no issue only a link names.
func TestASitesLinksFollowItsLinkReaderOnly(t *testing.T) {
	t.Parallel()
	s, first, firstRoot := newMirror(t)
	second := mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user2@example.com", 20))
	secondRoot, _, err := s.GetOrCreateMirrorRoot(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader, readerRoot, otherRoot := first, firstRoot, secondRoot
	if second.ID < first.ID {
		reader, readerRoot, otherRoot = second, secondRoot, firstRoot
	}
	mustCreateThread(t, s, "a")
	card := titledCard("10001", 5, "Fix the login page")
	mustSetThreadTicket(t, s, readLink(s, readerRoot, "a", card))
	if _, _, err := s.KeepMirrorTicket(otherRoot, "10001", 1, 1, nextMirrorRead(s, otherRoot)); err != nil {
		t.Fatal(err)
	}
	write := func(root int64) []string {
		t.Helper()
		changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{card}, webStructure().Catalog, nextMirrorRead(s, root))
		if err != nil {
			t.Fatal(err)
		}
		return changes.Threads
	}
	current := func(what string) {
		t.Helper()
		row, err := s.GetThread("a")
		if err != nil || row.TicketLink == nil || row.TicketLink.State != TicketLinkCurrent || row.TicketLink.ConnectionID != reader.ID {
			t.Fatalf("%s: link = %+v, %v; want current through the reader %s", what, row.TicketLink, err, reader.ID)
		}
	}
	for round := range 2 {
		if got := write(otherRoot); len(got) != 0 {
			t.Fatalf("round %d: the other account's unchanged card named %v", round, got)
		}
		current("after the other account's card")
		if got := write(readerRoot); len(got) != 0 {
			t.Fatalf("round %d: the reader's unchanged card named %v", round, got)
		}
		current("after the reader's card")
	}

	renamed := card
	renamed.Key, renamed.UpdatedAt = "OTHER-1", 6
	if got := write(otherRoot); len(got) != 0 {
		t.Fatalf("the other account's unchanged card named %v", got)
	}
	if changes, err := s.UpsertMirrorCards(otherRoot, currentSweep(s, otherRoot), []MirrorCard{renamed}, MirrorCatalog{}, nextMirrorRead(s, otherRoot)); err != nil || len(changes.Threads) != 0 {
		t.Fatalf("the other account's renamed card = %+v, %v; want no thread", changes, err)
	}
	if link := threadLink(t, s, "a"); link.Key != card.Key {
		t.Fatalf("link after the other account's renamed card = %+v; want the reader's key %s", link, card.Key)
	}
	if got, err := s.MarkMirrorIssuesMissing(otherRoot, nextMirrorRead(s, otherRoot), []string{"10001"}); err != nil || len(got) != 0 {
		t.Fatalf("the other account's missing read marked %v, %v", got, err)
	}
	current("after the other account's missing read")
	if got, _, err := s.ListMirrorOffBoardLinkedIssues(otherRoot, "", nil); err != nil || len(got) != 0 {
		t.Fatalf("the other account polls %v, %v; want none", got, err)
	}
	if got, _, err := s.ListMirrorOffBoardLinkedIssues(readerRoot, "", nil); err != nil || !slices.Equal(got, []string{"10001"}) {
		t.Fatalf("the reader polls %v, %v; want 10001", got, err)
	}
}

// A link shows only the card the mirror kept (mirrorCardSupersededSQL,
// mirrorCardDiffersSQL): after a read from later stored the renamed key
// NEW-1 at the same update time, an older read's OLD-1 copies nothing and
// names no thread, and a link of another thread from that older read, or
// from the mirror, shows NEW-1.
func TestALinkCopiesOnlyTheCardTheMirrorKept(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	for _, id := range []string{"a", "b", "c"} {
		mustCreateThread(t, s, id)
	}
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	n := nextMirrorRead(s, root).rootOrder
	early, later := mirrorReadOf(root, n), mirrorReadOf(root, n+1)
	renamed, stale := titledCard("10001", 5, "Fix the login page"), titledCard("10001", 5, "Fix the login page")
	renamed.Key, stale.Key = "NEW-1", "OLD-1"
	if got, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{renamed}, MirrorCatalog{}, later); err != nil || !slices.Equal(got.Threads, []string{"a"}) {
		t.Fatalf("the later read's rename = %+v, %v; want a", got, err)
	}

	got, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{stale}, MirrorCatalog{}, early)
	if err != nil || len(got.Threads) != 0 {
		t.Fatalf("the older read's card = %+v, %v; want no thread", got, err)
	}
	if link := mustGetThreadTicket(t, s, "a"); link.IssueKey != "NEW-1" || linkState(t, s, "a") != TicketLinkCurrent {
		t.Fatalf("link after the older read = %+v, %s; want NEW-1, current", link, linkState(t, s, "a"))
	}

	cold := readLink(s, root, "b", stale)
	cold.Read = early
	if set := mustSetThreadTicket(t, s, cold); !slices.Equal(set.Cards.Superseded, []string{"10001"}) || len(set.Cards.Threads) != 0 {
		t.Fatalf("a link from the older read = %+v; want its card superseded and no other thread named", set)
	}
	mustSetThreadTicket(t, s, heldLink(root, "c", "10001"))
	for _, id := range []string{"a", "b", "c"} {
		if link, row := mustGetThreadTicket(t, s, id), threadLink(t, s, id); link.IssueKey != "NEW-1" || row.Key != "NEW-1" || row.State != TicketLinkCurrent {
			t.Fatalf("%s: link %+v, row %+v; want NEW-1, current", id, link, row)
		}
	}
}

// A board page that stores 100 changed cards runs the absence clear and
// the link copy once each, and what they cost does not grow with the links
// a page does not name: with no link and with 500 links of another site
// the copy stops at its guard at the same cost, and 500 links of this site
// to issues off the page cost no more than one does (at most 11/10). The
// clear stops at its guard while the root has no absence. 10 links to
// the page's issues are copied and named. The page write's time in each
// case is logged.
func TestTheLinkCopyCostsThePagesLinks(t *testing.T) {
	const page, onPage = 100, 10
	type cost struct {
		clear, copy, threads int
		took                 time.Duration
	}
	measure := func(links int, site string, linkedOnPage int) cost {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		rows := mirrorRows(20001, page, "a")
		write := func(updated int64) MirrorChanges {
			for i := range rows {
				rows[i].Card.UpdatedAt = updated
			}
			p := webPage("10", rows...)
			p.ReadAt = nextMirrorRead(s, root)
			return mustWriteRowPage(t, s, root, p, nil)
		}
		write(5)
		for i := range links {
			id := fmt.Sprintf("linked-%03d", i)
			mustCreateThread(t, s, id)
			issue := 30001 + i
			if i < linkedOnPage {
				issue = 20001 + i
			}
			mustExec(t, s.db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at)
				VALUES (?1, 'jira', ?2, ?3, 'WEB-1', 'Issue', 'manual', 1)`, id, site, fmt.Sprint(issue))
		}
		var c cost
		var changes MirrorChanges
		c.clear, _ = countStatementRuns(t, mirrorAbsencesClearSQL, func() {
			var copyRuns int
			copyRuns, c.copy = countStatementRuns(t, mirrorLinkCopySQL, func() { changes = write(6) })
			if copyRuns != 1 {
				t.Fatalf("the link copy ran %d times; want once", copyRuns)
			}
		})
		c.threads = len(changes.Threads)
		var times []time.Duration
		for updated := range int64(5) {
			began := time.Now()
			write(7 + updated)
			times = append(times, time.Since(began))
		}
		slices.Sort(times)
		c.took = times[len(times)/2]
		return c
	}
	none := measure(0, mirrorSite, 0)
	other := measure(500, "https://other.example.com", 0)
	one := measure(1, mirrorSite, 0)
	off := measure(500, mirrorSite, 0)
	on := measure(500, mirrorSite, onPage)
	t.Logf("copy VM steps, page write time: no link %d, %v; 500 of another site %d, %v; 1 of this site off the page %d, %v; 500 off the page %d, %v; 500 with %d on the page %d, %v",
		none.copy, none.took, other.copy, other.took, one.copy, one.took, off.copy, off.took, onPage, on.copy, on.took)
	for name, c := range map[string]cost{"no link": none, "another site": other, "one off the page": one, "500 off the page": off, "on the page": on} {
		if c.clear != 1 {
			t.Errorf("%s: the absence clear ran %d times; want once", name, c.clear)
		}
	}
	if other.copy != none.copy {
		t.Errorf("500 links of another site cost the copy %d steps, no link %d; want the guard to stop it alike", other.copy, none.copy)
	}
	if 10*off.copy > 11*one.copy {
		t.Errorf("500 links off the page cost the copy %d steps, one link %d; want the links the page does not name to cost nothing", off.copy, one.copy)
	}
	if none.threads != 0 || other.threads != 0 || off.threads != 0 || on.threads != onPage {
		t.Errorf("named threads: %d, %d, %d, %d; want 0, 0, 0, %d", none.threads, other.threads, off.threads, on.threads, onPage)
	}
}

// The link copy and the missing mark's link read parse no bound list for
// a write that can name no link: with no link, with links of another
// site only, and on a root that is not its site's link reader, each costs
// the same for 1 issue as for 1,000. For the reader with a link of its
// site, the list is read.
func TestTheLinkWritersOfNoLinkParseNoList(t *testing.T) {
	t.Parallel()
	list := func(n int) string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("%q", fmt.Sprint(20001+i))
		}
		return "[" + strings.Join(ids, ",") + "]"
	}
	s, low, _, _, root := twoAccounts(t)
	steps := func(query string, n int, args ...any) int {
		return statementVMSteps(t, s, query, append([]any{root, list(n)}, args...)...)
	}
	check := func(name string, read bool) {
		t.Helper()
		for _, q := range []struct {
			name  string
			query string
			args  []any
		}{{"the link copy", mirrorLinkCopySQL, nil}, {"the missing links read", mirrorMissingLinksSQL, []any{int64(1)}}} {
			one, many := steps(q.query, 1, q.args...), steps(q.query, 1000, q.args...)
			if read != (many > one) {
				t.Errorf("%s: %s costs %d steps for 1 issue and %d for 1,000; want the list read %v", name, q.name, one, many, read)
			}
		}
	}
	check("no link", false)
	mustCreateThread(t, s, "a")
	mustExec(t, s.db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at)
		VALUES ('a', 'jira', 'https://other.example.com', '20001', 'WEB-1', 'Issue', 'manual', 1)`)
	check("a link of another site", false)
	mustExec(t, s.db, `UPDATE thread_tickets SET site = ?1`, mirrorSite)
	check("the reader with a link of its site", true)
	setConnectionEnabled(t, s, low.ID, true)
	check("a root that is not the reader", false)
}

// A project's thread list and the archive list project the links of the
// threads they return alone: with 500 other threads, one or all 500 of
// them linked, a list of one linked project thread, and of one linked
// archived thread, costs the same.
func TestSelectiveThreadListsProjectTheirOwnLinks(t *testing.T) {
	t.Parallel()
	measure := func(linkedElsewhere int) (project, archive int) {
		s, _, root := newMirror(t)
		if _, err := s.CreateProject(Project{ID: "p", Path: "/tmp/p", Name: "p", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		one := makeThread("one", "claude")
		one.ProjectID = "p"
		if err := s.CreateThread(one); err != nil {
			t.Fatal(err)
		}
		mustCreateThread(t, s, "archived")
		if _, _, err := s.ArchiveThread("archived"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"one", "archived"} {
			mustSetThreadTicket(t, s, readLink(s, root, id, titledCard("10001", 5, "Fix the login page")))
		}
		for i := range 500 {
			id := fmt.Sprintf("other-%03d", i)
			mustCreateThread(t, s, id)
			if i < linkedElsewhere {
				mustExec(t, s.db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at)
					VALUES (?1, 'jira', ?2, '10001', 'WEB-1', 'Issue', 'manual', 1)`, id, mirrorSite)
			}
		}
		projectQuery, projectArgs := listProjectThreadsQuery("p")
		archiveQuery := listArchivedThreadsQuery()
		for _, list := range []func() ([]Thread, error){func() ([]Thread, error) { return s.ListThreadsByProject("p") }, s.ListArchivedThreads} {
			threads, err := list()
			if err != nil || len(threads) != 1 || threads[0].TicketLink == nil || threads[0].TicketLink.State != TicketLinkCurrent {
				t.Fatalf("listed %+v, %v; want the one linked thread", threads, err)
			}
		}
		return statementVMSteps(t, s, projectQuery, projectArgs...), statementVMSteps(t, s, archiveQuery)
	}
	fewProject, fewArchive := measure(1)
	manyProject, manyArchive := measure(500)
	t.Logf("VM steps with 1 and 500 links elsewhere: the project list %d, %d; the archive list %d, %d", fewProject, manyProject, fewArchive, manyArchive)
	if manyProject != fewProject || manyArchive != fewArchive {
		t.Errorf("500 links elsewhere cost the project list %d steps and the archive list %d, 1 link %d and %d; want the other threads' links to cost nothing",
			manyProject, manyArchive, fewProject, fewArchive)
	}
}
