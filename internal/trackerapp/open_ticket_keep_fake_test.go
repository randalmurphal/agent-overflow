package trackerapp

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// The open-ticket keep against the fake trackers: the mirror keeps the
// issue of every open ticket for as long as its pump lives (keepTicket,
// store mirrorIssueKept), whatever brought the person to it, and stops
// keeping it when the last subscription ends; the cleanup then removes
// what nothing else keeps.

// keepSite is the Jira site of the keep tests: WEB's board, an epic on no
// scrum board that WEB-1 names as its parent (10104), an issue of a
// project no board shows (OPS-1, 10105), and a WEB issue done before the
// history window (10106), which no board read lists.
func keepSite(at time.Time) trackerfake.JiraSite {
	site := jiraSeed(at)
	day := at.Add(-48 * time.Hour)
	old := at.Add(-HistoryWindow - 24*time.Hour)
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	site.Issues[0].Parent = "10104"
	site.Issues = append(site.Issues,
		trackerfake.JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10000", Summary: "The epic", Status: "10000", Created: day, Updated: day},
		trackerfake.JiraIssue{ID: "10105", Key: "OPS-1", Project: "OPS", Type: "10002", Summary: "An ops task", Status: "10000", Created: day, Updated: day},
		trackerfake.JiraIssue{ID: "10106", Key: "WEB-6", Project: "WEB", Type: "10002", Summary: "Done long ago", Status: "10002",
			Resolution: "Done", Resolved: old, Created: old.Add(-time.Hour), Updated: old},
	)
	return site
}

// keepScenario is a fake Jira site of keepSite with its connection and
// WEB's board listed, the service on the sync intervals cad.
func keepScenario(t *testing.T, cad Cadence) (*fakeScenario, Connection, Scope) {
	t.Helper()
	s := newFakeScenarioWith(t, cad, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{keepSite(at)}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	return s, conn, scope
}

// kept reports whether the mirror of conn keeps issue: its card is read
// (Issue), and, with doc, its document is stored. A connection with no
// mirror root yet keeps nothing.
func (s *fakeScenario) kept(t *testing.T, conn Connection, issue tracker.IssueID) (card, doc bool) {
	t.Helper()
	root, ok, err := s.st.GetMirrorRoot(conn.ID)
	switch {
	case err != nil:
		t.Fatal(err)
	case !ok:
		return false, false
	}
	_, err = s.svc.Issue(conn.ID, issue)
	switch {
	case errors.Is(err, ErrNotMirrored):
	case err != nil:
		t.Fatal(err)
	default:
		card = true
	}
	d, ok, err := s.st.GetMirrorDetail(root, string(issue))
	if err != nil {
		t.Fatal(err)
	}
	return card, ok && len(d.Data) > 0
}

// openTicket subscribes to issue's ticket and waits for its first read.
func (s *fakeScenario) openTicket(t *testing.T, conn Connection, issue tracker.IssueID) TicketSubscription {
	t.Helper()
	sub := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, issue))
	settled(t, s.svc)
	return sub
}

// closeTicket releases sub, issue's last subscription, and waits for its
// pump's goroutine to return.
func (s *fakeScenario) closeTicket(t *testing.T, conn Connection, sub TicketSubscription, issue tracker.IssueID) {
	t.Helper()
	tp := s.svc.ticketPumpOf(conn.ID, issue)
	if tp == nil {
		t.Fatalf("no pump of %s", issue)
	}
	s.svc.ReleaseSubscription(sub.ID)
	exited(t, tp)
}

// exited waits for tp's goroutine to return.
func exited(t *testing.T, tp *ticketPump) {
	t.Helper()
	select {
	case <-tp.exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("the pump of %s did not return", tp.key.issue)
	}
}

// pumpHolds reports whether issue's pump holds its document.
func (s *fakeScenario) pumpHolds(conn Connection, issue tracker.IssueID) bool {
	tp := s.svc.ticketPumpOf(conn.ID, issue)
	s.svc.subMu.Lock()
	defer s.svc.subMu.Unlock()
	return tp.held != nil
}

// openTicketRows is how many open-ticket rows the mirror holds, all of
// them deleted to count them: the test's last word on the table.
func (s *fakeScenario) openTicketRows(t *testing.T) int64 {
	t.Helper()
	// No service has session -1 (Service.session is never negative).
	n, err := s.st.DropMirrorTicketsOfOtherSessions(-1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Every way a person comes to a ticket whose issue no board or link
// keeps: the mirror keeps the issue while the ticket is open, its
// document and card stored as any kept issue's and its pump holding
// nothing, and closing the ticket ends the keep, so the cleanup removes
// them. An issue a board also keeps stays kept.
func TestAnOpenTicketsIssueIsKeptWhateverBroughtItThere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		issue tracker.IssueID
		// before runs before the ticket opens; after once it is open.
		before, after func(t *testing.T, s *fakeScenario, conn Connection, scope Scope)
		// keptBefore is an issue the board keeps before the ticket opens,
		// alsoKept one it keeps after the ticket closed too.
		keptBefore, alsoKept bool
	}{
		{name: "a search result on no board", issue: "10105", before: synced},
		{name: "a parent opened from a ticket", issue: "10104", before: func(t *testing.T, s *fakeScenario, conn Connection, scope Scope) {
			synced(t, s, conn, scope)
			from := s.openTicket(t, conn, "10101")
			if v := must[TicketView](t)(s.svc.TicketDetail(from.ID)); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.ID != "10104" {
				t.Fatalf("the ticket names its parent %+v", v.Detail)
			}
		}},
		{name: "a done issue older than the window", issue: "10106", before: synced},
		{name: "an issue on a board not read yet", issue: "10101", before: func(*testing.T, *fakeScenario, Connection, Scope) {}},
		{name: "an issue a reconcile dropped from its board", issue: "10102", keptBefore: true, before: synced, after: func(t *testing.T, s *fakeScenario, conn Connection, scope Scope) {
			world := s.e.Snapshot()
			world.Jira[0].Token = "fake-jira-token-1"
			for i := range world.Jira[0].Issues {
				if is := &world.Jira[0].Issues[i]; is.ID == "10102" {
					is.Project, is.Key = "OPS", "OPS-2"
				}
			}
			if err := s.e.Seed(world); err != nil {
				t.Fatal(err)
			}
			must[Outcome](t)(s.svc.tickReconcile(t.Context(), scope, Background))
			if rows := s.boardRowIDs(t, scope); slices.Contains(rows, "10102") {
				t.Fatalf("the reconcile left 10102 on the board: %v", rows)
			}
		}},
		{name: "an issue its board keeps too", issue: "10101", before: synced, keptBefore: true, alsoKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			tc.before(t, s, conn, scope)
			if card, _ := s.kept(t, conn, tc.issue); card != tc.keptBefore {
				t.Fatalf("before the ticket opens the mirror keeps %s: %v", tc.issue, card)
			}
			sub := s.openTicket(t, conn, tc.issue)
			if tc.after != nil {
				tc.after(t, s, conn, scope)
				settled(t, s.svc)
			}
			if card, doc := s.kept(t, conn, tc.issue); !card || !doc {
				t.Fatalf("while open the mirror keeps the card %v and the document %v", card, doc)
			}
			if s.pumpHolds(conn, tc.issue) {
				t.Fatal("the pump holds the document the mirror keeps")
			}
			if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); !v.Loaded || v.Detail.Card.ID != string(tc.issue) {
				t.Fatalf("the open ticket shows %+v", v)
			}
			s.closeTicket(t, conn, sub, tc.issue)
			if err := s.svc.FinishMirrorCleanup(); err != nil {
				t.Fatal(err)
			}
			if card, doc := s.kept(t, conn, tc.issue); card != tc.alsoKept || doc != tc.alsoKept {
				t.Fatalf("after the ticket closed the mirror keeps the card %v and the document %v; want %v", card, doc, tc.alsoKept)
			}
		})
	}
}

// synced reads scope's board once, every unit.
func synced(t *testing.T, s *fakeScenario, _ Connection, scope Scope) {
	t.Helper()
	s.syncBoard(t, scope)
}

// An issue the mirror keeps for its ticket alone has its history read by
// the ticket's round: one never moved shows the time it entered its
// status, its creation, from the round after the opening, which no
// document can show (its history holds no status change).
func TestAnOpenTicketShowsItsStatusTimeWithinARound(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: 100 * time.Millisecond})
	s.syncBoard(t, scope)
	created := must[[]tracker.Detail](t)(detailsOf(t, s, conn, "10105"))[0].Card.Created
	sub := s.openTicket(t, conn, "10105")
	if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); v.Detail.Card.StatusSince != 0 {
		t.Fatalf("the opening shows since %d before any history read", v.Detail.Card.StatusSince)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		v := must[TicketView](t)(s.svc.TicketDetail(sub.ID))
		if v.Detail.Card.StatusSince == millis(created) {
			break
		}
		if v.Detail.Card.StatusSince != 0 || time.Now().After(deadline) {
			t.Fatalf("the ticket shows since %d; want its creation %d", v.Detail.Card.StatusSince, millis(created))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := must[IssueView](t)(s.svc.Issue(conn.ID, "10105")).Card; c.StatusSince != millis(created) {
		t.Fatalf("the mirror's card shows since %d", c.StatusSince)
	}
}

// detailsOf reads issues' documents through the connection's provider.
func detailsOf(t *testing.T, s *fakeScenario, conn Connection, issues ...tracker.IssueID) ([]tracker.Detail, error) {
	t.Helper()
	d, err := s.provider(conn.ID).Details(t.Context(), issues)
	return d.Details, err
}

// The ends of a pump other than its release each delete its row: the
// connection turned off, the connection removed, and the service
// stopped. None leaves an open-ticket row.
func TestEveryEndOfAnOpenTicketDeletesItsKeep(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(t *testing.T, s *fakeScenario, conn Connection){
		"turned off": func(t *testing.T, s *fakeScenario, conn Connection) {
			must[Connection](t)(s.svc.SetEnabled(conn.ID, false))
		},
		"removed": func(t *testing.T, s *fakeScenario, conn Connection) {
			if err := s.svc.Remove(conn.ID); err != nil {
				t.Fatal(err)
			}
		},
		"stopped": func(_ *testing.T, s *fakeScenario, _ Connection) { s.svc.Stop() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			s.openTicket(t, conn, "10105")
			tp := s.svc.ticketPumpOf(conn.ID, "10105")
			end(t, s, conn)
			exited(t, tp)
			if n := s.openTicketRows(t); n != 0 {
				t.Fatalf("%d open-ticket rows after the pump ended", n)
			}
		})
	}
}

// A restore empties the mirror, open tickets included, while their pumps
// live. The run that finds the root gone makes another (rehold), which
// keeps the open tickets again; a ticket opened before any run did keeps
// itself under the new root (keepTicket). Either way the next read stores
// the ticket's document and card as a kept issue's.
func TestARestoreKeepsTheOpenTicketsAgain(t *testing.T) {
	t.Parallel()
	for name, openedAfter := range map[string]bool{"a read finds it": false, "a ticket opened after it": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			open := []tracker.IssueID{"10105"}
			subs := []TicketSubscription{s.openTicket(t, conn, "10105")}
			snap := filepath.Join(t.TempDir(), "db.snapshot")
			if err := s.st.SnapshotTo(snap); err != nil {
				t.Fatal(err)
			}
			if _, err := s.st.RestoreFrom(snap); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.st.GetMirrorRoot(conn.ID); err != nil || ok {
				t.Fatalf("the restore left the mirror root: %v, %v", ok, err)
			}
			if openedAfter {
				open = append(open, "10104")
				subs = append(subs, s.openTicket(t, conn, "10104"))
			}
			rd := s.svc.roundOf(conn.ID)
			if rd == nil {
				t.Fatal("no ticket round runs")
			}
			if res := s.svc.roundTick(rd); res.failed {
				t.Fatalf("the round after the restore %+v", res)
			}
			settled(t, s.svc)
			for i, issue := range open {
				if err := s.svc.RefreshSubscription(t.Context(), subs[i].ID); err != nil {
					t.Fatal(err)
				}
				if card, doc := s.kept(t, conn, issue); !card || !doc {
					t.Fatalf("after the restore the mirror keeps %s's card %v and document %v", issue, card, doc)
				}
				if s.pumpHolds(conn, issue) {
					t.Fatalf("the pump of %s holds its document after the restore", issue)
				}
			}
			if n := s.openTicketRows(t); n != int64(len(open)) {
				t.Fatalf("%d open-ticket rows after the restore; want the tickets' %d", n, len(open))
			}
		})
	}
}

// The rows a process left when it ended without deleting them keep their
// issues until the next boot's StartLinkedSync deletes them; the cleanup
// then removes what nothing else keeps.
func TestTheBootDeletesTheOpenTicketsAnEarlierProcessLeft(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	sub := s.openTicket(t, conn, "10105")
	root := must[int64](t)(rootOf(s, conn))
	// An earlier process's row of the same issue.
	if err := s.st.KeepMirrorTicket(root, "10105", s.svc.session+1, 1); err != nil {
		t.Fatal(err)
	}
	s.closeTicket(t, conn, sub, "10105")
	if err := s.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if card, _ := s.kept(t, conn, "10105"); !card {
		t.Fatal("the earlier process's row kept nothing")
	}
	if err := s.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if card, doc := s.kept(t, conn, "10105"); card || doc {
		t.Fatalf("after the boot the mirror keeps the card %v and the document %v", card, doc)
	}
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after the boot", n)
	}
}

func rootOf(s *fakeScenario, conn Connection) (int64, error) {
	root, ok, err := s.st.GetMirrorRoot(conn.ID)
	if err == nil && !ok {
		err = errors.New("no mirror root")
	}
	return root, err
}

// Opening a ticket adds one store call to its pump's start, the keep, and
// closing it costs one, the drop; each is one statement and one row
// (store TestOpenTicketKeepCosts). A ticket of an issue its board keeps
// costs the same: the keep does not ask what else keeps the issue.
func TestOpeningAndClosingATicketKeepCosts(t *testing.T) {
	t.Parallel()
	for _, issue := range []tracker.IssueID{"10105", "10101"} {
		t.Run(string(issue), func(t *testing.T) {
			t.Parallel()
			s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			s.syncBoard(t, scope)
			cs := newCountingStore(s.st)
			s.svc.store = cs
			sub := s.openTicket(t, conn, issue)
			if n := cs.take()["KeepMirrorTicket"]; n != 1 {
				t.Fatalf("the opening kept the issue %d times; want 1", n)
			}
			s.closeTicket(t, conn, sub, issue)
			if got := describeCounts(cs.take()); got != "DropMirrorTicket:1" {
				t.Fatalf("the close cost %s; want DropMirrorTicket:1", got)
			}
		})
	}
}

// A round reads the connection's due histories only for a ticket that
// shows no time in its status (roundRead): with an issue due a history
// read, a round whose ticket shows its time from its document wants none,
// and one with a ticket that shows none wants one.
func TestARoundReadsHistoryOnlyForATicketWithoutItsStatusTime(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	// 10101 moved out of its status and back, a settle margin ago: its
	// document's history settles the time, and its card read since makes
	// its history due.
	statusSinceTracker{issue: "10101", status: "10001", other: "10002"}.leaveAndReturn(t, s, conn)
	s.clock.advance(time.Minute + time.Second)
	sub := s.openTicket(t, conn, "10101")
	if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); v.Detail.Card.StatusSince == 0 {
		t.Fatal("the ticket shows no time in its status from its document")
	}
	rd := s.svc.roundOf(conn.ID)
	if res := s.svc.roundTick(rd); !res.historyDue || res.historyWanted {
		t.Fatalf("a round of a ticket that shows its time: history due %v, wanted %v; want due and not wanted", res.historyDue, res.historyWanted)
	}
	s.openTicket(t, conn, "10105")
	if res := s.svc.roundTick(rd); !res.historyDue || !res.historyWanted {
		t.Fatalf("a round with a ticket that shows no time: history due %v, wanted %v; want both", res.historyDue, res.historyWanted)
	}
}
