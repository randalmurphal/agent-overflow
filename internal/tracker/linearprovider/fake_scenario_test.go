package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// The scenario runs the Provider over the linear client against the
// stateful fake Linear in internal/harness/trackerfake: it lists and
// reads the board and tickets, makes every write Linear supports and
// reads back what each changed, and checks that a delta read returns
// exactly the issues the writes changed.

type scenarioClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *scenarioClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *scenarioClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sid is a fixture ID in Linear's lowercase UUID form.
func sid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

var (
	scUser1, scUser2                     = sid(1), sid(2)
	scTeam                               = sid(10)
	scBacklog, scTodo, scStarted         = sid(20), sid(21), sid(22)
	scDone                               = sid(23)
	scActive, scUpcoming                 = sid(31), sid(32)
	scBug                                = sid(40)
	scIssue1, scIssue2, scIssue3         = sid(60), sid(61), sid(62)
	scComment                            = sid(70)
	scStart                              = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	scThree                      float64 = 3
)

func scenarioWorkspace() trackerfake.LinearWorkspace {
	at := func(days int) time.Time { return scStart.Add(time.Duration(days) * 24 * time.Hour) }
	return trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_scenario1", Viewer: scUser1,
		Users: []trackerfake.LinearUser{
			{ID: scUser1, Name: "User 1", Email: "user1@example.com"},
			{ID: scUser2, Name: "User 2", Email: "user2@example.com"},
		},
		Teams: []trackerfake.LinearTeam{{
			ID: scTeam, Key: "WEB", Name: "Web", Timezone: "UTC", Estimation: "fibonacci",
			CyclesEnabled: true, CycleWeeks: 2, DefaultState: scTodo, Members: []string{scUser1, scUser2},
		}},
		States: []trackerfake.LinearState{
			{ID: scBacklog, Team: scTeam, Name: "Backlog", Type: "backlog", Position: 0},
			{ID: scTodo, Team: scTeam, Name: "Todo", Type: "unstarted", Position: 1},
			{ID: scStarted, Team: scTeam, Name: "In Progress", Type: "started", Position: 2},
			{ID: scDone, Team: scTeam, Name: "Done", Type: "completed", Position: 3},
		},
		Cycles: []trackerfake.LinearCycle{
			{ID: scActive, Team: scTeam, Number: 2, StartsAt: at(-10), EndsAt: at(4)},
			{ID: scUpcoming, Team: scTeam, Number: 3, StartsAt: at(4), EndsAt: at(18)},
		},
		Labels: []trackerfake.LinearLabel{{ID: scBug, Name: "Bug", Team: scTeam}},
		Issues: []trackerfake.LinearIssue{
			{ID: scIssue1, Team: scTeam, Number: 1, Title: "Fix login", Description: "Login fails", State: scTodo, Assignee: scUser1, Creator: scUser1,
				Cycle: scActive, Labels: []string{scBug}, Priority: 2, Estimate: &scThree, SortOrder: 100, CreatedAt: at(-18), UpdatedAt: at(-1)},
			{ID: scIssue2, Team: scTeam, Number: 2, Title: "Add search", State: scBacklog, Creator: scUser2, SortOrder: 200, CreatedAt: at(-17), UpdatedAt: at(-2)},
			{ID: scIssue3, Team: scTeam, Number: 3, Title: "Login tests", State: scStarted, Creator: scUser1, SortOrder: 300,
				CreatedAt: at(-16), UpdatedAt: at(-3), StartedAt: at(-3), FormerKeys: []string{"OPS-7"}},
		},
		Comments: []trackerfake.LinearComment{{ID: scComment, Issue: scIssue1, User: scUser2, Body: "Looks good", CreatedAt: at(-2)}},
	}
}

type scenario struct {
	e     *trackerfake.Engine
	c     *linear.Client
	p     *Provider
	clock *scenarioClock
	ctx   context.Context
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	clock := &scenarioClock{now: scStart}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{scenarioWorkspace()}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := linear.New(linear.Options{
		BaseURL: srv.LinearURL(), Version: "test", Workspace: "example",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte("lin_api_scenario1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s: %s", r.Operation, r.Detail)
		}
	})
	return &scenario{e: e, c: c, p: p, clock: clock, ctx: context.Background()}
}

func scenarioMust[T any](t *testing.T) func(v T, err error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// allApplied fails the test unless every part of r was applied.
func allApplied(t *testing.T, what string, r tracker.Result) tracker.Result {
	t.Helper()
	if !r.AllApplied() {
		var b strings.Builder
		for _, p := range r.Parts {
			b.WriteString(" [" + p.Step.String() + " " + string(p.Issue) + " " + p.Outcome.State().String())
			if err := p.Outcome.Err(); err != nil {
				b.WriteString(": " + err.Error())
			}
			b.WriteString("]")
		}
		t.Fatalf("%s:%s", what, b.String())
	}
	return r
}

func rowIDs(rs []tracker.Row) []tracker.IssueID {
	out := make([]tracker.IssueID, len(rs))
	for i, r := range rs {
		out[i] = r.Card.ID
	}
	slices.Sort(out)
	return out
}

func (s *scenario) detail(t *testing.T, id string) tracker.Detail {
	t.Helper()
	return scenarioMust[tracker.Detail](t)(s.p.Detail(s.ctx, tracker.IssueID(id)))
}

func (s *scenario) row(t *testing.T, id string) tracker.Row {
	t.Helper()
	rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, tracker.BoardID(scTeam), []tracker.IssueID{tracker.IssueID(id)}, tracker.Eligibility{}))
	if len(rows.Rows) != 1 {
		t.Fatalf("rows of %s: %+v", id, rows)
	}
	return rows.Rows[0]
}

// drain reads ChangedRows from cursor to the end of the walk.
func (s *scenario) drain(t *testing.T, from tracker.Cursor, start time.Time) ([]tracker.Row, tracker.Cursor) {
	t.Helper()
	var rows []tracker.Row
	for {
		page := scenarioMust[tracker.RowPage](t)(s.p.ChangedRows(s.ctx, tracker.DeltaQuery{Board: tracker.BoardID(scTeam), From: from, Start: start}))
		rows = append(rows, page.Rows...)
		from = page.Next
		if !page.More {
			return rows, from
		}
	}
}

func TestFakeScenario(t *testing.T) {
	s := newScenario(t)
	board := tracker.BoardID(scTeam)
	window := scStart.Add(-30 * 24 * time.Hour)

	// Reads: who, where, the board, a ticket.
	if u := scenarioMust[tracker.User](t)(s.p.Identity(s.ctx)); u.ID != tracker.UserID(scUser1) {
		t.Fatalf("identity %+v", u)
	}
	if ps := scenarioMust[[]tracker.Project](t)(s.p.Projects(s.ctx)); len(ps) != 1 || ps[0].Key != "WEB" {
		t.Fatalf("projects %+v", ps)
	}
	boards := scenarioMust[[]tracker.Board](t)(s.p.Boards(s.ctx, tracker.ProjectID(scTeam)))
	if len(boards) != 1 || boards[0].ID != board || !boards[0].Sprints {
		t.Fatalf("boards %+v", boards)
	}
	st := scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, board, window))
	if len(st.Columns) != 4 || len(st.Sprints) != 2 || st.Estimate.Unit != tracker.UnitPoints {
		t.Fatalf("structure %+v", st)
	}
	if lts := scenarioMust[[]tracker.LinkType](t)(s.p.LinkTypes(s.ctx)); len(lts) == 0 {
		t.Fatalf("link types %+v", lts)
	}
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, board, window))
	slices.Sort(listing.IDs)
	if !listing.Complete || !slices.Equal(listing.IDs, []tracker.IssueID{tracker.IssueID(scIssue1), tracker.IssueID(scIssue2), tracker.IssueID(scIssue3)}) {
		t.Fatalf("listing %+v", listing)
	}
	if r := s.row(t, scIssue1); r.Estimate == nil || *r.Estimate != 3 || r.Card.Key != "WEB-1" || !slices.Equal(r.Card.Sprints, []tracker.SprintID{tracker.SprintID(scActive)}) {
		t.Fatalf("row %+v", r)
	}
	if cs := scenarioMust[tracker.Cards](t)(s.p.Cards(s.ctx, []tracker.IssueID{tracker.IssueID(scIssue2)})); len(cs.Cards) != 1 || cs.Cards[0].Key != "WEB-2" {
		t.Fatalf("cards %+v", cs)
	}
	d := s.detail(t, scIssue1)
	if d.Card.Key != "WEB-1" || strings.TrimSpace(d.Description) != "Login fails" || len(d.Comments.Comments) != 1 || len(d.Moves) != 3 {
		t.Fatalf("detail %+v", d)
	}
	// A page of comments names its authors, whatever the mirror holds.
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, tracker.IssueID(scIssue1), "")); len(cs.Comments) != 1 ||
		len(cs.Users) != 1 || cs.Users[0].ID != tracker.UserID(scUser2) || cs.Users[0].Name != "User 2" {
		t.Fatalf("comments %+v", cs)
	}
	// An identifier an issue had before it moved team still finds it.
	if l := scenarioMust[tracker.KeyLookup](t)(s.p.Lookup(s.ctx, []string{"WEB-2", "ops-7", "WEB-99"})); len(l.Found) != 2 ||
		l.Found[1].Key != "ops-7" || l.Found[1].Card.Key != "WEB-3" || !slices.Equal(l.Unmatched, []string{"WEB-99"}) {
		t.Fatalf("lookup %+v", l)
	}
	if sp := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, tracker.SearchQuery{Text: "login", Limit: 10})); len(sp.Cards) != 2 {
		t.Fatalf("search %+v", sp)
	}
	if fs := scenarioMust[[]tracker.CreateField](t)(s.p.CreateFields(s.ctx, tracker.ProjectID(scTeam), "")); len(fs) == 0 {
		t.Fatalf("create fields %+v", fs)
	}
	if us := scenarioMust[tracker.UserList](t)(s.p.AssignableUsers(s.ctx, tracker.OnIssue(tracker.IssueID(scIssue1)), "User")); len(us.Users) != 2 {
		t.Fatalf("assignable %+v", us)
	}
	if cs := scenarioMust[tracker.ChoiceList](t)(s.p.Choices(s.ctx, tracker.OnIssue(tracker.IssueID(scIssue1)), tracker.FieldLabels, "")); len(cs.Choices) != 1 {
		t.Fatalf("label choices %+v", cs)
	}

	first, cursor := s.drain(t, nil, window)
	if len(first) != 3 {
		t.Fatalf("first walk %v", rowIDs(first))
	}
	s.clock.advance(time.Minute)

	// Writes, each read back.
	created := allApplied(t, "create", s.p.Create(s.ctx, tracker.CreateInput{Project: tracker.ProjectID(scTeam), Title: "New page",
		Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("Body text", "")}}}))
	newID, newKey, ok := created.Created()
	if !ok || newKey != "WEB-4" {
		t.Fatalf("create named issue %v, key %q", ok, newKey)
	}
	if d := s.detail(t, string(newID)); d.Card.Title != "New page" || d.Card.Key != "WEB-4" || strings.TrimSpace(d.Description) != "Body text" ||
		len(d.Card.Sprints) != 0 {
		t.Fatalf("created %+v", d.Card)
	}
	inCycle := allApplied(t, "create in a cycle", s.p.Create(s.ctx, tracker.CreateInput{Project: tracker.ProjectID(scTeam), Title: "Cycle page",
		Board: board, Sprint: tracker.SprintID(scUpcoming)}))
	cycleID, _, _ := inCycle.Created()
	if len(inCycle.Parts) != 1 || !slices.Equal(s.detail(t, string(cycleID)).Card.Sprints, []tracker.SprintID{tracker.SprintID(scUpcoming)}) {
		t.Fatalf("created in a cycle: %+v", inCycle.Parts)
	}
	// A create after a row lands just after it in the team's manual order,
	// where Linear's default puts it above every issue (cycleID's).
	foot := s.row(t, scIssue1)
	atFoot := allApplied(t, "create at a cycle's foot", s.p.Create(s.ctx, tracker.CreateInput{Project: tracker.ProjectID(scTeam), Title: "Foot page",
		Board: board, Sprint: tracker.SprintID(scActive), After: tracker.RowPlace{Issue: tracker.IssueID(scIssue1), Order: foot.Order}}))
	footID, _, _ := atFoot.Created()
	if got, next := s.row(t, string(footID)).Order, s.row(t, scIssue2).Order; got <= foot.Order || got >= next ||
		s.row(t, string(cycleID)).Order >= foot.Order {
		t.Fatalf("created after %s: %s, before %s; the default create %s", foot.Order, got, next, s.row(t, string(cycleID)).Order)
	}
	if res := s.p.Create(s.ctx, tracker.CreateInput{Project: tracker.ProjectID(scTeam), Title: "Bad", Board: board,
		After: tracker.RowPlace{Issue: tracker.IssueID(scIssue1), Order: "0|i00000:"}}); len(res.Parts) != 1 || refusalReason(res.Parts[0]) != tracker.ReasonInvalid {
		t.Fatalf("create after a rank Linear never made: %+v", res.Parts)
	}
	if sp := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, tracker.SearchQuery{Text: "Bad", Limit: 10})); len(sp.Cards) != 0 {
		t.Fatalf("the refused create made %+v", sp.Cards)
	}

	allApplied(t, "edit", s.p.Edit(s.ctx, tracker.EditInput{Issue: tracker.IssueID(scIssue2), Fields: []tracker.FieldEdit{
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("Add site search")},
		{Field: tracker.FieldAssignee, Value: tracker.PickValues(scUser2)},
		{Field: tracker.FieldLabels, Value: tracker.PickValues(scBug)},
	}}))
	if c := s.detail(t, scIssue2).Card; c.Title != "Add site search" || c.Assignee != tracker.UserID(scUser2) || len(c.Labels) != 1 {
		t.Fatalf("edited %+v", c)
	}

	allApplied(t, "move", s.p.Move(s.ctx, tracker.MoveInput{Issue: tracker.IssueID(scIssue1), Move: tracker.MoveID(scDone)}))
	if c := s.detail(t, scIssue1).Card; c.Status != tracker.StatusID(scDone) || c.Resolved.IsZero() {
		t.Fatalf("moved %+v", c)
	}

	five := 5.0
	allApplied(t, "estimate", s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: board, Issue: tracker.IssueID(scIssue2), Value: &five}))
	if r := s.row(t, scIssue2); r.Estimate == nil || *r.Estimate != 5 {
		t.Fatalf("estimated %+v", r)
	}

	allApplied(t, "rank", s.p.Rank(s.ctx, tracker.RankInput{Board: board, Issues: []tracker.IssueID{tracker.IssueID(scIssue3)}, Next: tracker.IssueID(scIssue1)}))
	if r3, r1 := s.row(t, scIssue3), s.row(t, scIssue1); r3.Order >= r1.Order {
		t.Fatalf("ranked %s not before %s", r3.Order, r1.Order)
	}

	allApplied(t, "sprint move", s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: board, Sprint: tracker.SprintID(scUpcoming), Issues: []tracker.IssueID{tracker.IssueID(scIssue3)}}))
	if c := s.row(t, scIssue3).Card; !slices.Equal(c.Sprints, []tracker.SprintID{tracker.SprintID(scUpcoming)}) {
		t.Fatalf("sprint move %+v", c)
	}
	// A move to a place: the cycle and the place between its neighbors,
	// in one update.
	placed := allApplied(t, "placed sprint move", s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: board, Sprint: tracker.SprintID(scUpcoming),
		Issues: []tracker.IssueID{tracker.IssueID(scIssue2)}, Prev: tracker.IssueID(scIssue3), Next: tracker.IssueID(scIssue1)}))
	if got := len(placed.Parts); got != 1 || placed.Parts[0].Step != tracker.StepSprintMove {
		t.Fatalf("placed sprint move parts %+v", placed.Parts)
	}
	if r3, r2, r1 := s.row(t, scIssue3), s.row(t, scIssue2), s.row(t, scIssue1); !slices.Equal(r2.Card.Sprints, []tracker.SprintID{tracker.SprintID(scUpcoming)}) ||
		r3.Order >= r2.Order || r2.Order >= r1.Order {
		t.Fatalf("placed %s between %s and %s, sprints %v", r2.Order, r3.Order, r1.Order, r2.Card.Sprints)
	}

	// A place whose neighbors no longer stand in that order is refused
	// before anything is written: the issue stays in its cycle.
	stale := s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: board, Sprint: tracker.SprintID(scActive),
		Issues: []tracker.IssueID{tracker.IssueID(scIssue2)}, Prev: tracker.IssueID(scIssue1), Next: tracker.IssueID(scIssue3)})
	if r, ok := stale.Parts[0].Outcome.Refusal(); len(stale.Parts) != 1 || !ok || r.Reason != tracker.ReasonStale {
		t.Fatalf("stale placed move %+v", stale.Parts)
	}
	if c := s.row(t, scIssue2).Card; !slices.Equal(c.Sprints, []tracker.SprintID{tracker.SprintID(scUpcoming)}) {
		t.Fatalf("a refused placed move changed the cycle: %v", c.Sprints)
	}

	added := s.p.AddComment(s.ctx, tracker.CommentInput{Issue: tracker.IssueID(scIssue2), Markdown: "New comment"})
	allApplied(t, "comment", added)
	cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, tracker.IssueID(scIssue2), ""))
	if len(cs.Comments) != 1 || strings.TrimSpace(cs.Comments[0].Body) != "New comment" {
		t.Fatalf("comments %+v", cs)
	}
	// The applied part names the comment it made.
	if got := added.Parts[0].Comment; got == "" || got != cs.Comments[0].ID {
		t.Fatalf("the comment part names %q, the comment is %q", got, cs.Comments[0].ID)
	}
	allApplied(t, "comment edit", s.p.EditComment(s.ctx, tracker.CommentEdit{Issue: tracker.IssueID(scIssue2), Comment: cs.Comments[0].ID, Markdown: "Edited comment", From: cs.Comments[0].Body}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, tracker.IssueID(scIssue2), "")); strings.TrimSpace(cs.Comments[0].Body) != "Edited comment" {
		t.Fatalf("edited comment %+v", cs)
	}
	allApplied(t, "comment delete", s.p.DeleteComment(s.ctx, tracker.CommentDelete{Issue: tracker.IssueID(scIssue2), Comment: cs.Comments[0].ID}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, tracker.IssueID(scIssue2), "")); len(cs.Comments) != 0 {
		t.Fatalf("deleted comment %+v", cs)
	}

	allApplied(t, "link", s.p.Link(s.ctx, tracker.LinkInput{Type: "blocks", Outward: tracker.IssueID(scIssue2), Inward: newID}))
	links := s.detail(t, scIssue2).Links
	if len(links) != 1 || links[0].Issue.ID != newID || !links[0].Outward {
		t.Fatalf("links %+v", links)
	}
	allApplied(t, "unlink", s.p.Unlink(s.ctx, tracker.UnlinkInput{Issue: tracker.IssueID(scIssue2), Link: links[0].ID, Other: newID}))
	if links := s.detail(t, string(newID)).Links; len(links) != 0 {
		t.Fatalf("unlinked %+v", links)
	}

	// Writes Linear lacks are refused before any request.
	before := len(s.e.Requests(0).Requests)
	for name, r := range map[string]tracker.Result{
		"sprint create": s.p.CreateSprint(s.ctx, tracker.SprintInput{Board: board, Name: "Cycle"}),
		"attach":        s.p.Attach(s.ctx, tracker.AttachInput{Issue: tracker.IssueID(scIssue2), Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("a"), Size: 1}}}),
	} {
		if len(r.Parts) == 0 || !errors.Is(r.Parts[0].Outcome.Err(), tracker.ErrUnsupported) {
			t.Fatalf("%s was not refused as unsupported: %+v", name, r)
		}
	}
	if after := len(s.e.Requests(0).Requests); after != before {
		t.Fatalf("unsupported writes sent %d requests", after-before)
	}

	// History shows the status, cycle, estimate and assignee changes.
	h := scenarioMust[tracker.History](t)(s.p.History(s.ctx, tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(scIssue1), tracker.IssueID(scIssue2), tracker.IssueID(scIssue3)}, Boards: []tracker.BoardID{board}}))
	byIssue := map[tracker.IssueID]tracker.IssueHistory{}
	for _, ih := range h.Issues {
		byIssue[ih.Issue] = ih
	}
	if st := byIssue[tracker.IssueID(scIssue1)].Status; len(st) != 1 || st[0].To != tracker.StatusID(scDone) {
		t.Fatalf("status history %+v", st)
	}
	if as := byIssue[tracker.IssueID(scIssue2)].Assignee; len(as) != 1 || as[0].To != tracker.UserID(scUser2) {
		t.Fatalf("assignee history %+v", as)
	}
	if es := byIssue[tracker.IssueID(scIssue2)].Estimate; len(es) != 1 || es[0].To == nil || *es[0].To != 5 {
		t.Fatalf("estimate history %+v", es)
	}
	if sp := byIssue[tracker.IssueID(scIssue3)].Sprint; len(sp) != 1 || !slices.Equal(sp[0].To, []tracker.SprintID{tracker.SprintID(scUpcoming)}) {
		t.Fatalf("sprint history %+v", sp)
	}

	// The delta from the first walk's cursor is exactly the issues the
	// writes changed.
	delta, _ := s.drain(t, cursor, window)
	want := []tracker.IssueID{tracker.IssueID(scIssue1), tracker.IssueID(scIssue2), tracker.IssueID(scIssue3), newID, cycleID, footID}
	slices.Sort(want)
	if !slices.Equal(rowIDs(delta), want) {
		t.Fatalf("delta %v, want %v", rowIDs(delta), want)
	}
}

// refusalReason is a refused part's reason, or "" for any other part.
func refusalReason(p tracker.Part) tracker.Reason {
	r, _ := p.Outcome.Refusal()
	return r.Reason
}
