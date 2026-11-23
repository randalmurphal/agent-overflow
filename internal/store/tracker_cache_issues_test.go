package store

import (
	"errors"
	"reflect"
	"testing"
)

// linkIssue links a new thread to a WEB issue on the mirror's site.
func linkIssue(t *testing.T, s *Store, threadID, issueID string) {
	t.Helper()
	mustCreateThread(t, s, threadID)
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: threadID, Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: issueID},
		IssueKey: "WEB-" + issueID, Origin: TicketOriginManual, LinkedAt: 10}, true)
}

func mustMirrorCards(t *testing.T, s *Store, root int64, ids ...string) []MirrorCard {
	t.Helper()
	cards, err := s.ListMirrorCards(root, ids)
	if err != nil {
		t.Fatal(err)
	}
	return cards
}

func assertMirrorChanges(t *testing.T, what string, got, want MirrorChanges) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: changes = %+v\nwant %+v", what, got, want)
	}
}

// A card older than the stored one is not stored and is reported, but its
// row on the board still is; a card of the same time with other content
// replaces it, and a status change makes the time the status began unknown.
func TestMirrorCardIsNeverReplacedByAnOlderOne(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	newer := webRow("10001", 10, "a", "21")
	newer.Card.StatusID = "3"
	mustWriteRowPage(t, s, root, webPage("10", newer), nil)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET status_since = 9`)

	older := webRow("10001", 5, "z")
	older.Card.StatusID = "1"
	page := webPage("10", older)
	page.ReadAt = mirrorReadAt(2)
	got := mustWriteRowPage(t, s, root, page, nil)
	assertMirrorChanges(t, "older card", got, MirrorChanges{Rows: []string{"10001"}, Superseded: []string{"10001"}})
	newer.Card.StatusSince = 9
	if cards := mustMirrorCards(t, s, root, "10001"); !reflect.DeepEqual(cards, []MirrorCard{newer.Card}) {
		t.Fatalf("card = %+v; want the newer one", cards)
	}
	if key := mustOrderKey(t, s, "10", "10001"); key != "z" {
		t.Fatalf("order key = %s", key)
	}
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{older.Card}, MirrorCatalog{}, mirrorReadAt(2)); err != nil ||
		!reflect.DeepEqual(changes, MirrorChanges{Superseded: []string{"10001"}}) {
		t.Fatalf("older card with no board = %+v, %v", changes, err)
	}

	same := newer
	same.Card.StatusID = "10001"
	page = webPage("10", same)
	page.ReadAt = mirrorReadAt(3)
	got = mustWriteRowPage(t, s, root, page, nil)
	assertMirrorChanges(t, "card of the same time", got, MirrorChanges{Cards: []string{"10001"}, Rows: []string{"10001"}})
	assertRows(t, s.db, `SELECT status_since FROM tracker_cache_issues`, "0")
}

// A thread this computer owns keeps the issue its ticket names, on this
// connection's site, after the issue leaves every board; a card with no
// board is stored only for a kept issue.
func TestMirrorKeepsIssuesLinkedToOwnedThreads(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a"), webRow("10002", 5, "b"), webRow("10003", 5, "c"), webRow("10004", 5, "d")), nil)
	linkIssue(t, s, "linked", "10001")
	linkIssue(t, s, "deleting", "10002")
	mustExec(t, s.db, `UPDATE threads SET deleting = 1 WHERE id = 'deleting'`)
	mustCreateThread(t, s, "other-kind")
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: "other-kind", Ticket: TicketRef{Kind: TrackerKindLinear, Site: mirrorSite, IssueID: "10004"},
		IssueKey: "APP-4", Origin: TicketOriginManual, LinkedAt: 10}, true)
	mustCreateThread(t, s, "elsewhere")
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: "elsewhere", Ticket: TicketRef{Kind: TrackerKindJira, Site: "https://other.example.com", IssueID: "10003"},
		IssueKey: "WEB-3", Origin: TicketOriginManual, LinkedAt: 10}, true)
	if linked, more, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10"}); err != nil || more || len(linked) != 0 {
		t.Fatalf("linked issues on a board = %v, %v, %v", linked, more, err)
	}

	if _, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, mirrorReadAt(5), 0); err != nil {
		t.Fatal(err)
	}
	if linked, more, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10"}); err != nil || more || !reflect.DeepEqual(linked, []string{"10001"}) {
		t.Fatalf("linked issues off the boards = %v, %v, %v", linked, more, err)
	}
	if cards := mustMirrorCards(t, s, root, "10001", "10002", "10003", "10004"); len(cards) != 1 || cards[0].IssueID != "10001" {
		t.Fatalf("cards after the reconcile = %+v; want only the linked 10001", cards)
	}
	changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 7), webCard("10009", 7)}, MirrorCatalog{}, mirrorReadAt(1))
	assertMirrorChanges(t, "linked cards", changes, MirrorChanges{Cards: []string{"10001"}, Unkept: []string{"10009"}})
	if err != nil {
		t.Fatal(err)
	}
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 7)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil || !reflect.DeepEqual(changes, MirrorChanges{}) {
		t.Fatalf("same card again = %+v, %v", changes, err)
	}
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 7), webCard("10001", 8)}, MirrorCatalog{}, mirrorReadAt(1)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("card twice: %v", err)
	}
}

// Two reads of a card with the same update time land in either order: the
// one that began later wins, for the card and its sprints together, on a
// board and off it. A project key rename changes every key and moves no
// update time.
func TestATiedCardFollowsItsReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	renamed := webRow("10001", 5, "a", "22")
	renamed.Card.Key = "APP-10001"
	page := webPage("10", renamed)
	page.ReadAt = mirrorReadAt(30)
	mustWriteRowPage(t, s, root, page, nil)
	former := webRow("10001", 5, "a", "21")
	page = webPage("10", former)
	page.ReadAt = mirrorReadAt(20)
	got := mustWriteRowPage(t, s, root, page, nil)
	if cards := mustMirrorCards(t, s, root, "10001"); len(cards) != 1 || cards[0].Key != "APP-10001" || !reflect.DeepEqual(cards[0].Sprints, []string{"22"}) {
		t.Fatalf("after a late row read the card is %+v; want the later read's key and sprints", cards)
	}
	assertMirrorChanges(t, "late row read", got, MirrorChanges{Superseded: []string{"10001"}})
	changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{former.Card}, MirrorCatalog{}, mirrorReadAt(25))
	if err != nil || !reflect.DeepEqual(changes, MirrorChanges{Superseded: []string{"10001"}}) {
		t.Fatalf("late card read = %+v, %v", changes, err)
	}
	if cards := mustMirrorCards(t, s, root, "10001"); cards[0].Key != "APP-10001" || !reflect.DeepEqual(cards[0].Sprints, []string{"22"}) {
		t.Fatalf("after a late card read the card is %+v; want the later read's key and sprints", cards)
	}
	changes, err = s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{former.Card}, MirrorCatalog{}, mirrorReadAt(40))
	if err != nil || !reflect.DeepEqual(changes, MirrorChanges{Cards: []string{"10001"}}) {
		t.Fatalf("a card read later = %+v, %v", changes, err)
	}
	if cards := mustMirrorCards(t, s, root, "10001"); cards[0].Key != "WEB-10001" || !reflect.DeepEqual(cards[0].Sprints, []string{"21"}) {
		t.Fatalf("after a later card read the card is %+v", cards)
	}
	// A read that changes the sprints alone is the card's latest change.
	moved := former
	moved.Card.Sprints = []string{"22"}
	page = webPage("10", moved)
	page.ReadAt = mirrorReadAt(50)
	assertMirrorChanges(t, "sprints moved", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page = webPage("10", former)
	page.ReadAt = mirrorReadAt(45)
	assertMirrorChanges(t, "late read of the sprints", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Superseded: []string{"10001"}})
	if cards := mustMirrorCards(t, s, root, "10001"); !reflect.DeepEqual(cards[0].Sprints, []string{"22"}) {
		t.Fatalf("after a late read of the sprints the card is %+v", cards)
	}
	// The same card from an earlier read loses nothing, so it reports
	// nothing.
	page = webPage("10", moved)
	page.ReadAt = mirrorReadAt(45)
	assertMirrorChanges(t, "identical earlier read", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{})
}
