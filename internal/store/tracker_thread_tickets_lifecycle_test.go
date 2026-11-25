package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// ticketThreadState puts thread id into one transfer or delete state.
type ticketThreadState struct {
	name  string
	enter func(t *testing.T, s *Store, id string)
}

func outgoingMove(phase string) ticketThreadState {
	return ticketThreadState{"outgoing move " + phase, func(t *testing.T, s *Store, id string) {
		seedTransfer(t, s, id, "outgoing", "move", phase, 0)
	}}
}

var (
	claimedDelete = ticketThreadState{"claimed delete", func(t *testing.T, s *Store, id string) {
		if _, err := s.ClaimDeleteFamily("op", []string{id}); err != nil {
			t.Fatal(err)
		}
	}}
	committedDelete = ticketThreadState{"committed delete", func(t *testing.T, s *Store, id string) {
		if err := s.BeginThreadDelete(id); err != nil {
			t.Fatal(err)
		}
	}}
)

// storedTicket reads a thread's link row whoever owns the thread.
func storedTicket(t *testing.T, s *Store, threadID string) (ThreadTicket, bool) {
	t.Helper()
	link, err := scanThreadTicket(s.db.QueryRow(`SELECT `+threadTicketColumns+` FROM thread_tickets WHERE thread_id = ?`, threadID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadTicket{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return link, true
}

func threadIDs(rows []Thread) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// A link write refuses a thread other thread mutations refuse, and a card
// write's copy to the links of its ticket skips it; a cancelled move gives
// the thread back to its writers.
func TestThreadTicketWritesFollowTheThreadLifecycle(t *testing.T) {
	t.Parallel()
	isTransfer := func(err error) bool {
		var transfer *ThreadTransferError
		return errors.As(err, &transfer)
	}
	isDeleting := func(err error) bool { return errors.Is(err, ErrThreadDeleting) }
	for _, c := range []struct {
		state   ticketThreadState
		refused func(error) bool
	}{
		{outgoingMove("preparing"), isTransfer},
		{outgoingMove("committed"), isTransfer},
		{outgoingMove("complete"), isTransfer},
		{outgoingMove("canceled"), nil},
		{claimedDelete, isDeleting},
		{committedDelete, isDeleting},
	} {
		t.Run(c.state.name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustCreateThread(t, s, "a")
			mustSetThreadTicket(t, s, readLink(s, root, "a", titledCard("10001", 5, "Fix the login page")))
			// Another thread keeps the card whatever state a is in.
			linkIssue(t, s, "keeper", "10001")
			linked := mustGetThreadTicket(t, s, "a")
			c.state.enter(t, s, "a")

			moved, copyErr := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{titledCard("10001", 6, "Moved")}, MirrorCatalog{}, nextMirrorRead(s, root))
			_, setErr := s.SetThreadTicket(readLink(s, root, "a", titledCard("10002", 5, "Another")))
			_, cleared, clearErr := s.ClearThreadTicket("a")
			if copyErr != nil {
				t.Fatalf("card write: %v", copyErr)
			}
			if c.refused == nil {
				if !slices.Equal(moved.Threads, []string{"a", "keeper"}) || setErr != nil || clearErr != nil || !cleared {
					t.Fatalf("copy %v, set %v, clear %v %v; want every write to land", moved.Threads, setErr, cleared, clearErr)
				}
				return
			}
			if !slices.Equal(moved.Threads, []string{"keeper"}) || !c.refused(setErr) || !c.refused(clearErr) || cleared {
				t.Fatalf("copy %v, set %v, clear %v %v; want each refused", moved.Threads, setErr, cleared, clearErr)
			}
			if link, ok := storedTicket(t, s, "a"); !ok || link != linked {
				t.Fatalf("stored link = %+v, %v; want it untouched %+v", link, ok, linked)
			}
		})
	}
}

// The link reads keep the threads this computer owns (owned_threads): a
// thread given away, or arriving and not yet here, reads as unlinked.
func TestThreadTicketReadsKeepOwnedThreads(t *testing.T) {
	t.Parallel()
	incomingMove := ticketThreadState{"incoming move preparing", func(t *testing.T, s *Store, id string) {
		seedTransfer(t, s, id, "incoming", "move", "preparing", 0)
	}}
	outgoingCopy := ticketThreadState{"outgoing copy complete", func(t *testing.T, s *Store, id string) {
		seedTransfer(t, s, id, "outgoing", "copy", "complete", 128)
	}}
	for _, c := range []struct {
		state ticketThreadState
		owned bool
	}{
		{outgoingMove("preparing"), true},
		{outgoingMove("committed"), false},
		{outgoingMove("complete"), false},
		{outgoingMove("canceled"), true},
		{outgoingCopy, true},
		{incomingMove, false},
		{claimedDelete, true},
		{committedDelete, false},
	} {
		t.Run(c.state.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			mustCreateThread(t, s, "a")
			mustCreateThread(t, s, "b")
			mustExec(t, s.db, `UPDATE threads SET updated_at = 1 WHERE id = 'a'`)
			mustExec(t, s.db, `UPDATE threads SET updated_at = 2 WHERE id = 'b'`)
			seedThreadTicket(t, s, webLink("a"))
			seedThreadTicket(t, s, webLink("b"))
			c.state.enter(t, s, "a")

			want := []string{"b"}
			if c.owned {
				want = []string{"b", "a"}
			}
			if rows, err := s.ListThreadsForTicket(webTicket); err != nil || !slices.Equal(threadIDs(rows), want) {
				t.Fatalf("threads of the ticket = %v, %v; want %v", threadIDs(rows), err, want)
			}
			if _, ok, err := s.GetThreadTicket("a"); err != nil || ok != c.owned {
				t.Fatalf("get = %v, %v; want %v", ok, err, c.owned)
			}
			byThread, err := s.ListThreadTickets([]string{"a", "b"})
			if _, ok := byThread["a"]; err != nil || ok != c.owned || len(byThread) != len(want) {
				t.Fatalf("batch read = %+v, %v; want a %v", byThread, err, c.owned)
			}
		})
	}
}

// A holder has no link a read shows or a write reaches, even a row left on
// it.
func TestThreadTicketSkipsHolders(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	seedLinearSource(t, s, "S", 1)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	// A row no writer leaves on a holder, to prove every path skips one.
	seedThreadTicket(t, s, webLink("S"))
	// A live thread keeps the card, so the card write reaches the links.
	linkIssue(t, s, "live", "10001")

	moved, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{titledCard("10001", 5, "Moved")}, MirrorCatalog{}, nextMirrorRead(s, root))
	if err != nil || !slices.Equal(moved.Threads, []string{"live"}) {
		t.Fatalf("card write reached %v, %v; want only the live thread", moved.Threads, err)
	}
	if got, _ := storedTicket(t, s, "S"); got != webLink("S") {
		t.Fatalf("holder row = %+v; want it untouched", got)
	}
	if rows, err := s.ListThreadsForTicket(webTicket); err != nil || !slices.Equal(threadIDs(rows), []string{"live"}) {
		t.Fatalf("threads of the ticket = %v, %v", threadIDs(rows), err)
	}
	if _, ok, err := s.GetThreadTicket("S"); err != nil || ok {
		t.Fatalf("get = %v, %v", ok, err)
	}
	if got, err := s.ListThreadsLinkedToSite(TrackerKindJira, mirrorSite); err != nil || !slices.Equal(got, []string{"live"}) {
		t.Fatalf("threads of the site = %v, %v", got, err)
	}
	if _, err := s.SetThreadTicket(readLink(s, root, "S", titledCard("10001", 5, "Moved"))); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("set on a holder: %v; want ErrInvalidThreadTicket", err)
	}
}
