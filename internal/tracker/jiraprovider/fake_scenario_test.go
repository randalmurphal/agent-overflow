package jiraprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// The scenario runs the Provider over the jira and tempo clients against
// the stateful fake Jira and Tempo in internal/harness/trackerfake: it
// lists and reads boards and tickets, makes every write and reads back
// what each changed, and checks that a delta read returns exactly the
// issues the writes changed.

const scenarioSite = "example.atlassian.net"

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

func scenarioDoc(text string) json.RawMessage {
	return json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
}

func scenarioFixture(start time.Time) trackerfake.JiraSite {
	day := start.Add(-48 * time.Hour)
	return trackerfake.JiraSite{
		Site: scenarioSite, Account: "acc-user1", Token: "scenario-jira-1",
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
			{ID: "10101", Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "First story", Description: scenarioDoc("first"), Sprints: []int64{1},
				Assignee: "acc-user1", Reporter: "acc-user1", Priority: "3", Status: "10001", Created: day, Updated: day,
				Values:   map[string]json.RawMessage{"customfield_10016": json.RawMessage(`5`)},
				Comments: []trackerfake.JiraComment{{ID: "40001", Author: "acc-user2", Body: scenarioDoc("a comment"), Created: day}},
				Changelog: []trackerfake.JiraChange{{Author: "acc-user1", Created: day.Add(time.Hour), Items: []trackerfake.JiraChangeItem{
					{FieldID: "status", Field: "status", FieldType: "jira", From: scenarioPtr("10000"), To: scenarioPtr("10001"), FromString: scenarioPtr("To Do"), ToString: scenarioPtr("In Progress")}}}}},
			{ID: "10102", Key: "WEB-2", Project: "WEB", Type: "10002", Summary: "Second task", Sprints: []int64{1}, Created: day, Updated: day},
			{ID: "10103", Key: "WEB-3", Project: "WEB", Type: "10002", Summary: "Backlog task", Created: day, Updated: day},
		},
		Tempo: &trackerfake.TempoAccount{
			Token: "scenario-tempo-1",
			Worklogs: []trackerfake.TempoWorklog{{ID: 7001, Issue: "10101", Author: "acc-user1", StartDate: day.Format(time.DateOnly), StartTime: "09:00:00",
				Spent: 3600, Description: "Work"}},
		},
	}
}

func scenarioPtr(s string) *string { return &s }

type scenario struct {
	e     *trackerfake.Engine
	p     *Provider
	clock *scenarioClock
	ctx   context.Context
}

func newScenario(t *testing.T, withTempo bool) *scenario {
	t.Helper()
	return newObservedScenario(t, withTempo, nil)
}

// newObservedScenario is newScenario whose fake reports each request it
// records to onRequest (trackerfake.Options.OnRequest).
func newObservedScenario(t *testing.T, withTempo bool, onRequest func(trackerfake.Request)) *scenario {
	t.Helper()
	clock := &scenarioClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now, OnRequest: onRequest})
	t.Cleanup(e.Close)
	site := scenarioFixture(clock.Now())
	if !withTempo {
		site.Tempo = nil
	}
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, scenarioSite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	jc, err := jira.New(jira.Options{
		BaseURL: srv.JiraURL(scenarioSite), Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte("scenario-jira-1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jc.Close)
	opts := Options{Jira: jc, Site: "https://" + scenarioSite}
	if withTempo {
		tc, err := tempo.New(tempo.Options{
			BaseURL: srv.TempoURL(), Version: "test", Issues: TempoIssues(jc),
			Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte("scenario-tempo-1")))),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tc.Close)
		opts.Tempo = tc
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s %s: %s", r.Method, r.Path, r.Detail)
		}
	})
	return &scenario{e: e, p: p, clock: clock, ctx: context.Background()}
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

func scenarioMust[T any](t *testing.T) func(v T, err error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func cardIDs(cs []tracker.Card) []tracker.IssueID {
	out := make([]tracker.IssueID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func rowIDs(rs []tracker.Row) []tracker.IssueID {
	out := make([]tracker.IssueID, len(rs))
	for i, r := range rs {
		out[i] = r.Card.ID
	}
	slices.Sort(out)
	return out
}

func (s *scenario) detail(t *testing.T, id tracker.IssueID) tracker.Detail {
	t.Helper()
	return scenarioMust[tracker.Detail](t)(s.p.Detail(s.ctx, id))
}

func (s *scenario) row(t *testing.T, id tracker.IssueID) tracker.Row {
	t.Helper()
	rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, "1", []tracker.IssueID{id}))
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
		page := scenarioMust[tracker.RowPage](t)(s.p.ChangedRows(s.ctx, tracker.DeltaQuery{Board: "1", From: from, Start: start}))
		rows = append(rows, page.Rows...)
		from = page.Next
		if !page.More {
			return rows, from
		}
	}
}

func TestFakeScenario(t *testing.T) {
	s := newScenario(t, true)
	t0 := s.clock.Now()
	window := t0.Add(-30 * 24 * time.Hour)

	// Reads: who, where, which boards, a board, a ticket.
	if u := scenarioMust[tracker.User](t)(s.p.Identity(s.ctx)); u.ID != "acc-user1" {
		t.Fatalf("identity %+v", u)
	}
	if ps := scenarioMust[[]tracker.Project](t)(s.p.Projects(s.ctx)); len(ps) != 1 || ps[0].Key != "WEB" {
		t.Fatalf("projects %+v", ps)
	}
	if ts := scenarioMust[[]tracker.IssueType](t)(s.p.IssueTypes(s.ctx, "10000")); len(ts) < 3 {
		t.Fatalf("issue types %+v", ts)
	}
	boards := scenarioMust[[]tracker.Board](t)(s.p.Boards(s.ctx, "10000"))
	if len(boards) != 1 || boards[0].ID != "1" || !boards[0].Sprints {
		t.Fatalf("boards %+v", boards)
	}
	st := scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	if len(st.Columns) != 3 || len(st.Sprints) != 2 || st.Estimate.Unit != tracker.UnitPoints {
		t.Fatalf("structure %+v", st)
	}
	if lts := scenarioMust[[]tracker.LinkType](t)(s.p.LinkTypes(s.ctx)); len(lts) != 2 {
		t.Fatalf("link types %+v", lts)
	}
	listing := scenarioMust[tracker.Listing](t)(s.p.BoardIssues(s.ctx, "1", window))
	slices.Sort(listing.IDs)
	if !listing.Complete || !slices.Equal(listing.IDs, []tracker.IssueID{"10101", "10102", "10103"}) {
		t.Fatalf("listing %+v", listing)
	}
	rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, "1", listing.IDs))
	if len(rows.Rows) != 3 || len(rows.Missing) != 0 {
		t.Fatalf("rows %+v", rows)
	}
	if r := s.row(t, "10101"); r.Estimate == nil || *r.Estimate != 5 || r.Card.Status != "10001" || !slices.Equal(r.Card.Sprints, []tracker.SprintID{"1"}) {
		t.Fatalf("row %+v", r)
	}
	cards := scenarioMust[tracker.Cards](t)(s.p.Cards(s.ctx, []tracker.IssueID{"10102", "10199"}))
	if !slices.Equal(cardIDs(cards.Cards), []tracker.IssueID{"10102"}) || !slices.Equal(cards.Missing, []tracker.IssueID{"10199"}) {
		t.Fatalf("cards %+v", cards)
	}
	d := s.detail(t, "10101")
	if d.Card.Key != "WEB-1" || strings.TrimSpace(d.Description) != "first" || len(d.Comments.Comments) != 1 || len(d.History) == 0 || len(d.Moves) == 0 ||
		d.URL != "https://"+scenarioSite+"/browse/WEB-1" {
		t.Fatalf("detail %+v", d)
	}
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10101", "")); len(cs.Comments) != 1 {
		t.Fatalf("comments %+v", cs)
	}
	if l := scenarioMust[tracker.KeyLookup](t)(s.p.Lookup(s.ctx, []string{"WEB-2", "WEB-99"})); len(l.Found) != 1 || !slices.Equal(l.Unmatched, []string{"WEB-99"}) {
		t.Fatalf("lookup %+v", l)
	}
	if sp := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, tracker.SearchQuery{Text: "task", Project: "10000", Limit: 10})); len(sp.Cards) != 2 {
		t.Fatalf("search %+v", sp)
	}
	if fs := scenarioMust[[]tracker.Field](t)(s.p.CreateFields(s.ctx, "10000", "10001")); len(fs) == 0 {
		t.Fatalf("create fields %+v", fs)
	}
	if us := scenarioMust[tracker.UserList](t)(s.p.AssignableUsers(s.ctx, tracker.OnIssue("10101"), "User")); len(us.Users) != 2 {
		t.Fatalf("assignable %+v", us)
	}
	if cs := scenarioMust[tracker.ChoiceList](t)(s.p.Choices(s.ctx, tracker.OnIssue("10101"), tracker.FieldPriority, "")); len(cs.Choices) == 0 {
		t.Fatalf("priority choices %+v", cs)
	}

	// The first delta walk delivers every issue on the board; the cursor
	// it ends with sees only what changes after it.
	first, cursor := s.drain(t, nil, window)
	if !slices.Equal(rowIDs(first), []tracker.IssueID{"10101", "10102", "10103"}) {
		t.Fatalf("first walk %v", rowIDs(first))
	}
	s.clock.advance(time.Minute)

	// Writes, each read back.
	created := allApplied(t, "create", s.p.Create(s.ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "Created task",
		Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("Body text", "")}}}))
	newID, ok := created.Created()
	if !ok {
		t.Fatal("create named no issue")
	}
	if d := s.detail(t, newID); d.Card.Title != "Created task" || strings.TrimSpace(d.Description) != "Body text" {
		t.Fatalf("created %+v", d.Card)
	}

	allApplied(t, "edit", s.p.Edit(s.ctx, tracker.EditInput{Issue: "10102", Fields: []tracker.FieldEdit{
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")},
		{Field: tracker.FieldAssignee, Value: tracker.PickValues("acc-user2")},
		{Field: tracker.FieldLabels, Value: tracker.TypedValues("frontend")},
	}}))
	if c := s.detail(t, "10102").Card; c.Title != "Second task, renamed" || c.Assignee != "acc-user2" || len(c.Labels) != 1 {
		t.Fatalf("edited %+v", c)
	}

	moves := scenarioMust[[]tracker.Move](t)(s.p.Moves(s.ctx, "10101"))
	done := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == "10002" })
	if done < 0 {
		t.Fatalf("moves %+v", moves)
	}
	allApplied(t, "move", s.p.Move(s.ctx, tracker.MoveInput{Issue: "10101", Move: moves[done].ID}))
	if c := s.detail(t, "10101").Card; c.Status != "10002" || c.Resolved.IsZero() {
		t.Fatalf("moved %+v", c)
	}

	eight := 8.0
	allApplied(t, "estimate", s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: "1", Issue: "10102", Value: &eight}))
	if r := s.row(t, "10102"); r.Estimate == nil || *r.Estimate != 8 {
		t.Fatalf("estimated %+v", r)
	}

	allApplied(t, "rank", s.p.Rank(s.ctx, tracker.RankInput{Board: "1", Issues: []tracker.IssueID{"10102"}, Next: "10101"}))
	if r2, r1 := s.row(t, "10102"), s.row(t, "10101"); r2.Order >= r1.Order {
		t.Fatalf("ranked %s not before %s", r2.Order, r1.Order)
	}

	allApplied(t, "sprint move", s.p.MoveToSprint(s.ctx, tracker.SprintMoveInput{Board: "1", Sprint: "2", Issues: []tracker.IssueID{"10103"}}))
	if c := s.row(t, "10103").Card; !slices.Equal(c.Sprints, []tracker.SprintID{"2"}) {
		t.Fatalf("sprint move %+v", c)
	}

	allApplied(t, "comment", s.p.AddComment(s.ctx, tracker.CommentInput{Issue: "10102", Markdown: "New comment"}))
	cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", ""))
	if len(cs.Comments) != 1 || strings.TrimSpace(cs.Comments[0].Body) != "New comment" {
		t.Fatalf("comments %+v", cs)
	}
	allApplied(t, "comment edit", s.p.EditComment(s.ctx, tracker.CommentEdit{Issue: "10102", Comment: cs.Comments[0].ID, Markdown: "Edited comment", From: cs.Comments[0].Body}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", "")); strings.TrimSpace(cs.Comments[0].Body) != "Edited comment" {
		t.Fatalf("edited comment %+v", cs)
	}
	allApplied(t, "comment delete", s.p.DeleteComment(s.ctx, tracker.CommentDelete{Issue: "10102", Comment: cs.Comments[0].ID}))
	if cs := scenarioMust[tracker.CommentPage](t)(s.p.Comments(s.ctx, "10102", "")); len(cs.Comments) != 0 {
		t.Fatalf("deleted comment %+v", cs)
	}

	allApplied(t, "link", s.p.Link(s.ctx, tracker.LinkInput{Type: "10000", Outward: "10102", Inward: "10103"}))
	links := s.detail(t, "10102").Links
	if len(links) != 1 || links[0].Issue.ID != "10103" || !links[0].Outward {
		t.Fatalf("links %+v", links)
	}
	allApplied(t, "unlink", s.p.Unlink(s.ctx, tracker.UnlinkInput{Issue: "10102", Link: links[0].ID, Other: "10103"}))
	if links := s.detail(t, "10103").Links; len(links) != 0 {
		t.Fatalf("unlinked %+v", links)
	}

	allApplied(t, "attach", s.p.Attach(s.ctx, tracker.AttachInput{Issue: "10102", Files: []tracker.File{{Name: "notes.txt", Content: strings.NewReader("hello")}}}))
	atts := s.detail(t, "10102").Attachments
	if len(atts) != 1 || atts[0].Name != "notes.txt" {
		t.Fatalf("attachments %+v", atts)
	}
	var got bytes.Buffer
	if err := s.p.Download(s.ctx, tracker.DownloadInput{Issue: "10102", Attachment: atts[0].ID, To: &got, Limit: 1 << 20}); err != nil || got.String() != "hello" {
		t.Fatalf("download %q, %v", got.String(), err)
	}

	// Sprints: create, edit, complete the active one into the new one,
	// start the new one.
	allApplied(t, "sprint create", s.p.CreateSprint(s.ctx, tracker.SprintInput{Board: "1", Name: "Sprint 3", Goal: "Ship"}))
	st = scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	i3 := slices.IndexFunc(st.Sprints, func(sp tracker.Sprint) bool { return sp.Name == "Sprint 3" })
	if i3 < 0 || st.Sprints[i3].State != tracker.SprintFuture || st.Sprints[i3].Goal != "Ship" {
		t.Fatalf("sprints %+v", st.Sprints)
	}
	sprint3 := st.Sprints[i3].ID
	renamed := "Sprint 3, renamed"
	allApplied(t, "sprint edit", s.p.EditSprint(s.ctx, tracker.SprintEdit{Board: "1", Sprint: sprint3, Name: &renamed}))
	allApplied(t, "sprint complete", s.p.CompleteSprint(s.ctx, tracker.SprintCompletion{Board: "1", Sprint: "1", MoveTo: sprint3}))
	if c := s.row(t, "10102").Card; !slices.Equal(c.Sprints, []tracker.SprintID{sprint3}) {
		t.Fatalf("an unfinished issue did not move: %+v", c)
	}
	start := s.clock.Now()
	allApplied(t, "sprint start", s.p.StartSprint(s.ctx, tracker.SprintStart{Board: "1", Sprint: sprint3, Start: start, End: start.Add(14 * 24 * time.Hour)}))
	st = scenarioMust[tracker.Structure](t)(s.p.Structure(s.ctx, "1", window))
	states := map[tracker.SprintID]tracker.SprintState{}
	for _, sp := range st.Sprints {
		states[sp.ID] = sp.State
	}
	if states["1"] != tracker.SprintClosed || states[sprint3] != tracker.SprintActive {
		t.Fatalf("sprints %+v", st.Sprints)
	}

	// Logged time through Tempo.
	tt := s.p.Time()
	today := scenarioMust[tracker.Date](t)(tracker.DateOf(t0))
	allApplied(t, "worklog add", tt.AddWorklog(s.ctx, tracker.WorklogInput{Issue: "10102", Day: today, Spent: 30 * time.Minute, Description: "Review"}))
	wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}))
	if len(wls) != 1 || wls[0].Issue != "10102" || wls[0].Spent != 30*time.Minute {
		t.Fatalf("worklogs %+v", wls)
	}
	hour := time.Hour
	allApplied(t, "worklog edit", tt.EditWorklog(s.ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}))
	wls = scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}))
	if len(wls) != 1 || wls[0].Spent != time.Hour {
		t.Fatalf("edited worklogs %+v", wls)
	}
	allApplied(t, "worklog delete", tt.DeleteWorklog(s.ctx, tracker.WorklogDelete{From: wls[0]}))
	if wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, tracker.WorklogQuery{Author: "acc-user1", From: today, To: today})); len(wls) != 0 {
		t.Fatalf("deleted worklogs %+v", wls)
	}

	// History shows the status, sprint and assignee changes; estimates
	// are TestFakeScenarioEstimateHistory's.
	h := s.history(t, "10101", "10102")
	if st := h["10101"].Status; len(st) != 2 || st[1].To != "10002" {
		t.Fatalf("status history %+v", st)
	}
	if as := h["10102"].Assignee; len(as) != 1 || as[0].To != "acc-user2" {
		t.Fatalf("assignee history %+v", as)
	}
	if sp := h["10102"].Sprint; len(sp) != 1 || !slices.Equal(sp[0].To, []tracker.SprintID{sprint3}) {
		t.Fatalf("sprint history %+v", sp)
	}

	// The delta from the first walk's cursor is exactly the issues the
	// writes changed.
	delta, _ := s.drain(t, cursor, window)
	want := []tracker.IssueID{"10101", "10102", "10103", newID}
	slices.Sort(want)
	if !slices.Equal(rowIDs(delta), want) {
		t.Fatalf("delta %v, want %v", rowIDs(delta), want)
	}
}

// history is the board's history of issues, by issue.
func (s *scenario) history(t *testing.T, issues ...tracker.IssueID) map[tracker.IssueID]tracker.IssueHistory {
	t.Helper()
	h := scenarioMust[tracker.History](t)(s.p.History(s.ctx, tracker.HistoryQuery{Issues: issues, Boards: []tracker.BoardID{"1"}}))
	out := map[tracker.IssueID]tracker.IssueHistory{}
	for _, ih := range h.Issues {
		out[ih.Issue] = ih
	}
	return out
}

// TestFakeScenarioEstimateHistory reads back an estimate write as the
// board's estimate history. The fake records a custom number field's
// change in fromString and toString with from and to null, as Jira Cloud
// is believed to (unverified); the Provider reads only from and to.
func TestFakeScenarioEstimateHistory(t *testing.T) {
	s := newScenario(t, false)
	eight := 8.0
	allApplied(t, "estimate", s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: "1", Issue: "10101", Value: &eight}))
	es := s.history(t, "10101")["10101"].Estimate
	if len(es) != 1 || es[0].From == nil || *es[0].From != 5 || es[0].To == nil || *es[0].To != 8 {
		t.Fatalf("estimate history %+v, want one change from 5 to 8", es)
	}
}

// TestFakeScenarioJiraWorklogs runs logged time over Jira's own worklogs,
// as a connection without Tempo does.
func TestFakeScenarioJiraWorklogs(t *testing.T) {
	s := newScenario(t, false)
	tt := s.p.Time()
	today := scenarioMust[tracker.Date](t)(tracker.DateOf(s.clock.Now()))
	allApplied(t, "worklog add", tt.AddWorklog(s.ctx, tracker.WorklogInput{Issue: "10102", Day: today, Spent: 30 * time.Minute, Description: "Review"}))
	q := tracker.WorklogQuery{Author: "acc-user1", From: today, To: today}
	wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q))
	if len(wls) != 1 || wls[0].Issue != "10102" || wls[0].Spent != 30*time.Minute {
		t.Fatalf("worklogs %+v", wls)
	}
	hour := time.Hour
	allApplied(t, "worklog edit", tt.EditWorklog(s.ctx, tracker.WorklogEdit{From: wls[0], Spent: &hour}))
	if wls = scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q)); len(wls) != 1 || wls[0].Spent != time.Hour {
		t.Fatalf("edited worklogs %+v", wls)
	}
	allApplied(t, "worklog delete", tt.DeleteWorklog(s.ctx, tracker.WorklogDelete{From: wls[0]}))
	if wls := scenarioMust[[]tracker.Worklog](t)(tt.Worklogs(s.ctx, q)); len(wls) != 0 {
		t.Fatalf("deleted worklogs %+v", wls)
	}
}

// TestDetailReadsNothingBeforeItsVersion proves the comments a detail
// holds are read after the issue read that gives its card's update time
// (tracker.Capabilities.DetailMovesUpdated): a comment added while the
// issue's read is in flight, which moves that time, shows in the detail
// whose card has the moved time. The issue's read is held until the
// comment lands; a comments read made meanwhile, as when the two ran
// together, is answered before it, and the detail would pair the new
// update time with the old comments, which a later check of the card
// would then certify.
func TestDetailReadsNothingBeforeItsVersion(t *testing.T) {
	const issueRoute, commentsRoute = "GET rest/api/3/issue/{issue}", "GET rest/api/3/issue/{issue}/comment"
	seen := make(chan trackerfake.Request, 1024)
	s := newObservedScenario(t, false, func(r trackerfake.Request) {
		select {
		case seen <- r:
		default:
			t.Error("the requests outran the test")
		}
	})
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: issueRoute, Kind: trackerfake.FaultHold, Times: 1}); err != nil {
		t.Fatal(err)
	}
	type read struct {
		d   tracker.Detail
		err error
	}
	done := make(chan read, 1)
	go func() {
		d, err := s.p.Detail(s.ctx, "10102")
		done <- read{d, err}
	}()
	for r := range seen {
		if r.Route == issueRoute && r.Held {
			break
		}
	}
	// A comments read sent with the issue's arrives at once; the grace
	// bounds the wait for one that is never sent.
	early := false
	grace := time.After(300 * time.Millisecond)
wait:
	for {
		select {
		case r := <-seen:
			if r.Route == commentsRoute && r.Status != 0 {
				early = true
				break wait
			}
		case <-grace:
			break wait
		}
	}
	s.clock.advance(time.Minute)
	allApplied(t, "comment", s.p.AddComment(s.ctx, tracker.CommentInput{Issue: "10102", Markdown: "Added while read"}))
	if err := s.e.Release(origin, issueRoute, false); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if early {
		t.Error("the comments were read while the issue's read was in flight")
	}
	if !got.d.Card.Updated.Equal(s.clock.Now()) {
		t.Fatalf("the card's update time is %v, not the comment's %v", got.d.Card.Updated, s.clock.Now())
	}
	if !slices.ContainsFunc(got.d.Comments.Comments, func(c tracker.Comment) bool { return strings.TrimSpace(c.Body) == "Added while read" }) {
		t.Errorf("the card has the update time the comment moved it to, and the comments lack it: %+v", got.d.Comments.Comments)
	}
}
