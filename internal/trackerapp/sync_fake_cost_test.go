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
	out, err := s.SyncBoard(t.Context(), scope, due, Background)
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
		{"detail, read through", pin("detail"), func(t *testing.T) {
			if _, _, err := s.svc.IssueDetail(t.Context(), conn.ID, one, Interactive); err != nil {
				t.Fatal(err)
			}
		}},
		{"detail, from the mirror", 0, func(t *testing.T) {
			if _, _, err := s.svc.IssueDetail(t.Context(), conn.ID, one, Interactive); err != nil {
				t.Fatal(err)
			}
		}},
		// The provider reads history as final through the tracker's clock
		// less a settling margin, so an issue edited within the margin stays
		// due and each history tick reads it again until the margin passes.
		{"history, again within the margin", pin("history one"), func(t *testing.T) { tickOK(t, s.svc, scope, BoardDue{History: true}) }},
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
		"delta none": 1, "reconcile held": 1, "delta one": 1, "history one": 2, "detail": 2,
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
		"structure": 5, "structure again": 4, "reconcile missing": 3, "delta first": 1, "history batch": 1,
		"delta none": 1, "reconcile held": 2, "delta one": 1, "history one": 1, "detail": 1,
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

func TestFakeJiraLargeBoardCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		day := at.Add(-48 * time.Hour)
		site.Issues = nil
		for i := range largeIssues {
			site.Issues = append(site.Issues, trackerfake.JiraIssue{
				ID: fmt.Sprint(20000 + i), Key: fmt.Sprintf("WEB-%d", i+1), Project: "WEB", Type: "10002",
				Summary: fmt.Sprintf("Task %d", i+1), Status: "10000", Assignee: "acc-user1", Created: day, Updated: day.Add(time.Duration(i) * time.Second),
			})
		}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	var edited []tracker.IssueID
	for i := range 10 {
		edited = append(edited, tracker.IssueID(fmt.Sprint(20000+i*7)))
	}
	s.largeCosts(t, Scope{Connection: conn.ID, Board: "1"}, func() { s.editTitles(t, conn.ID, edited) }, map[string][2]int{
		"first sync": {6, 9}, "history backfill": {8, 7}, "idle tick": {1, 2}, "10 changed rows": {1, 4},
	})
}

func TestFakeLinearLargeBoardCosts(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		w := linearSeed(at)
		w.Issues = nil
		for i := range largeIssues {
			w.Issues = append(w.Issues, trackerfake.LinearIssue{
				ID: lid(1000 + i), Team: lid(10), Number: float64(i + 1), Title: fmt.Sprintf("Task %d", i+1), State: lid(21), Assignee: lid(1),
				Creator: lid(1), SortOrder: float64(i), CreatedAt: at.Add(-72 * time.Hour), UpdatedAt: at.Add(-48*time.Hour + time.Duration(i)*time.Second),
			})
		}
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{w}}
	})
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	var edited []tracker.IssueID
	for i := range 10 {
		edited = append(edited, tracker.IssueID(lid(1000+i*7)))
	}
	s.largeCosts(t, Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}, func() { s.editTitles(t, conn.ID, edited) }, map[string][2]int{
		"first sync": {10, 9}, "history backfill": {4, 7}, "idle tick": {1, 2}, "10 changed rows": {1, 4},
	})
}
