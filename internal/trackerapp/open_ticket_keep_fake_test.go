package trackerapp

import (
	"context"
	"errors"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The open-ticket keep against the fake trackers: the mirror keeps the
// issue of every open ticket from the bind before its first read to the
// pump's end (bindTickets, dropTicket, store mirrorIssueKept), whatever
// brought the person to it, and stops
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
	d, ok, err := getMirrorBody(s.st, root, string(issue))
	if err != nil {
		t.Fatal(err)
	}
	return card, ok && len(d.Data) > 0
}

// openTicket subscribes to issue's ticket and waits for its first read.
func (s *fakeScenario) openTicket(t *testing.T, conn Connection, issue tracker.IssueID) TicketSubscription {
	t.Helper()
	sub := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, issue, untied))
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

// An issue the mirror keeps for its ticket alone that never moved shows
// the time it entered its status, its creation, from the opening's read:
// its history is complete from the creation and holds no status change,
// so the read settles the time, and the board's card shows it too.
func TestAnOpenTicketNeverMovedShowsItsCreationFromTheOpening(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	created := must[[]tracker.Detail](t)(detailsOf(t, s, conn, "10105"))[0].Card.Created
	s.clock.advance(time.Minute + time.Second)
	sub := s.openTicket(t, conn, "10105")
	if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); v.Detail.Card.StatusSince != millis(created) {
		t.Fatalf("the opening shows since %d; want its creation %d", v.Detail.Card.StatusSince, millis(created))
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
// keeps the open tickets again; a ticket opened before any run did binds
// itself under the new root (bindTickets). Either way the next read stores
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
			}
			if n := s.openTicketRows(t); n != int64(len(open)) {
				t.Fatalf("%d open-ticket rows after the restore; want the tickets' %d", n, len(open))
			}
		})
	}
}

// earlierRows writes n open-ticket rows of issue under conn's mirror root
// that a process before this one left (another session), and returns the
// root.
func (s *fakeScenario) earlierRows(t *testing.T, conn Connection, issue tracker.IssueID, n int) int64 {
	t.Helper()
	root, _, err := s.st.GetOrCreateMirrorRoot(conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	read := must[store.MirrorRead](t)(s.st.BeginMirrorRead(root, 1))
	for i := range n {
		if _, _, err := s.st.KeepMirrorTicket(root, string(issue), s.svc.session+1, int64(i+1), read); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// earlierDrops is a store that counts the deletes of an earlier process's
// open tickets, fails the first fail of them, and runs each through hold.
type earlierDrops struct {
	*store.Store
	mu    sync.Mutex
	calls int
	fail  int
	hold  func()
}

func (s *earlierDrops) DropMirrorTicketsOfOtherSessions(session int64) (int64, error) {
	s.mu.Lock()
	s.calls++
	failed := s.calls <= s.fail
	s.mu.Unlock()
	if s.hold != nil {
		s.hold()
	}
	if failed {
		return 0, errors.New("test: the store refused the delete")
	}
	return s.Store.DropMirrorTicketsOfOtherSessions(session)
}

func (s *earlierDrops) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// The rows a process left when it ended without deleting them are deleted
// by this process's first run, before any bind: a ticket opened at once
// keeps its own row, the earlier rows are gone, and the cleanup removes
// what nothing else keeps once the ticket closes.
func TestTheFirstRunDeletesTheOpenTicketsAnEarlierProcessLeft(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.earlierRows(t, conn, "10105", 2)
	sub := s.openTicket(t, conn, "10105")
	if n := must[int64](t)(s.st.DropMirrorTicketsOfOtherSessions(s.svc.session)); n != 0 {
		t.Fatalf("%d rows of the earlier process after the first run", n)
	}
	if card, doc := s.kept(t, conn, "10105"); !card || !doc {
		t.Fatalf("the open ticket's own row keeps the card %v and the body %v", card, doc)
	}
	s.closeTicket(t, conn, sub, "10105")
	if err := s.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if card, doc := s.kept(t, conn, "10105"); card || doc {
		t.Fatalf("after the close the mirror keeps the card %v and the body %v", card, doc)
	}
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after the close", n)
	}
}

// A run that no view read preceded, a detail refresh here, deletes the
// rows an earlier process left as it starts.
func TestARunBeforeAnyViewReadDeletesTheEarlierRows(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.earlierRows(t, conn, "10105", 2)
	if _, _, err := s.svc.issueDetail(t.Context(), conn.ID, "10105", Interactive, true); err != nil {
		t.Fatal(err)
	}
	if n := must[int64](t)(s.st.DropMirrorTicketsOfOtherSessions(s.svc.session)); n != 0 {
		t.Fatalf("%d rows of the earlier process after the first run", n)
	}
}

// A delete of the earlier rows that fails fails what tried it, a view
// read or a run: an opening, whose answer reads the mirror's copy, fails
// with it, and the next try deletes them and binds the ticket; a run
// whose delete fails is the ticket's failure, which its next read
// retries.
func TestAFailedDeleteOfTheEarlierRowsIsRetried(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	s.earlierRows(t, conn, "10105", 1)
	drops := &earlierDrops{Store: s.st, fail: 1}
	s.svc.store = drops
	if _, err := s.svc.SubscribeTicket(conn.ID, "10105", untied); err == nil || !strings.Contains(err.Error(), "the store refused the delete") {
		t.Fatalf("an opening whose delete failed: %v; want the delete's failure", err)
	}
	sub := s.openTicket(t, conn, "10105")
	if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); v.Failure != nil || !v.Loaded {
		t.Fatalf("after the retry the ticket shows %+v", v)
	}
	if n := drops.count(); n != 2 {
		t.Fatalf("the delete ran %d times; want the failure and the retry", n)
	}
	if n := must[int64](t)(s.st.DropMirrorTicketsOfOtherSessions(s.svc.session)); n != 0 {
		t.Fatalf("%d rows of the earlier process after the retry", n)
	}
	if n := s.openTicketRows(t); n != 1 {
		t.Fatalf("%d open-ticket rows; want the ticket's own", n)
	}
}

// Two first runs at once delete the earlier rows once: the second waits
// for the first's delete. While the first's delete is held, the second run
// is given time to reach a delete of its own, which it must not.
func TestConcurrentFirstRunsDeleteTheEarlierRowsOnce(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.earlierRows(t, conn, "10105", 1)
	arrived, second, release := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	drops := &earlierDrops{Store: s.st, hold: func() {
		first := false
		once.Do(func() { first = true; close(arrived) })
		if !first {
			second <- struct{}{}
		}
		<-release
	}}
	s.svc.store = drops
	done := make(chan error, 2)
	// Two reads of tickets, which take no lock of a scope.
	run := func(issue tracker.IssueID) {
		_, _, err := s.svc.IssueDetail(context.Background(), conn.ID, issue, Background)
		done <- err
	}
	go run("10105")
	<-arrived
	go run("10104")
	select {
	case <-second:
		close(release)
		t.Fatal("the second first run deleted the earlier rows while the first's delete ran")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := drops.count(); n != 1 {
		t.Fatalf("the delete ran %d times; want once", n)
	}
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after the first runs", n)
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
			// The opening paints from the record (no body yet), then its
			// first read stores the ticket and builds its view from the
			// record once, as it settles; 10101's view shows other issues,
			// whose cards it reads.
			quiet(t, s.svc)
			want := "GetMirrorCatalog:1 GetMirrorRoot:1 GetMirrorSync:1 KeepMirrorTicket:1 ListMirrorBodies:2 StoreMirrorTickets:1"
			if issue == "10101" {
				want = "GetMirrorCatalog:1 GetMirrorRoot:1 GetMirrorSync:1 KeepMirrorTicket:1 ListMirrorBodies:2 ListMirrorCards:1 StoreMirrorTickets:1"
			}
			if got := describeCounts(cs.take()); got != want {
				t.Fatalf("the opening cost %s; want %s", got, want)
			}
			s.closeTicket(t, conn, sub, issue)
			if got := describeCounts(cs.take()); got != "DropMirrorTicket:1" {
				t.Fatalf("the close cost %s; want DropMirrorTicket:1", got)
			}
		})
	}
}

// A round reads the connection's due histories only for a shown ticket
// whose record's status time no final read decided (roundRead): with an
// issue due a history read, a round whose ticket's read settled its time
// wants none, and one with a ticket edited inside the settle margin, whose
// read cannot settle it, wants one.
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
	s.editTitles(t, conn.ID, []tracker.IssueID{"10105"})
	s.openTicket(t, conn, "10105")
	if res := s.svc.roundTick(rd); !res.historyDue || !res.historyWanted {
		t.Fatalf("a round with a ticket read inside the margin: history due %v, wanted %v; want both", res.historyDue, res.historyWanted)
	}
}

// childShowsParentKey waits until the open child's ticket shows its
// parent 10104 with key, after the events told before.
func (s *fakeScenario) childShowsParentKey(t *testing.T, child TicketSubscription, told int, key string) {
	t.Helper()
	s.events.waitFor(t, "the child's change", func(_ []BoardEvent, i []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(i[told:], func(e IssueEvent) bool { return e.Issue == "10101" && e.Changed })
	})
	if v := must[TicketView](t)(s.svc.TicketDetail(child.ID)); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Key != key {
		t.Fatalf("the child shows its parent %+v; want the key %q", v.Detail, key)
	}
}

// Closing the ticket of a parent no board keeps and opening it again
// before the cleanup removed its record: the reopening's keep shows the
// record again with no write of it (its read finds the card it holds), and
// the open child's ticket, which showed the parent without its key after
// the close, is told and shows the key again.
func TestReopeningAParentsTicketRebuildsItsOpenChild(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	child := s.openTicket(t, conn, "10101")
	parent := s.openTicket(t, conn, "10104")
	quiet(t, s.svc)
	_, told, _ := s.events.snapshot()
	s.closeTicket(t, conn, parent, "10104")
	quiet(t, s.svc)
	s.childShowsParentKey(t, child, len(told), "")
	_, told, _ = s.events.snapshot()
	s.openTicket(t, conn, "10104")
	quiet(t, s.svc)
	s.childShowsParentKey(t, child, len(told), "WEB-4")
}

// dropsRefused is a store whose first refuse drops of an open ticket fail.
type dropsRefused struct {
	*store.Store
	mu     sync.Mutex
	refuse int
	calls  int
}

func (d *dropsRefused) DropMirrorTicket(session, pump int64) (bool, error) {
	d.mu.Lock()
	d.calls++
	refused := d.calls <= d.refuse
	d.mu.Unlock()
	if refused {
		return false, errors.New("test: the store refused the drop")
	}
	return d.Store.DropMirrorTicket(session, pump)
}

func (d *dropsRefused) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// A close whose drop fails stays owed: the store keeps the parent, so the
// open child still shows its key and nothing is told; the next run tries
// the drop again, and once it commits the child is told and shows the
// parent without the key, and the parent's row is gone.
func TestAFailedDropOfAClosedTicketIsRetried(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	child := s.openTicket(t, conn, "10101")
	parent := s.openTicket(t, conn, "10104")
	quiet(t, s.svc)
	drops := &dropsRefused{Store: s.st, refuse: 1}
	s.svc.store = drops
	s.closeTicket(t, conn, parent, "10104")
	quiet(t, s.svc)
	if card, _ := s.kept(t, conn, "10104"); !card {
		t.Fatal("the mirror stopped keeping the parent while its drop failed")
	}
	if v := must[TicketView](t)(s.svc.TicketDetail(child.ID)); v.Detail == nil || v.Detail.Card.Parent == nil || v.Detail.Card.Parent.Key != "WEB-4" {
		t.Fatalf("while the drop is owed the child shows its parent %+v; want WEB-4", v.Detail)
	}
	_, told, _ := s.events.snapshot()
	s.syncBoard(t, scope)
	quiet(t, s.svc)
	s.childShowsParentKey(t, child, len(told), "")
	if n := drops.count(); n != 2 {
		t.Fatalf("the drop ran %d times; want the failure and the retry", n)
	}
	if card, _ := s.kept(t, conn, "10104"); card {
		t.Fatal("after the retried drop the mirror keeps the parent")
	}
	if n := s.openTicketRows(t); n != 1 {
		t.Fatalf("%d open-ticket rows; want the child's", n)
	}
}

// A view read before this process's first run, with a row an earlier
// process left keeping an issue no board keeps: the read deletes the
// earlier rows first, so it does not show the issue, and no earlier row
// remains.
func TestAViewReadAtStartDoesNotShowAnIssueOnlyAnEarlierProcessKept(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.syncBoard(t, scope)
	sub := s.openTicket(t, conn, "10105")
	s.closeTicket(t, conn, sub, "10105")
	s.earlierRows(t, conn, "10105", 1)
	// This process as at its start: no run has deleted the earlier rows.
	s.svc.earlier.Lock()
	s.svc.earlier.done = false
	s.svc.earlier.Unlock()
	if _, err := s.svc.Issue(conn.ID, "10105"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("a view read at start of an issue only an earlier process kept: %v; want ErrNotMirrored", err)
	}
	if n := must[int64](t)(s.st.DropMirrorTicketsOfOtherSessions(s.svc.session)); n != 0 {
		t.Fatalf("%d rows of the earlier process after the view read", n)
	}
}

// beginsGated is a store whose next BeginMirrorRead made by a bind of open
// tickets (bindTickets), once armed, waits for release.
type beginsGated struct {
	*keepsMade
	mu      sync.Mutex
	armed   bool
	arrived chan struct{}
	release chan struct{}
}

func (g *beginsGated) BeginMirrorRead(root, now int64) (store.MirrorRead, error) {
	g.mu.Lock()
	hold := g.armed && strings.Contains(string(debug.Stack()), "bindTickets")
	if hold {
		g.armed = false
	}
	g.mu.Unlock()
	if hold {
		close(g.arrived)
		<-g.release
	}
	return g.keepsMade.BeginMirrorRead(root, now)
}

// A round that took the read of an open ticket whose bind failed, and
// binds it after the ticket closed and opened again: the closed pump is
// dropped under opMu before the round's bind takes opMu and finds it
// dropped, so the old pump writes no keep: closing the new pump, whose
// keep is the issue's one row, leaves none.
func TestAClosedPumpsKeepDoesNotOutliveAReopening(t *testing.T) {
	t.Parallel()
	s, conn, scope := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	s.syncBoard(t, scope)
	gate := &beginsGated{keepsMade: &keepsMade{Store: s.st, refuse: "10105"}, arrived: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate.release) }) })
	s.svc.store = gate
	old := s.openTicket(t, conn, "10105")
	if v := must[TicketView](t)(s.svc.TicketDetail(old.ID)); v.Failure == nil {
		t.Fatalf("the ticket whose keep failed shows %+v; want its failure", v)
	}
	oldPump := s.svc.ticketPumpOf(conn.ID, "10105")
	// The round reads the ticket its retry is due for, and binds it first.
	s.svc.subMu.Lock()
	oldPump.due = s.svc.now()
	s.svc.subMu.Unlock()
	gate.mu.Lock()
	gate.armed = true
	gate.mu.Unlock()
	rd := s.svc.roundOf(conn.ID)
	if rd == nil {
		t.Fatal("no ticket round runs")
	}
	done := make(chan ticketsResult, 1)
	go func() { done <- s.svc.roundTick(rd) }()
	waitClosed(t, "the round's bind", gate.arrived)
	s.closeTicket(t, conn, old, "10105")
	reopened := s.openTicket(t, conn, "10105")
	once.Do(func() { close(gate.release) })
	<-done
	quiet(t, s.svc)
	s.closeTicket(t, conn, reopened, "10105")
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after the reopened ticket closed", n)
	}
}

// Stop makes a last try of a drop that failed as it ended the ticket's
// pump: after Stop no open-ticket row remains.
func TestStopMakesALastTryOfAFailedDrop(t *testing.T) {
	t.Parallel()
	s, conn, _ := keepScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	s.openTicket(t, conn, "10105")
	drops := &dropsRefused{Store: s.st, refuse: 1}
	s.svc.store = drops
	s.svc.Stop()
	if n := drops.count(); n != 2 {
		t.Fatalf("the drop ran %d times; want the failure and the last try", n)
	}
	if n := s.openTicketRows(t); n != 0 {
		t.Fatalf("%d open-ticket rows after Stop", n)
	}
}
