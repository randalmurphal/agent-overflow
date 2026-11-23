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

// A link write refuses a thread other thread mutations refuse, and the
// write to every link of a ticket skips it; a cancelled move gives the
// thread back to its writers.
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
			s := newTestStore(t)
			mustCreateThread(t, s, "a")
			mustSetThreadTicket(t, s, webLink("a"), true)
			c.state.enter(t, s, "a")

			seen, seenErr := s.RecordTicketSeen(webTicket, "APP-3", "Moved")
			other := ThreadTicket{ThreadID: "a", Ticket: TicketRef{Kind: TrackerKindLinear, Site: "workspace-a", IssueID: "issue-1"},
				IssueKey: "ENG-1", Origin: TicketOriginManual, LinkedAt: 30}
			_, setErr := s.SetThreadTicket(other)
			cleared, clearErr := s.ClearThreadTicket("a")
			if seenErr != nil {
				t.Fatalf("seen: %v", seenErr)
			}
			if c.refused == nil {
				if !slices.Equal(seen, []string{"a"}) || setErr != nil || clearErr != nil || !cleared {
					t.Fatalf("seen %v, set %v, clear %v %v; want every write to land", seen, setErr, cleared, clearErr)
				}
				return
			}
			if len(seen) != 0 || !c.refused(setErr) || !c.refused(clearErr) || cleared {
				t.Fatalf("seen %v, set %v, clear %v %v; want each refused", seen, setErr, cleared, clearErr)
			}
			if link, ok := storedTicket(t, s, "a"); !ok || link != webLink("a") {
				t.Fatalf("stored link = %+v, %v; want it untouched", link, ok)
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
			mustSetThreadTicket(t, s, webLink("a"), true)
			mustSetThreadTicket(t, s, webLink("b"), true)
			c.state.enter(t, s, "a")

			want := []ThreadTicket{webLink("b")}
			if c.owned {
				want = []ThreadTicket{webLink("a"), webLink("b")}
			}
			if links, err := s.ListThreadsForTicket(webTicket); err != nil || !slices.Equal(links, want) {
				t.Fatalf("threads of the ticket = %+v, %v; want %+v", links, err, want)
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
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	// A row no writer leaves on a holder, to prove every path skips one.
	mustExec(t, s.db, `INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES ('S','jira','https://example.atlassian.net','10001','WEB-1','','manual',1)`)

	if seen, err := s.RecordTicketSeen(webTicket, "APP-3", "Moved"); err != nil || len(seen) != 0 {
		t.Fatalf("seen = %v, %v; want the holder skipped", seen, err)
	}
	if links, err := s.ListThreadsForTicket(webTicket); err != nil || len(links) != 0 {
		t.Fatalf("threads of the ticket = %+v, %v", links, err)
	}
	if _, ok, err := s.GetThreadTicket("S"); err != nil || ok {
		t.Fatalf("get = %v, %v", ok, err)
	}
	if _, err := s.SetThreadTicket(webLink("S")); !errors.Is(err, ErrInvalidThreadTicket) {
		t.Fatalf("set on a holder: %v; want ErrInvalidThreadTicket", err)
	}
}
