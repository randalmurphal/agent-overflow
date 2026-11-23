package jiraprovider

import (
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

const scenarioSite = "example.atlassian.net"

// scenarioWindow is the mirror's history window (trackerapp.HistoryWindow),
// the sprint window the scenarios read with.
const scenarioWindow = 180 * 24 * time.Hour

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
	opts := Options{Jira: jc, Site: "https://" + scenarioSite, SprintsFrom: func() time.Time { return clock.Now().Add(-scenarioWindow) }}
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
