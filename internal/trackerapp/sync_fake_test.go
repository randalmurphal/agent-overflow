package trackerapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The scenarios run the units through the real providers and clients
// against the stateful fake trackers: seed a board, sync it, change the
// fake through its API as a person would, sync again, and compare what
// the mirror reads with what the fake holds.

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeScenario is a Service whose connections reach the fake trackers.
type fakeScenario struct {
	*fixture
	e     *trackerfake.Engine
	clock *fakeClock
}

func newFakeScenario(t *testing.T, seed func(at time.Time) trackerfake.Fixture, sites ...string) *fakeScenario {
	t.Helper()
	return newFakeScenarioWith(t, Cadence{}, seed, sites...)
}

// newFakeScenarioWith is newFakeScenario with the service's sync
// intervals cad.
func newFakeScenarioWith(t *testing.T, cad Cadence, seed func(at time.Time) trackerfake.Fixture, sites ...string) *fakeScenario {
	t.Helper()
	// The providers read history as final through a time behind the
	// wall clock, so the fake's clock and the service's are the wall
	// clock's.
	clock := &fakeClock{now: time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(seed(clock.Now())); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, sites...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	f := newFixtureWith(t, cad)
	f.svc.now = time.Now
	f.origin = func(kind, site string) (string, error) {
		if kind == store.TrackerKindLinear {
			return srv.LinearURL(), nil
		}
		return srv.JiraURL(strings.TrimPrefix(site, "https://")), nil
	}
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s %s %s: %s", r.Method, r.Path, r.Operation, r.Detail)
		}
	})
	return &fakeScenario{fixture: f, e: e, clock: clock}
}

// syncBoard runs every unit of scope once, as a subscribe would, and
// fails on any failure.
func (s *fakeScenario) syncBoard(t *testing.T, scope Scope) Changes {
	t.Helper()
	var all Changes
	for i, unit := range []func(context.Context, Scope, Urgency) (Outcome, error){
		s.svc.tickStructure, s.svc.tickReconcile, s.svc.tickDelta, s.svc.tickHistory,
	} {
		out, err := unit(t.Context(), scope, Background)
		if err != nil || out.Failure != nil || out.Ended || out.More() {
			t.Fatalf("unit %d (structure, reconcile, delta, history): %+v, %v", i, out, err)
		}
		all.add(out.Changes())
	}
	return all
}

// issueCard is what the fake holds of an issue as the mirror shows it.
type issueCard struct{ ID, Key, Title, Status string }

// compare fails unless the mirror's cards of the board are want.
func (s *fakeScenario) compare(t *testing.T, scope Scope, want []issueCard) {
	t.Helper()
	var got []issueCard
	for _, sec := range []Section{{Kind: SectionColumn, Column: 0}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}, {Kind: SectionColumn, Column: 3}} {
		page := must[SectionPage](t)(s.svc.SectionRows(scope, StatusGroups{}, sec, nil))
		for _, r := range page.Rows {
			got = append(got, issueCard{ID: r.Card.ID, Key: r.Card.Key, Title: r.Card.Title, Status: r.Card.Status.ID})
		}
	}
	byID := func(a, b issueCard) int { return strings.Compare(a.ID, b.ID) }
	slices.SortFunc(got, byID)
	slices.SortFunc(want, byID)
	if !slices.Equal(got, want) {
		t.Fatalf("the mirror shows\n%+v\nthe fake holds\n%+v", got, want)
	}
}

func jiraDoc(text string) json.RawMessage {
	return json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
}

func jiraSeed(at time.Time) trackerfake.JiraSite {
	day := at.Add(-48 * time.Hour)
	return trackerfake.JiraSite{
		Site: "example.atlassian.net", Account: "acc-user1", Token: "fake-jira-token-1",
		Users: []trackerfake.JiraUser{
			{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com", TimeZone: "UTC"},
			{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com", TimeZone: "UTC"},
		},
		Fields: []trackerfake.JiraField{
			{ID: "customfield_10016", Name: "Story Points", Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}},
		},
		Projects: []trackerfake.JiraProject{{ID: "10000", Key: "WEB", Name: "Web"}},
		Boards: []trackerfake.JiraBoard{{ID: 1, Name: "WEB board", Type: "scrum", Project: "WEB", Estimation: "customfield_10016", Columns: []trackerfake.JiraColumn{
			{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}},
		}}},
		Sprints: []trackerfake.JiraSprint{
			{ID: 1, Board: 1, Name: "Sprint 1", State: "active", Start: day, End: day.Add(14 * 24 * time.Hour)},
			{ID: 2, Board: 1, Name: "Sprint 2", State: "future"},
		},
		Issues: []trackerfake.JiraIssue{
			{ID: "10101", Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "First story", Description: jiraDoc("first"), Sprints: []int64{1},
				Assignee: "acc-user1", Reporter: "acc-user1", Priority: "3", Status: "10001", Created: day, Updated: day,
				Values: map[string]json.RawMessage{"customfield_10016": json.RawMessage(`5`)}},
			{ID: "10102", Key: "WEB-2", Project: "WEB", Type: "10002", Summary: "Second task", Sprints: []int64{1}, Status: "10000", Created: day, Updated: day},
			{ID: "10103", Key: "WEB-3", Project: "WEB", Type: "10002", Summary: "Backlog task", Status: "10000", Created: day, Updated: day},
		},
	}
}

func (s *fakeScenario) jiraCards() []issueCard {
	var out []issueCard
	for _, is := range s.e.Snapshot().Jira[0].Issues {
		out = append(out, issueCard{ID: is.ID, Key: is.Key, Title: is.Summary, Status: is.Status})
	}
	return out
}

func applied(t *testing.T, what string, r tracker.Result) tracker.Result {
	t.Helper()
	if !r.AllApplied() {
		t.Fatalf("%s: %+v", what, r)
	}
	return r
}

func TestFakeJiraScenario(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)

	first := s.syncBoard(t, scope)
	if !slices.Equal(first.Rows, ids("10101", "10102", "10103")) || !slices.Equal(first.History, ids("10101", "10102", "10103")) {
		t.Fatalf("first sync %+v", first)
	}
	s.compare(t, scope, s.jiraCards())
	v := must[BoardView](t)(s.svc.Board(scope, StatusGroups{}, []Section{{Kind: SectionSprint, Sprint: "1"}}))
	if len(v.Sprints) != 2 || v.Sprints[0].Total.Rows != 2 || v.Columns[1].Statuses[0].Name == "" || len(v.Sections[0].Rows) != 2 {
		t.Fatalf("board %+v", v)
	}
	if a := v.Sections[0].Rows[0].Card.Assignee; a == nil || a.Name != "User 1" {
		t.Fatalf("the sprint's first card %+v", v.Sections[0].Rows[0].Card)
	}

	// A person edits, moves and plans through the tracker.
	s.clock.advance(time.Minute)
	p := s.provider(conn.ID)
	applied(t, "edit", p.Edit(t.Context(), tracker.EditInput{Issue: "10102", Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")}}}))
	moves := must[[]tracker.Move](t)(p.Moves(t.Context(), "10101"))
	done := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == "10002" })
	applied(t, "move", p.Move(t.Context(), tracker.MoveInput{Issue: "10101", Move: moves[done].ID}))
	applied(t, "sprint move", p.MoveToSprint(t.Context(), tracker.SprintMoveInput{Board: "1", Sprint: "2", Issues: ids("10103")}))
	created, ok := applied(t, "create", p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "New task"})).Created()
	if !ok {
		t.Fatal("the create named no issue")
	}

	second := s.syncBoard(t, scope)
	want := append(ids("10101", "10102", "10103"), created)
	slices.Sort(want)
	if !slices.Equal(second.Cards, want) {
		t.Fatalf("second sync cards %v, want %v", second.Cards, want)
	}
	s.compare(t, scope, s.jiraCards())
	v = must[BoardView](t)(s.svc.Board(scope, StatusGroups{}, []Section{{Kind: SectionSprint, Sprint: "2"}}))
	if got := rowIssues(v.Sections[0].Rows); !slices.Equal(got, []string{"10103"}) {
		t.Fatalf("sprint 2 holds %v", got)
	}
	d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, "10101", Interactive)
	if err != nil || strings.TrimSpace(d.Description) != "first" || d.Card.Status.ID != "10002" {
		t.Fatalf("detail %+v, %v", d, err)
	}
}

// refusedCursorRestarts stores, as scope's cursor, the cursor the mirror
// holds with the version of an older encoding (the provider refuses it,
// wrapping tracker.ErrInvalidInput), then runs a delta: the walk starts
// over with one page and stores a cursor the provider reads, with no
// failure.
func (s *fakeScenario) refusedCursorRestarts(t *testing.T, scope Scope) {
	t.Helper()
	root, _, err := s.st.GetMirrorRoot(scope.Connection)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := s.st.GetMirrorSync(root, string(scope.Board), s.svc.steady().UnixMilli())
	if err != nil || len(st.Cursor) == 0 {
		t.Fatalf("no cursor to age: %+v, %v", st, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(st.Cursor, &fields); err != nil {
		t.Fatal(err)
	}
	fields["v"] = 1
	older := must[[]byte](t)(json.Marshal(fields))
	must[store.MirrorChanges](t)(s.st.WriteMirrorRowPage(root, store.MirrorRowPage{BoardID: string(scope.Board), Lifetime: st.Lifetime, ReadAt: beginReadAt(t, s.st, root, time.Now().UnixMilli())}, older))
	routes := s.requestsOf(t, func() {
		out := must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
		if out.Failure != nil || out.Ended || out.More() {
			t.Fatalf("a delta from a refused cursor: %+v", out)
		}
	})
	if total(routes) != 1 {
		t.Fatalf("the restarted walk made %s", describe(routes))
	}
	st, _, err = s.st.GetMirrorSync(root, string(scope.Board), s.svc.steady().UnixMilli())
	if err != nil || bytes.Equal(st.Cursor, older) || len(st.Cursor) == 0 {
		t.Fatalf("the cursor after the restart %s, %v", st.Cursor, err)
	}
	// The new cursor is one the provider reads: the next delta fails
	// nothing and asks once.
	routes = s.requestsOf(t, func() { tickOK(t, s.svc, scope, BoardDue{Delta: true}) })
	if total(routes) != 1 {
		t.Fatalf("the delta after the restart made %s", describe(routes))
	}
}

func TestFakeRefusedCursorRestartsTheWalk(t *testing.T) {
	t.Parallel()
	t.Run("jira", func(t *testing.T) {
		t.Parallel()
		s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
			return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
		}, "example.atlassian.net")
		conn := s.addJira()
		s.writeToken(conn, "fake-jira-token-1")
		scope := Scope{Connection: conn.ID, Board: "1"}
		s.listBoards(conn.ID, scope.Board)
		s.syncBoard(t, scope)
		s.refusedCursorRestarts(t, scope)
	})
	t.Run("linear", func(t *testing.T) {
		t.Parallel()
		s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
			return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
		})
		conn := s.addLinear()
		s.writeToken(conn, "lin_api_fake00001")
		scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
		s.listBoards(conn.ID, scope.Board)
		s.syncBoard(t, scope)
		s.refusedCursorRestarts(t, scope)
	})
}

// Linear fixture IDs, in Linear's lowercase UUID form.
func lid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

func linearSeed(at time.Time) trackerfake.LinearWorkspace {
	days := func(n int) time.Time { return at.Add(time.Duration(n) * 24 * time.Hour) }
	three := 3.0
	return trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_fake00001", Viewer: lid(1),
		Users: []trackerfake.LinearUser{{ID: lid(1), Name: "User 1", Email: "user1@example.com"}, {ID: lid(2), Name: "User 2", Email: "user2@example.com"}},
		Teams: []trackerfake.LinearTeam{{
			ID: lid(10), Key: "WEB", Name: "Web", Timezone: "UTC", Estimation: "fibonacci",
			CyclesEnabled: true, CycleWeeks: 2, DefaultState: lid(21), Members: []string{lid(1), lid(2)},
		}},
		States: []trackerfake.LinearState{
			{ID: lid(20), Team: lid(10), Name: "Backlog", Type: "backlog", Position: 0},
			{ID: lid(21), Team: lid(10), Name: "Todo", Type: "unstarted", Position: 1},
			{ID: lid(22), Team: lid(10), Name: "In Progress", Type: "started", Position: 2},
			{ID: lid(23), Team: lid(10), Name: "Done", Type: "completed", Position: 3},
		},
		Cycles: []trackerfake.LinearCycle{
			{ID: lid(31), Team: lid(10), Number: 2, StartsAt: days(-10), EndsAt: days(4)},
			{ID: lid(32), Team: lid(10), Number: 3, StartsAt: days(4), EndsAt: days(18)},
		},
		Issues: []trackerfake.LinearIssue{
			{ID: lid(60), Team: lid(10), Number: 1, Title: "Fix login", Description: "Login fails", State: lid(21), Assignee: lid(1), Creator: lid(1),
				Cycle: lid(31), Priority: 2, Estimate: &three, SortOrder: 100, CreatedAt: days(-18), UpdatedAt: days(-1)},
			{ID: lid(61), Team: lid(10), Number: 2, Title: "Add search", State: lid(20), Creator: lid(2), SortOrder: 200, CreatedAt: days(-17), UpdatedAt: days(-2)},
			{ID: lid(62), Team: lid(10), Number: 3, Title: "Login tests", State: lid(22), Creator: lid(1), SortOrder: 300,
				CreatedAt: days(-16), UpdatedAt: days(-3), StartedAt: days(-3)},
		},
	}
}

func (s *fakeScenario) linearCards() []issueCard {
	var out []issueCard
	for _, is := range s.e.Snapshot().Linear[0].Issues {
		out = append(out, issueCard{ID: is.ID, Key: fmt.Sprintf("WEB-%d", int(is.Number)), Title: is.Title, Status: is.State})
	}
	return out
}

func TestFakeLinearScenario(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	})
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
	s.listBoards(conn.ID, scope.Board)

	first := s.syncBoard(t, scope)
	if len(first.Rows) != 3 || len(first.History) != 3 {
		t.Fatalf("first sync %+v", first)
	}
	s.compare(t, scope, s.linearCards())
	v := must[BoardView](t)(s.svc.Board(scope, StatusGroups{}, []Section{{Kind: SectionSprint, Sprint: lid(31)}}))
	if len(v.Columns) != 4 || v.Columns[0].Statuses[0].Name != "Backlog" || len(v.Sections[0].Rows) != 1 {
		t.Fatalf("board %+v", v)
	}
	// Linear names users by ID only: the name comes from the structure.
	if a := v.Sections[0].Rows[0].Card.Assignee; a == nil || a.Name != "User 1" {
		t.Fatalf("the cycle's card %+v", v.Sections[0].Rows[0].Card)
	}

	s.clock.advance(time.Minute)
	p := s.provider(conn.ID)
	applied(t, "edit", p.Edit(t.Context(), tracker.EditInput{Issue: tracker.IssueID(lid(61)), Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Add search, renamed")}}}))
	applied(t, "move", p.Move(t.Context(), tracker.MoveInput{Issue: tracker.IssueID(lid(60)), Move: tracker.MoveID(lid(23))}))

	second := s.syncBoard(t, scope)
	if !slices.Equal(second.Cards, ids(lid(60), lid(61))) {
		t.Fatalf("second sync cards %v", second.Cards)
	}
	s.compare(t, scope, s.linearCards())
	d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tracker.IssueID(lid(60)), Interactive)
	if err != nil || strings.TrimSpace(d.Description) != "Login fails" || d.Card.Status.Name != "Done" {
		t.Fatalf("detail %+v, %v", d, err)
	}
}
