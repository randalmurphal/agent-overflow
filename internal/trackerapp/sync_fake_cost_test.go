package trackerapp

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// The requests each unit makes against the fake trackers, through the
// real providers and clients, pinned so a change that adds one fails.

// requestsOf runs f and returns the routes of the requests the fake
// recorded meanwhile, counted.
func (s *fakeScenario) requestsOf(t *testing.T, f func()) map[string]int {
	t.Helper()
	since := 0
	if log := s.e.Requests(0).Requests; len(log) > 0 {
		since = log[len(log)-1].Seq
	}
	f()
	out := map[string]int{}
	log := s.e.Requests(since)
	if log.Dropped > 0 {
		t.Fatalf("the request log dropped %d requests", log.Dropped)
	}
	for _, r := range log.Requests {
		route := r.Route
		if r.Operation != "" {
			route = r.Operation
		}
		out[route]++
	}
	return out
}

func total(routes map[string]int) int {
	n := 0
	for _, c := range routes {
		n += c
	}
	return n
}

func describe(routes map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(routes)) {
		parts = append(parts, fmt.Sprintf("%s x%d", k, routes[k]))
	}
	return strings.Join(parts, ", ")
}

// unitCost is one measured operation and its pinned request count.
type unitCost struct {
	name string
	want int
	run  func(t *testing.T)
}

// measure runs each operation in order, logs its requests and fails when
// a count differs from its pin.
func (s *fakeScenario) measure(t *testing.T, ops []unitCost) {
	t.Helper()
	for _, op := range ops {
		routes := s.requestsOf(t, func() { op.run(t) })
		t.Logf("%-40s %2d requests: %s", op.name, total(routes), describe(routes))
		if got := total(routes); got != op.want {
			t.Errorf("%s: %d requests, pinned at %d: %s", op.name, got, op.want, describe(routes))
		}
	}
}

// tickOK runs one tick and fails on anything but a clean outcome.
func tickOK(t *testing.T, s *Service, scope Scope, due BoardDue) Outcome {
	t.Helper()
	out, err := s.syncTick(t.Context(), scope, due, Background)
	if err != nil || out.Failure != nil || out.Ended {
		t.Fatalf("tick %+v: %+v, %v", due, out, err)
	}
	return out
}

func (s *fakeScenario) costs(t *testing.T, conn Connection, scope Scope, edit func(), one tracker.IssueID, pins map[string]int) {
	t.Helper()
	pin := func(name string) int {
		n, ok := pins[name]
		if !ok {
			t.Fatalf("no pin for %s", name)
		}
		return n
	}
	ops := []unitCost{
		{"subscribe: structure", pin("structure"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Structure: true}) }},
		{"subscribe: reconcile, all missing", pin("reconcile missing"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Reconcile: true}) }},
		{"subscribe: delta, first walk", pin("delta first"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Delta: true}) }},
		{"subscribe: history, a batch", pin("history batch"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{History: true}) }},
		{"delta, nothing changed", pin("delta none"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Delta: true}) }},
		{"reconcile, all held", pin("reconcile held"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Reconcile: true}) }},
		{"structure again", pin("structure again"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Structure: true}) }},
		{"history, nothing due", 0, func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{History: true}) }},
	}
	s.measure(t, ops)
	// The edit lands after the time the providers read history as final
	// through (the wall clock less a settling margin), so it is due.
	s.clock.advance(2 * time.Hour)
	edit()
	s.measure(t, []unitCost{
		{"delta, one changed issue", pin("delta one"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{Delta: true}) }},
		{"history, one issue", pin("history one"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{History: true}) }},
		// The provider reads history as final through the tracker's clock
		// less a settling margin, so the issue edited within the margin is
		// read once more only when a read can be final for its edit
		// (TestHistoryIsReadTwicePerChange), never on the ticks between.
		{"history, again within the margin", 0, func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{History: true}) }},
		{"linked tick, nothing linked", 0, func(t *testing.T) {
			if out, err := s.svc.SyncLinked(t.Context(), conn.ID, LinkedDue{Cards: true}, Background); err != nil || out.Failure != nil {
				t.Fatalf("%+v, %v", out, err)
			}
		}},
		{"board view and a section page", 0, func(t *testing.T) {
			must[BoardView](t)(s.svc.Board(scope, []Section{{Kind: SectionColumn}, {Kind: SectionBacklog}}))
			must[SectionPage](t)(s.svc.SectionRows(scope, Section{Kind: SectionColumn, Column: 1}, nil))
		}},
	})
}

// detailCosts measures the opens of one issue's detail on scope's board,
// requests and store statements: read with none stored, verified with
// nothing changed (a card check where the tracker's update time moves with
// its detail, a detail read otherwise), the mirror's copy, verified with
// nothing changed once the issue links three others (created through
// project with linkType), opened again after a comment, which must show,
// after sync stored a newer card, past the age bound, and refreshed by a
// person.
func (s *fakeScenario) detailCosts(t *testing.T, conn Connection, scope Scope, one tracker.IssueID, project tracker.ProjectID, issueType tracker.IssueTypeID, linkType tracker.LinkTypeID, pins map[string][2]int) {
	t.Helper()
	tc := func(name string, run func(t *testing.T)) tickCost {
		p, ok := pins[name]
		if !ok {
			t.Fatalf("no pin for %s", name)
		}
		return tickCost{name: name, requests: p[0], stmts: p[1], run: run}
	}
	open := func(t *testing.T) DetailView {
		t.Helper()
		v, _, err := s.svc.IssueDetail(t.Context(), conn.ID, one, Interactive)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	s.measureTicks(t, []tickCost{
		tc("detail, none stored", func(t *testing.T) { open(t) }),
		tc("detail, verified unchanged", func(t *testing.T) { open(t) }),
		tc("detail, the mirror's copy", func(t *testing.T) {
			if _, _, err := s.svc.MirroredDetail(conn.ID, one); err != nil {
				t.Fatal(err)
			}
		}),
	})
	// Three links move the issue's update time: an open reads the detail
	// with them before the measured one.
	p := s.provider(conn.ID)
	s.clock.advance(time.Minute)
	for i := range 3 {
		created, ok := applied(t, "create", p.Create(t.Context(), tracker.CreateInput{Project: project, Type: issueType, Title: fmt.Sprint("Linked task ", i)})).Created()
		if !ok {
			t.Fatal("the create named no issue")
		}
		applied(t, "link", p.Link(t.Context(), tracker.LinkInput{Type: linkType, Outward: one, Inward: created}))
	}
	open(t)
	s.measureTicks(t, []tickCost{
		tc("detail, verified unchanged with 3 links", func(t *testing.T) {
			if v := open(t); len(v.Links) != 3 {
				t.Fatalf("%d links", len(v.Links))
			}
		}),
	})
	s.clock.advance(time.Minute)
	applied(t, "comment", p.AddComment(t.Context(), tracker.CommentInput{Issue: one, Markdown: "A new comment"}))
	s.measureTicks(t, []tickCost{
		tc("detail, after a comment", func(t *testing.T) {
			v := open(t)
			if !slices.ContainsFunc(v.Comments, func(c CommentView) bool { return strings.TrimSpace(c.Body) == "A new comment" }) {
				t.Fatalf("the comment does not show: %+v", v.Comments)
			}
		}),
	})
	// Sync stores the card an edit moved before the open.
	s.clock.advance(time.Minute)
	applied(t, "edit", p.Edit(t.Context(), tracker.EditInput{Issue: one, Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed before the open")}}}))
	tickOK(t, s.svc, scope, BoardDue{Delta: true})
	s.measureTicks(t, []tickCost{
		tc("detail, the mirror already newer", func(t *testing.T) {
			if v := open(t); v.Card.Title != "Renamed before the open" {
				t.Fatalf("the detail shows %q", v.Card.Title)
			}
		}),
	})
	s.svc.now = func() time.Time { return time.Now().Add(detailMaxAge + time.Minute) }
	s.measureTicks(t, []tickCost{
		tc("detail, older than the age bound", func(t *testing.T) { open(t) }),
		tc("detail, forced refresh", func(t *testing.T) {
			if _, _, err := s.svc.RefreshIssueDetail(t.Context(), conn.ID, one, Interactive); err != nil {
				t.Fatal(err)
			}
		}),
	})
}

func TestFakeJiraRequestCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	edit := func() {
		applied(t, "edit", s.provider(conn.ID).Edit(t.Context(), tracker.EditInput{Issue: "10102", Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")}}}))
	}
	s.costs(t, conn, scope, edit, "10102", map[string]int{
		"structure": 3, "structure again": 3, "reconcile missing": 3, "delta first": 1, "history batch": 2,
		"delta none": 1, "reconcile held": 1, "delta one": 1, "history one": 2,
	})
	// Jira's update time moves with its detail: a check is one card read
	// (an issue bulk fetch, the linked issues' cards in it), and a moved
	// card adds the detail's two reads (the issue, then its comments). A
	// mirror card already newer, the age bound and a refresh read the
	// detail with no check.
	s.detailCosts(t, conn, scope, "10102", "10000", "10002", "10000", map[string][2]int{
		"detail, none stored": {2, 5}, "detail, verified unchanged": {1, 5},
		"detail, the mirror's copy": {0, 5}, "detail, verified unchanged with 3 links": {1, 5},
		"detail, after a comment": {3, 6}, "detail, the mirror already newer": {2, 5},
		"detail, older than the age bound": {2, 5}, "detail, forced refresh": {2, 4},
	})
}

func TestFakeLinearRequestCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	})
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
	s.listBoards(conn.ID, scope.Board)
	edit := func() {
		applied(t, "edit", s.provider(conn.ID).Edit(t.Context(), tracker.EditInput{Issue: tracker.IssueID(lid(61)), Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Add search, renamed")}}}))
	}
	s.costs(t, conn, scope, edit, tracker.IssueID(lid(61)), map[string]int{
		"structure": 2, "structure again": 1, "reconcile missing": 3, "delta first": 1, "history batch": 1,
		"delta none": 1, "reconcile held": 2, "delta one": 1, "history one": 1,
	})
	// Linear's update time does not move with a comment: every open reads
	// the detail, in one request, and reads no stored detail first.
	s.detailCosts(t, conn, scope, tracker.IssueID(lid(61)), tracker.ProjectID(lid(10)), "", "related", map[string][2]int{
		"detail, none stored": {1, 4}, "detail, verified unchanged": {1, 4},
		"detail, the mirror's copy": {0, 5}, "detail, verified unchanged with 3 links": {1, 4},
		"detail, after a comment": {1, 4}, "detail, the mirror already newer": {1, 4},
		"detail, older than the age bound": {1, 4}, "detail, forced refresh": {1, 4},
	})
}

// largeIssues is how many issues the large scenarios seed: one Jira row
// page, five Linear pages, five batches of missing rows and ten history
// reads.
const largeIssues = 500

// tickCost is one measured tick: its requests to the fake and its store
// statements (accessor calls, each one statement or transaction), both
// pinned; when stmtNames is set, the statements by name too.
type tickCost struct {
	name            string
	requests, stmts int
	stmtNames       map[string]int
	run             func(t *testing.T)
}

// measureTicks runs each tick in order through a counting store, logs
// its requests and statements and fails when either differs from its pin.
func (s *fakeScenario) measureTicks(t *testing.T, ticks []tickCost) {
	t.Helper()
	cs := newCountingStore(s.st)
	s.svc.store = cs
	for _, tc := range ticks {
		cs.take()
		routes := s.requestsOf(t, func() { tc.run(t) })
		stmts := cs.take()
		t.Logf("%-20s %3d requests: %s", tc.name, total(routes), describe(routes))
		t.Logf("%-20s %3d statements: %s", "", total(stmts), describeCounts(stmts))
		if got := total(routes); got != tc.requests {
			t.Errorf("%s: %d requests, pinned at %d: %s", tc.name, got, tc.requests, describe(routes))
		}
		if got := total(stmts); got != tc.stmts {
			t.Errorf("%s: %d statements, pinned at %d: %s", tc.name, got, tc.stmts, describeCounts(stmts))
		}
		if tc.stmtNames != nil && !maps.Equal(stmts, tc.stmtNames) {
			t.Errorf("%s: statements %s, pinned %s", tc.name, describeCounts(stmts), describeCounts(tc.stmtNames))
		}
	}
}

// largeCosts measures the ticks that scale with a board's size: the first
// sync of the board (structure, reconcile and delta in one tick), the
// history backfill up to its bound, an idle tick, and a tick that reads
// 10 changed rows. edit changes 10 issues.
func (s *fakeScenario) largeCosts(t *testing.T, scope Scope, edit func(), pins map[string][2]int) {
	t.Helper()
	tc := func(name string, run func(t *testing.T)) tickCost {
		p, ok := pins[name]
		if !ok {
			t.Fatalf("no pin for %s", name)
		}
		return tickCost{name: name, requests: p[0], stmts: p[1], run: run}
	}
	// An idle tick reads the scope's sync state, asks the tracker once and
	// records the read: the service holds the connection, its provider and
	// its mirror root.
	idle := tc("idle tick", func(t *testing.T) {
		if out := tickOK(t, s.svc, scope, BoardDue{Delta: true}); !out.Changes().Empty() {
			t.Fatalf("an idle tick stored %+v", out.Changes())
		}
	})
	idle.stmtNames = map[string]int{"GetMirrorSync": 1, "RecordMirrorSyncAttempt": 1}
	// Opening the board with its backlog and first column: the root, one
	// snapshot of the structure, totals, pages and the names of the columns
	// and cards (6+2+1 statements in the store), and the sync state. The
	// fixtures' cards have no parents, so no parent read.
	open := tc("board open", func(t *testing.T) {
		v := must[BoardView](t)(s.svc.Board(scope, []Section{{Kind: SectionBacklog}, {Kind: SectionColumn, Column: 0}}))
		if v.All.Rows != largeIssues || len(v.Sections) != 2 {
			t.Fatalf("the board holds %d rows, %d sections", v.All.Rows, len(v.Sections))
		}
	})
	open.stmtNames = map[string]int{"GetMirrorRoot": 1, "ReadMirrorBoardView": 1, "GetMirrorSync": 1}
	s.listBoards(scope.Connection, scope.Board)
	s.measureTicks(t, []tickCost{
		tc("first sync", func(t *testing.T) {
			if out := tickOK(t, s.svc, scope, BoardDue{Structure: true, Reconcile: true, Delta: true}); len(out.Changes().Rows) != largeIssues || out.More() {
				t.Fatalf("the first sync stored %d rows, more %v", len(out.Changes().Rows), out.More())
			}
		}),
		tc("history backfill", func(t *testing.T) {
			out := tickOK(t, s.svc, scope, BoardDue{History: true})
			if len(out.Changes().History) != historyBatch*historyBatches || !out.More() {
				t.Fatalf("the backfill stored %d histories, more %v", len(out.Changes().History), out.More())
			}
		}),
		idle,
	})
	s.clock.advance(2 * time.Hour)
	edit()
	s.measureTicks(t, []tickCost{
		tc("10 changed rows", func(t *testing.T) {
			if out := tickOK(t, s.svc, scope, BoardDue{Delta: true}); len(out.Changes().Cards) != 10 {
				t.Fatalf("the tick stored %d changed cards", len(out.Changes().Cards))
			}
		}),
		open,
	})
}

// editTitles renames issues through the provider, as a person would.
func (s *fakeScenario) editTitles(t *testing.T, conn string, issues []tracker.IssueID) {
	t.Helper()
	p := s.provider(conn)
	for _, id := range issues {
		applied(t, "edit", p.Edit(t.Context(), tracker.EditInput{Issue: id, Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed " + string(id))}}}))
	}
}

// largeJiraFixture is the Jira seed with board 1 holding n tasks, each
// updated a second after the one before.
func largeJiraFixture(n int) func(at time.Time) trackerfake.Fixture {
	return func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		day := at.Add(-48 * time.Hour)
		site.Issues = nil
		for i := range n {
			site.Issues = append(site.Issues, trackerfake.JiraIssue{
				ID: fmt.Sprint(20000 + i), Key: fmt.Sprintf("WEB-%d", i+1), Project: "WEB", Type: "10002",
				Summary: fmt.Sprintf("Task %d", i+1), Status: "10000", Assignee: "acc-user1", Created: day, Updated: day.Add(time.Duration(i) * time.Second),
			})
		}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}
}

// largeLinearFixture is the Linear seed with team 10 holding n issues,
// each updated a second after the one before.
func largeLinearFixture(n int) func(at time.Time) trackerfake.Fixture {
	return func(at time.Time) trackerfake.Fixture {
		w := linearSeed(at)
		w.Issues = nil
		for i := range n {
			w.Issues = append(w.Issues, trackerfake.LinearIssue{
				ID: lid(1000 + i), Team: lid(10), Number: float64(i + 1), Title: fmt.Sprintf("Task %d", i+1), State: lid(21), Assignee: lid(1),
				Creator: lid(1), SortOrder: float64(i), CreatedAt: at.Add(-72 * time.Hour), UpdatedAt: at.Add(-48*time.Hour + time.Duration(i)*time.Second),
			})
		}
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{w}}
	}
}

func TestFakeJiraLargeBoardCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, largeJiraFixture(largeIssues), "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	var edited []tracker.IssueID
	for i := range 10 {
		edited = append(edited, tracker.IssueID(fmt.Sprint(20000+i*7)))
	}
	s.largeCosts(t, Scope{Connection: conn.ID, Board: "1"}, func() { s.editTitles(t, conn.ID, edited) }, map[string][2]int{
		"first sync": {6, 8}, "history backfill": {8, 6}, "idle tick": {1, 2}, "10 changed rows": {1, 3}, "board open": {0, 3},
	})
}

func TestFakeLinearLargeBoardCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, largeLinearFixture(largeIssues))
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	var edited []tracker.IssueID
	for i := range 10 {
		edited = append(edited, tracker.IssueID(lid(1000+i*7)))
	}
	s.largeCosts(t, Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}, func() { s.editTitles(t, conn.ID, edited) }, map[string][2]int{
		"first sync": {7, 8}, "history backfill": {4, 6}, "idle tick": {1, 2}, "10 changed rows": {1, 3}, "board open": {0, 3},
	})
}
