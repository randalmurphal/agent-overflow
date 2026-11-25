package store

import (
	"errors"
	"testing"
)

// twoAccounts is a store with two Jira connections of one site and their
// mirror roots: low has the lower connection ID, so it is the site's link
// reader while it is on, and high lists boards 10, 11 and 12 with board
// 10's structure stored. low starts turned off, so high is the reader.
func twoAccounts(t *testing.T) (s *Store, low, high TrackerConnection, lowRoot, highRoot int64) {
	t.Helper()
	s = newTestStore(t)
	low = mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 10))
	high = mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user2@example.com", 20))
	if high.ID < low.ID {
		low, high = high, low
	}
	var err error
	if lowRoot, _, err = s.GetOrCreateMirrorRoot(low.ID); err != nil {
		t.Fatal(err)
	}
	if highRoot, _, err = s.GetOrCreateMirrorRoot(high.ID); err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, high.ID, "10", "11", "12")
	mustReplaceStructure(t, s, highRoot, webStructure(), true)
	setConnectionEnabled(t, s, low.ID, false)
	return s, low, high, lowRoot, highRoot
}

func setConnectionEnabled(t *testing.T, s *Store, id string, enabled bool) {
	t.Helper()
	if _, _, err := s.SetTrackerConnectionEnabled(id, enabled); err != nil {
		t.Fatal(err)
	}
}

// threadLink is threadID's link as its row shows it.
func threadLink(t *testing.T, s *Store, threadID string) ThreadTicketLink {
	t.Helper()
	row, err := s.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TicketLink == nil {
		t.Fatalf("thread %s has no link", threadID)
	}
	return *row.TicketLink
}

// A board page whose row the board refuses (a later read removed the
// issue's row, so the earlier page may not add it) stores no card, so the
// link shows the stored card's title, and the page names no thread.
func TestARefusedBoardRowLeavesTheLinkAlone(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	on := webPage("10", MirrorRow{Card: titledCard("10001", 5, "Fix the login page"), OrderKey: "a"})
	on.ReadAt = nextMirrorRead(s, root)
	mustWriteRowPage(t, s, root, on, nil)

	early := nextMirrorRead(s, root)
	removed := webPage("10")
	removed.Missing = []string{"10001"}
	removed.ReadAt = mirrorReadOf(root, early.rootOrder+1)
	if got := mustWriteRowPage(t, s, root, removed, nil); len(got.Removed) != 1 {
		t.Fatalf("the later read removed %v; want the row", got.Removed)
	}
	late := webPage("10", MirrorRow{Card: titledCard("10001", 9, "A title the board refused"), OrderKey: "a"})
	late.ReadAt = early
	got := mustWriteRowPage(t, s, root, late, nil)
	if len(got.Refused) != 1 || len(got.Cards) != 0 {
		t.Fatalf("the earlier page = %+v; want its row refused and no card stored", got)
	}
	if len(got.Threads) != 0 {
		t.Fatalf("the refused page named %v", got.Threads)
	}
	if link := threadLink(t, s, "a"); link.Title != "Fix the login page" || link.State != TicketLinkCurrent {
		t.Fatalf("link after the refused page = %+v; want the stored title, current", link)
	}
}

// Account high reads a linked board issue. Lower-ID low is turned on, and
// its first read fails; high's board stores the issue's new key and
// status meanwhile. When low is turned off again, the link shows what
// high's mirror holds, not what it held before low was on.
func TestAReaderChangeShowsTheReadersMirror(t *testing.T) {
	t.Parallel()
	s, low, high, _, highRoot := twoAccounts(t)
	mustCreateThread(t, s, "a")
	open := titledCard("10001", 5, "Fix the login page")
	mustSetThreadTicket(t, s, readLink(s, highRoot, "a", open))
	on := webPage("10", MirrorRow{Card: open, OrderKey: "a"})
	on.ReadAt = nextMirrorRead(s, highRoot)
	mustWriteRowPage(t, s, highRoot, on, nil)

	setConnectionEnabled(t, s, low.ID, true)
	closed := titledCard("10001", 6, "Fix the login page")
	closed.Key, closed.StatusID = "WEB-2", "10001"
	moved := webPage("10", MirrorRow{Card: closed, OrderKey: "a"})
	moved.ReadAt = nextMirrorRead(s, highRoot)
	mustWriteRowPage(t, s, highRoot, moved, nil)
	setConnectionEnabled(t, s, low.ID, false)

	link := threadLink(t, s, "a")
	if link.Key != "WEB-2" || link.StatusID != "10001" || link.StatusName != "Done" || link.State != TicketLinkCurrent || link.ConnectionID != high.ID {
		t.Fatalf("link after the switch back = %+v; want WEB-2, Done, current through %s", link, high.ID)
	}
}

// Account high reads a linked issue no board shows. Lower-ID low is
// turned on and fails to read it; the cleanup sweep removes high's card,
// which no link keeps while low is the reader. When low is turned off
// again, high's mirror holds no card, so the link is not checked.
func TestASwitchBackAfterTheSweepIsNotChecked(t *testing.T) {
	t.Parallel()
	s, low, high, _, highRoot := twoAccounts(t)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, highRoot, "a", titledCard("10001", 5, "Fix the login page")))

	setConnectionEnabled(t, s, low.ID, true)
	if err := s.sweepMirrorRoot(highRoot, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if cards := mustMirrorCards(t, s, highRoot, "10001"); len(cards) != 0 {
		t.Fatalf("the sweep left %+v", cards)
	}
	setConnectionEnabled(t, s, low.ID, false)

	link := threadLink(t, s, "a")
	if link.State != TicketLinkNotChecked || link.ConnectionID != high.ID || link.Title != "Fix the login page" {
		t.Fatalf("link after the switch back = %+v; want the last-seen title, not checked through %s", link, high.ID)
	}
}

// A relink's card read begins while high is the reader; lower-ID low is
// turned on before the link is written. high's mirror no longer keeps the
// new issue, so the card is not stored, the write is refused, and the
// thread keeps the link it had.
func TestARelinkAfterTheReaderChangedIsRefused(t *testing.T) {
	t.Parallel()
	s, low, _, _, highRoot := twoAccounts(t)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, highRoot, "a", titledCard("10001", 5, "Fix the login page")))

	w := readLink(s, highRoot, "a", titledCard("10002", 5, "Another ticket"))
	setConnectionEnabled(t, s, low.ID, true)
	if _, err := s.SetThreadTicket(w); !errors.Is(err, ErrThreadTicketNotReader) {
		t.Fatalf("a link read through a connection that stopped being the reader = %v; want ErrThreadTicketNotReader", err)
	}
	if link := mustGetThreadTicket(t, s, "a"); link.Ticket.IssueID != "10001" {
		t.Fatalf("link after the refused relink = %+v; want 10001 still", link)
	}
	if cards := mustMirrorCards(t, s, highRoot, "10002"); len(cards) != 0 {
		t.Fatalf("the refused relink stored %+v", cards)
	}
}

// A read that did not return a linked issue makes the link not found; a
// relink from the card the mirror still holds, older than that read,
// leaves it not found.
func TestACachedRelinkKeepsAKnownMissingIssueNotFound(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustCreateThread(t, s, "a")
	mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
	if _, err := s.MarkMirrorIssuesMissing(root, nextMirrorRead(s, root), []string{"10001"}); err != nil {
		t.Fatal(err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkNotFound {
		t.Fatalf("state after the missing read = %q", state)
	}
	if held := mustMirrorCards(t, s, root, "10001"); len(held) != 1 {
		t.Fatalf("the mirror holds %+v", held)
	}
	if _, err := s.SetThreadTicket(heldLink(root, "a", "10001")); !errors.Is(err, ErrThreadTicketNotHeld) {
		t.Fatalf("the cached relink = %v; want ErrThreadTicketNotHeld", err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkNotFound {
		t.Fatalf("state after the cached relink = %q; want not found", state)
	}
}

// A read that misses a linked issue makes its link not found. When the
// tracker returns the same card again and the same thread is set to the
// same ticket, the stored link is unchanged but its row is current again:
// the set reports the change, which its caller announces, and names no
// other thread.
func TestSettingARecoveredTicketAgainReportsTheRowChanged(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustCreateThread(t, s, "a")
	card := titledCard("10001", 5, "Fix the login page")
	mustSetThreadTicket(t, s, readLink(s, root, "a", card))
	stored := mustGetThreadTicket(t, s, "a")
	if _, err := s.MarkMirrorIssuesMissing(root, nextMirrorRead(s, root), []string{"10001"}); err != nil {
		t.Fatal(err)
	}
	if state := linkState(t, s, "a"); state != TicketLinkNotFound {
		t.Fatalf("state after the missing read = %q", state)
	}
	set := mustSetThreadTicket(t, s, readLink(s, root, "a", card))
	if got := mustGetThreadTicket(t, s, "a"); got != stored {
		t.Fatalf("stored link after the same card again = %+v; want %+v", got, stored)
	}
	if state := linkState(t, s, "a"); state != TicketLinkCurrent {
		t.Fatalf("state after the same card again = %q; want current", state)
	}
	if !set.Changed || set.Replaced != nil || len(set.Cards.Threads) != 0 {
		t.Fatalf("the set that made the row current = %+v; want the row changed and no other thread named", set)
	}
}
