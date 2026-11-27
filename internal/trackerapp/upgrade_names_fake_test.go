package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An upgrade to v172 left a mirror whose stored issues name parents by ID
// alone: no read before it kept a parent's name. The scenarios make that
// mirror as an upgrade does: the store's file, its board read, is made
// the v171 database it would have been and opened again, which runs v172. The stories here are
// open, untouched for longer than the history window, so no delta reads
// them again, and their epics are in a project no board lists. The
// linked tick, run to its end as the linked loop runs it, reads each
// epic's own card once and names it; an epic the tracker no longer
// returns is asked for once and never again.

// upgradeEpics is how many epics, one story each, the scenarios seed: one
// more than the store's batch of unnamed parents (100), so the worklist
// takes two.
const upgradeEpics = 101

// sqlOnStore runs stmt on the fixture's store file, for a state no store
// method writes.
func (f *fixture) sqlOnStore(t *testing.T, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.file+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, errors.Join(err, db.Close()))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// reopen stops the service, closes the store, runs between on its file,
// and opens the file again, as a quit and a start of the app do: the open
// applies every migration the file has not had. A new service runs over
// the store it opened.
func (f *fixture) reopen(t *testing.T, between func()) {
	t.Helper()
	f.svc.Stop()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between()
	}
	st, err := store.New(f.file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	f.st, f.deps.Store = st, st
	f.restart(t)
}

// upgradedFrom171 makes the store's file the v171 database its mirror
// would have been, v172's table, column and record and the later
// migrations' tables and records taken out, and opens it again, so v172
// runs on it as it runs on an upgrade.
func (s *fakeScenario) upgradedFrom171(t *testing.T) {
	t.Helper()
	s.reopen(t, func() {
		s.sqlOnStore(t, `DROP TABLE activity_spans`)
		s.sqlOnStore(t, `DROP TABLE thread_days`)
		s.sqlOnStore(t, `DELETE FROM migration_versions WHERE version = 174`)
		s.sqlOnStore(t, `DROP TABLE tracker_time_entries`)
		s.sqlOnStore(t, `DELETE FROM migration_versions WHERE version = 173`)
		s.sqlOnStore(t, `DROP TABLE tracker_cache_parents`)
		s.sqlOnStore(t, `ALTER TABLE tracker_cache_roots DROP COLUMN names_after`)
		s.sqlOnStore(t, `DELETE FROM migration_versions WHERE version = 172`)
	})
	s.svc.now = time.Now
}

// runLinked runs the linked loop's work for conn to its end, as the loop
// does while a client is connected, and fails on any failure it reports.
func (s *fakeScenario) runLinked(t *testing.T, conn Connection) {
	t.Helper()
	_, _, before := s.events.snapshot()
	s.svc.linkedConnection(t.Context(), conn.ID)
	if _, _, errs := s.events.snapshot(); len(errs) > len(before) {
		t.Fatalf("the linked loop failed: %v", errs[len(before):])
	}
	if owed := must[store.MirrorUnnamedParents](t)(s.st.ListMirrorUnnamedParents(must[int64](t)(rootOf(s, conn)))); owed.Owed {
		t.Fatalf("the worklist is owed %+v after the loop", owed)
	}
}

// boardParents are the parents the board's cards name, by card.
func (s *fakeScenario) boardParents(t *testing.T, scope Scope) map[string]ParentView {
	t.Helper()
	out := map[string]ParentView{}
	v := must[BoardView](t)(s.svc.Board(scope, StatusGroups{}, nil))
	sections := []Section{{Kind: SectionBacklog}}
	for col := range v.Columns {
		sections = append(sections, Section{Kind: SectionColumn, Column: col})
	}
	for _, sec := range sections {
		var after *SectionCursor
		for {
			page := must[SectionPage](t)(s.svc.SectionRows(scope, StatusGroups{}, sec, after))
			for _, r := range page.Rows {
				if r.Card.Parent != nil {
					out[r.Card.ID] = *r.Card.Parent
				}
			}
			if page.Next == nil {
				break
			}
			after = page.Next
		}
	}
	return out
}

// exportEpics is the dashboard export's epic of each ticket of scope that
// has one, as "key title".
func (s *fakeScenario) exportEpics(t *testing.T, scope Scope) map[string]string {
	t.Helper()
	b, err := s.svc.dashboardBoard(scope, StatusGroups{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.svc.leave()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, ok, err := b.Fill(t.Context(), conn, []string{"tickets", "epics"}); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	rows, err := conn.QueryContext(t.Context(), `SELECT t.id, e.key || ' ' || e.title FROM tickets t JOIN epics e ON e.id = t.epic`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	out := map[string]string{}
	for rows.Next() {
		var id, epic string
		if err := rows.Scan(&id, &epic); err != nil {
			t.Fatal(err)
		}
		out[id] = epic
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// askedFor counts the requests since seq whose query, body or variables
// name id.
func (s *fakeScenario) askedFor(since int, id string) int {
	n := 0
	for _, r := range s.e.Requests(since).Requests {
		if strings.Contains(r.Query.Encode(), id) || strings.Contains(string(r.Body), id) || strings.Contains(string(r.Variables), id) {
			n++
		}
	}
	return n
}

// lastSeq is the sequence number of the fake's last request.
func (s *fakeScenario) lastSeq() int {
	reqs := s.e.Requests(0).Requests
	if len(reqs) == 0 {
		return 0
	}
	return reqs[len(reqs)-1].Seq
}

// upgradeJiraSite is jiraSeed with upgradeEpics epics in OPS, which no
// board lists, each with one WEB story, open and untouched for longer than
// the history window.
func upgradeJiraSite(at time.Time) trackerfake.JiraSite {
	site := jiraSeed(at)
	old := at.Add(-HistoryWindow - 20*24*time.Hour)
	site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	for i := 1; i <= upgradeEpics; i++ {
		site.Issues = append(site.Issues,
			trackerfake.JiraIssue{ID: strconv.Itoa(20000 + i), Key: fmt.Sprintf("OPS-%d", i), Project: "OPS", Type: "10000",
				Summary: fmt.Sprintf("Epic %d", i), Status: "10000", Created: old, Updated: old},
			trackerfake.JiraIssue{ID: strconv.Itoa(30000 + i), Key: fmt.Sprintf("WEB-%d", 100+i), Project: "WEB", Type: "10001",
				Summary: fmt.Sprintf("Story %d", i), Parent: strconv.Itoa(20000 + i), Status: "10000", Created: old, Updated: old})
	}
	return site
}

// upgradeLinearWorkspace is linearSeed with upgradeEpics parents in team
// OPS, which no board lists, each with one WEB child, open and untouched
// for longer than the history window.
func upgradeLinearWorkspace(at time.Time) trackerfake.LinearWorkspace {
	ws := linearSeed(at)
	old := at.Add(-HistoryWindow - 20*24*time.Hour)
	ws.Teams = append(ws.Teams, trackerfake.LinearTeam{ID: lid(11), Key: "OPS", Name: "Ops", Timezone: "UTC", DefaultState: lid(25), Members: []string{lid(1)}})
	ws.States = append(ws.States, trackerfake.LinearState{ID: lid(25), Team: lid(11), Name: "Todo", Type: "unstarted", Position: 0})
	for i := 1; i <= upgradeEpics; i++ {
		ws.Issues = append(ws.Issues,
			trackerfake.LinearIssue{ID: lid(1000 + i), Team: lid(11), Number: float64(i), Title: fmt.Sprintf("Parent %d", i), State: lid(25),
				Creator: lid(1), SortOrder: float64(i), CreatedAt: old, UpdatedAt: old},
			trackerfake.LinearIssue{ID: lid(3000 + i), Team: lid(10), Number: float64(100 + i), Title: fmt.Sprintf("Child %d", i), State: lid(21),
				Creator: lid(1), Parent: lid(1000 + i), SortOrder: float64(1000 + i), CreatedAt: old, UpdatedAt: old})
	}
	return ws
}

// deleteParent deletes parent from the fake trackers, its child no longer
// naming it, as the tracker answers once a parent is deleted. The
// mirror's copy of the child, read before, still names it.
func (s *fakeScenario) deleteParent(t *testing.T, parent string) {
	t.Helper()
	world := s.e.Snapshot()
	for i := range world.Jira {
		world.Jira[i].Token = "fake-jira-token-1"
		world.Jira[i].Issues = slices.DeleteFunc(world.Jira[i].Issues, func(is trackerfake.JiraIssue) bool { return is.ID == parent })
		for j := range world.Jira[i].Issues {
			if world.Jira[i].Issues[j].Parent == parent {
				world.Jira[i].Issues[j].Parent = ""
			}
		}
	}
	for i := range world.Linear {
		world.Linear[i].APIKey = "lin_api_fake00001"
		world.Linear[i].Issues = slices.DeleteFunc(world.Linear[i].Issues, func(is trackerfake.LinearIssue) bool { return is.ID == parent })
		for j := range world.Linear[i].Issues {
			if world.Linear[i].Issues[j].Parent == parent {
				world.Linear[i].Issues[j].Parent = ""
			}
		}
	}
	if err := s.e.Seed(world); err != nil {
		t.Fatal(err)
	}
}

// upgradeCase is one tracker's upgrade scenario: its seed, connection and
// board, and the IDs and names of its parents and children.
type upgradeCase struct {
	name                string
	scenario            func(t *testing.T) (*fakeScenario, Connection, Scope)
	parent, child       func(i int) string
	parentKey, parentAs func(i int) string
	// epics is whether the dashboard export has epics: a Linear ticket's
	// parent is no epic.
	epics bool
}

var upgradeCases = []upgradeCase{
	{
		name: "jira",
		scenario: func(t *testing.T) (*fakeScenario, Connection, Scope) {
			s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
				return trackerfake.Fixture{Jira: []trackerfake.JiraSite{upgradeJiraSite(at)}}
			}, "example.atlassian.net")
			conn := s.addJira()
			s.writeToken(conn, "fake-jira-token-1")
			scope := Scope{Connection: conn.ID, Board: "1"}
			s.listBoards(conn.ID, scope.Board)
			return s, conn, scope
		},
		parent:    func(i int) string { return strconv.Itoa(20000 + i) },
		child:     func(i int) string { return strconv.Itoa(30000 + i) },
		parentKey: func(i int) string { return fmt.Sprintf("OPS-%d", i) },
		parentAs:  func(i int) string { return fmt.Sprintf("Epic %d", i) },
		epics:     true,
	},
	{
		name: "linear",
		scenario: func(t *testing.T) (*fakeScenario, Connection, Scope) {
			s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
				return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{upgradeLinearWorkspace(at)}}
			})
			conn := s.addLinear()
			s.writeToken(conn, "lin_api_fake00001")
			scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
			s.listBoards(conn.ID, scope.Board)
			return s, conn, scope
		},
		parent:    func(i int) string { return lid(1000 + i) },
		child:     func(i int) string { return lid(3000 + i) },
		parentKey: func(i int) string { return fmt.Sprintf("OPS-%d", i) },
		parentAs:  func(i int) string { return fmt.Sprintf("Parent %d", i) },
	},
}

// upgraded syncs c's board, deletes the first parent from the tracker, so
// the worklist's first batch holds a parent the tracker does not return,
// and
// puts the mirror in the state an upgrade from v171 leaves.
func (c upgradeCase) upgraded(t *testing.T) (*fakeScenario, Connection, Scope) {
	t.Helper()
	s, conn, scope := c.scenario(t)
	s.syncBoard(t, scope)
	s.deleteParent(t, c.parent(1))
	s.upgradedFrom171(t)
	if owed := must[store.MirrorUnnamedParents](t)(s.st.ListMirrorUnnamedParents(must[int64](t)(rootOf(s, conn)))); !owed.Owed {
		t.Fatalf("the upgraded mirror owes no names: %+v", owed)
	}
	if p := s.boardParents(t, scope); p[c.child(2)].Title != "" {
		t.Fatalf("the upgraded mirror names %s's parent %+v", c.child(2), p[c.child(2)])
	}
	return s, conn, scope
}

// check fails unless the board's cards and, with epics, the dashboard
// export name every parent the tracker returns, and the parent it no
// longer returns by ID alone.
func (c upgradeCase) check(t *testing.T, s *fakeScenario, scope Scope) {
	t.Helper()
	parents := s.boardParents(t, scope)
	var epics map[string]string
	if c.epics {
		epics = s.exportEpics(t, scope)
	}
	for i := 1; i <= upgradeEpics; i++ {
		want := ParentView{ID: c.parent(i), Key: c.parentKey(i), Title: c.parentAs(i)}
		if i == 1 {
			want = ParentView{ID: c.parent(i)}
		}
		if got := parents[c.child(i)]; got != want {
			t.Fatalf("the board's %s names its parent %+v; want %+v", c.child(i), got, want)
		}
		if !c.epics {
			continue
		}
		wantEpic := c.parentKey(i) + " " + c.parentAs(i)
		if i == 1 {
			wantEpic = ""
		}
		if got := epics[c.child(i)]; got != wantEpic {
			t.Fatalf("the dashboard export's epic of %s is %q; want %q", c.child(i), got, wantEpic)
		}
	}
}

func TestTheLinkedTickNamesTheParentsAnUpgradeOwes(t *testing.T) {
	t.Parallel()
	for _, c := range upgradeCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, conn, scope := c.upgraded(t)
			since := s.lastSeq()
			s.runLinked(t, conn)
			c.check(t, s, scope)
			if n := s.askedFor(since, c.parent(1)); n != 1 {
				t.Fatalf("the parent the tracker does not return was asked for %d times; want once", n)
			}
			if n := s.askedFor(since, c.parent(2)); n != 1 {
				t.Fatalf("a parent was asked for %d times; want once", n)
			}
			// The worklist is over: the loop's next run, as after the next
			// interval or a restart, asks for no parent.
			since = s.lastSeq()
			s.runLinked(t, conn)
			s.reopen(t, nil)
			s.svc.now = time.Now
			s.runLinked(t, conn)
			if n := s.askedFor(since, c.parent(1)); n != 0 {
				t.Fatalf("the parent the tracker does not return was asked for again %d times", n)
			}
		})
	}
}

// listsCut is a store whose second listing of unnamed parents fails, as
// the app quitting after the worklist's first batch would leave it.
type listsCut struct {
	Store
	lists atomic.Int32
}

func (c *listsCut) ListMirrorUnnamedParents(rootID int64) (store.MirrorUnnamedParents, error) {
	if c.lists.Add(1) == 2 {
		return store.MirrorUnnamedParents{}, errors.New("the app quit")
	}
	return c.Store.ListMirrorUnnamedParents(rootID)
}

// A restart between the worklist's two batches, the store closed and
// opened again, asks for the second batch's parents alone: the first
// batch's cursor was passed as its read was stored.
func TestARestartBetweenNameBatchesAsksForTheRestAlone(t *testing.T) {
	t.Parallel()
	for _, c := range upgradeCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, conn, scope := c.upgraded(t)
			s.svc.store = &listsCut{Store: s.st}
			_, _, before := s.events.snapshot()
			s.svc.linkedConnection(t.Context(), conn.ID)
			if _, _, errs := s.events.snapshot(); len(errs) != len(before)+1 || !strings.Contains(errs[len(errs)-1].Error(), "the app quit") {
				t.Fatalf("the cut linked loop reported %v", errs[len(before):])
			}
			if p := s.boardParents(t, scope); p[c.child(2)].Title != c.parentAs(2) {
				t.Fatalf("after the first batch %s names its parent %+v", c.child(2), p[c.child(2)])
			}
			s.reopen(t, nil)
			s.svc.now = time.Now
			since := s.lastSeq()
			s.runLinked(t, conn)
			c.check(t, s, scope)
			for i, want := range map[int]int{1: 0, upgradeEpics - 1: 0, upgradeEpics: 1} {
				if n := s.askedFor(since, c.parent(i)); n != want {
					t.Fatalf("after the restart parent %d was asked for %d times; want %d", i, n, want)
				}
			}
		})
	}
}

// A mirror made after v172 owes no names: a new connection's, one made
// again after the connection was turned off and on, and one made after a
// restore, which empties the mirror's tables. Its linked tick asks for no
// parent.
func TestANewMirrorOwesNoNames(t *testing.T) {
	t.Parallel()
	c := upgradeCases[0]
	s, conn, scope := c.scenario(t)
	owesNothing := func(what string) {
		t.Helper()
		s.syncBoard(t, scope)
		root := must[int64](t)(rootOf(s, conn))
		if owed := must[store.MirrorUnnamedParents](t)(s.st.ListMirrorUnnamedParents(root)); owed.Owed {
			t.Fatalf("%s: the mirror owes %+v", what, owed)
		}
		since := s.lastSeq()
		out := must[Outcome](t)(s.svc.SyncLinked(t.Context(), conn.ID, LinkedDue{Names: true}, Background))
		if n := len(s.e.Requests(since).Requests); out.Failure != nil || out.More() || n > 0 {
			t.Fatalf("%s: the names tick %+v made %d requests", what, out, n)
		}
	}
	owesNothing("a new connection")
	old := must[int64](t)(rootOf(s, conn))
	must[Connection](t)(s.svc.SetEnabled(conn.ID, false))
	must[Connection](t)(s.svc.SetEnabled(conn.ID, true))
	owesNothing("turned off and on")
	if root := must[int64](t)(rootOf(s, conn)); root == old {
		t.Fatalf("turned off and on, the connection kept its root %d", root)
	}
	s.sqlOnStore(t, `DELETE FROM tracker_cache_roots`)
	owesNothing("after a restore")
}
