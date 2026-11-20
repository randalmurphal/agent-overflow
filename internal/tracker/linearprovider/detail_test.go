package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func historiesVars(ids ...string) string {
	return js(map[string]any{"filter": map[string]any{"and": []any{map[string]any{"id": map[string]any{"in": ids}}}}, "first": 10, "entries": 50})
}

// History maps state, cycle, assignee and estimate entries oldest first,
// is complete only past the creation fold, reports an estimate on the
// board of the team the issue was in after the change, and leaves out an
// issue the answer omits.
func TestHistory(t *testing.T) {
	t.Parallel()
	created := "2026-11-01T09:00:00.000Z"
	entries := []string{ // newest first, as Linear sends them
		lineartest.HistoryEntry{ID: "h4", CreatedAt: "2026-11-05T09:00:00.000Z", Actor: "user-2", FromEstimate: f64(2), ToEstimate: f64(3)}.JSON(),
		lineartest.HistoryEntry{ID: "h3", CreatedAt: "2026-11-04T09:00:00.000Z", FromTeam: app, ToTeam: web}.JSON(),
		lineartest.HistoryEntry{ID: "h2", CreatedAt: "2026-11-03T09:00:00.000Z", Actor: "user-1", ToEstimate: f64(2), FromCycle: "c1", ToCycle: "c2"}.JSON(),
		lineartest.HistoryEntry{ID: "h1", CreatedAt: "2026-11-02T09:00:00.000Z", Actor: "user-1", FromState: "s-todo", ToState: "s-doing", ToAssignee: "user-1"}.JSON(),
	}
	// The clock moves an hour once the read is answered, so a CompleteTo
	// taken after the read would show it.
	began := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(began.UnixNano())
	p, _ := newProvider(t, lineartest.Exchange{Op: "IssueHistories", Vars: historiesVars(issueID(1), issueID(2)),
		Body:       page(lineartest.IssueChangesJSON(issueID(1), web, created, entries, false, "")),
		BeforeBody: func() { clock.Add(int64(time.Hour)) }})
	p.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	h, err := p.History(context.Background(), tracker.HistoryQuery{
		Issues: []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2)), "WEB-3"},
		Boards: []tracker.BoardID{web},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Issues) != 1 {
		t.Fatalf("history %+v", h)
	}
	ih := h.Issues[0]
	// Complete from the measured fold's upper bound after creation, final
	// through the settle margin before the read began.
	if want := time.Date(2026, 11, 1, 9, 3, 0, 0, time.UTC); !ih.CompleteFrom.Equal(want) {
		t.Errorf("complete from %v, want %v", ih.CompleteFrom, want)
	}
	if want := began.Add(-2 * time.Minute); !ih.CompleteTo.Equal(want) {
		t.Errorf("complete to %v, want %v", ih.CompleteTo, want)
	}
	if len(ih.Status) != 1 || ih.Status[0].From != "s-todo" || ih.Status[0].To != "s-doing" || ih.Status[0].Author != "user-1" {
		t.Errorf("status %+v", ih.Status)
	}
	if len(ih.Assignee) != 1 || ih.Assignee[0].From != "" || ih.Assignee[0].To != "user-1" {
		t.Errorf("assignee %+v", ih.Assignee)
	}
	if len(ih.Sprint) != 1 || fmt.Sprint(ih.Sprint[0].From, ih.Sprint[0].To) != "[c1] [c2]" {
		t.Errorf("sprint %+v", ih.Sprint)
	}
	// h2's estimate was on team app's board, which q does not name; h4's
	// is on web's.
	if len(ih.Estimate) != 1 || ih.Estimate[0].Board != web || *ih.Estimate[0].From != 2 || *ih.Estimate[0].To != 3 || ih.Estimate[0].Author != "user-2" {
		t.Errorf("estimate %+v", ih.Estimate)
	}
}

// A failed read returns no history, and no IDs send nothing.
func TestHistoryFailure(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t, lineartest.Exchange{Op: "IssueHistories", Vars: historiesVars(issueID(1)), Status: 500, Body: `{"errors":[{"message":"boom"}]}`})
	h, err := p.History(context.Background(), tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}})
	if err == nil || h.Issues != nil {
		t.Errorf("history %+v %v", h, err)
	}
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok {
		t.Errorf("the transport's error is not reachable: %v", err)
	}
	if h, err := p.History(context.Background(), tracker.HistoryQuery{Issues: []tracker.IssueID{"WEB-1"}}); err != nil || h.Issues != nil {
		t.Errorf("no issue IDs: %+v %v", h, err)
	}
}

// Detail maps the issue, its comments, every link past the first page,
// fields, moves and a history whose IDs are named by reads of exactly
// the IDs the issue and its entries name; the issue's own project name
// comes with the issue.
func TestDetail(t *testing.T) {
	t.Parallel()
	is := issue(1, 1)
	is.LabelIDs, is.Project, is.DueDate, is.State, is.Description = []string{"label-bug"}, "project-1", "2026-12-01", "s-todo", "Steps"
	self := lineartest.IssueRef{ID: issueID(1), Identifier: "WEB-1", Title: "T", State: "s-todo"}
	other := lineartest.IssueRef{ID: issueID(2), Identifier: "WEB-2", Title: "Other", State: "s-done"}
	third := lineartest.IssueRef{ID: issueID(3), Identifier: "WEB-3", Title: "Third", State: "s-todo"}
	detail := lineartest.DetailJSON(is, lineartest.Detail{
		ProjectName: "Launch",
		Comments:    []string{lineartest.NewComment().JSON()}, MoreComments: true, CommentCursor: "cc1",
		Relations:        []string{lineartest.RelationJSON("rel-1", "blocks", self, other)},
		RelationCursor:   "r1",
		InverseRelations: []string{lineartest.RelationJSON("rel-2", "similar", other, self)},
	})
	archived := true
	history := []string{
		lineartest.HistoryEntry{ID: "h3", CreatedAt: "2026-11-04T09:00:00.000Z", Archived: &archived, FromPriority: f64(0), ToPriority: f64(2),
			FromTeam: app, ToTeam: web}.JSON(),
		lineartest.HistoryEntry{ID: "h2", CreatedAt: "2026-11-03T09:00:00.000Z", Actor: "user-1", FromParent: issueID(2), AddedLabelIDs: []string{"label-bug"},
			RemovedLabelIDs: []string{"label-gone"}, FromTitle: "Old", ToTitle: "New", FromProject: "project-0", ToProject: "project-1"}.JSON(),
		lineartest.HistoryEntry{ID: "h1", CreatedAt: "2026-11-02T09:00:00.000Z", Actor: "user-1", FromState: "s-done", ToState: "s-todo",
			ToAssignee: "user-1", ToCycle: "c1", FromProject: "", ToProject: "project-1", ToEstimate: f64(3)}.JSON(),
	}
	parent := issue(2, 2)
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Issue", Vars: `{"id":"` + issueID(1) + `"}`, Body: `{"data":{"issue":` + detail + `}}`},
		lineartest.Exchange{Op: "IssueRelations", Vars: `{"id":"` + issueID(1) + `","first":100,"after":"r1"}`,
			Body: nodePage(lineartest.RelationJSON("rel-3", "related", self, third))},
		statesX(web, lineartest.State{ID: "s-todo", Name: "Todo", Type: "unstarted", Team: web},
			lineartest.State{ID: "s-done", Name: "Done", Type: "completed", Team: web}),
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + issueID(1) + `","first":100}`,
			Body: `{"data":{` + lineartest.NodePageJSON(history[:2], true, "h-c1") + `}}`},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + issueID(1) + `","first":100,"after":"h-c1"}`, Body: nodePage(history[2])},
		labelsByIDX([]string{"label-gone"}),
		byIDX("UsersByID", []string{"user-1"}, lineartest.User{ID: "user-1", Name: "User 1"}.JSON()),
		byIDX("CyclesByID", []string{"c1"}, lineartest.Cycle{ID: "c1", Number: 7, Team: web, StartsAt: at, EndsAt: at, Active: true}.JSON()),
		byIDX("ProjectsByID", []string{"project-0"}, lineartest.Project{ID: "project-0", Name: "Old"}.JSON()),
		byIDX("TeamsByID", []string{app, web}, lineartest.NewTeam("APP").JSON(), lineartest.NewTeam("WEB").JSON()),
		issuesX(byIDVars([]string{issueID(2)}, nil, true), parent),
	)
	d, err := p.Detail(context.Background(), tracker.IssueID(issueID(1)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Card.Key != "WEB-1" || d.Card.Labels[0].Name != "Bug" || d.Description != "Steps" || d.URL == "" ||
		!d.HistoryFrom.Equal(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC).Add(creationFold)) {
		t.Errorf("detail %+v", d)
	}
	if len(d.Comments.Comments) != 1 || d.Comments.Next != "cc1" || d.Comments.Comments[0].Author != "user-1" {
		t.Errorf("comments %+v", d.Comments)
	}
	if len(d.Links) != 3 || d.Links[1].Issue.Key != "WEB-3" || d.Links[1].Type.ID != "related" || !d.Links[0].Outward || d.Links[0].Issue.Key != "WEB-2" || d.Links[0].Type.Inward != "is blocked by" ||
		d.Links[2].Outward || d.Links[2].Issue.ID != tracker.IssueID(issueID(2)) || d.Links[2].Type.Outward != "similar to" {
		t.Errorf("links %+v", d.Links)
	}
	if len(d.Fields) != 2 || d.Fields[0].ID != FieldDueDate || d.Fields[0].Value.Dates()[0].String() != "2026-12-01" ||
		d.Fields[1].Value.Choices()[0] != (tracker.Choice{ID: "project-1", Name: "Launch"}) || !d.Fields[1].Editable {
		t.Errorf("fields %+v", d.Fields)
	}
	if len(d.Moves) != 1 || !reflect.DeepEqual(d.Moves[0], tracker.Move{ID: "s-done", Name: "Done", To: "s-done"}) || len(d.Statuses) != 2 {
		t.Errorf("moves %+v statuses %+v", d.Moves, d.Statuses)
	}
	var events []string
	for _, e := range d.History {
		events = append(events, e.Field+":"+e.From+">"+e.To)
	}
	want := "Status:Done>Todo Assignee:>User 1 Cycle:>Cycle 7 Project:>Launch Estimate:>3 " +
		"Project:Old>Launch Parent:WEB-2> Title:Old>New Labels:label-gone>Bug " +
		"Team:APP Web>WEB Web Priority:No priority>High Archived:>archived"
	if strings.Join(events, " ") != want {
		t.Errorf("events\n got %s\nwant %s", strings.Join(events, " "), want)
	}
	if d.History[0].Author != "user-1" || !d.History[0].At.Equal(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("first event %+v", d.History[0])
	}
}

// A detail with no labels, project or named changes reads no catalog.
func TestDetailReadsOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	is := issue(1, 1)
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Issue", Vars: `{"id":"` + issueID(1) + `"}`, Body: `{"data":{"issue":` + lineartest.DetailJSON(is, lineartest.Detail{}) + `}}`},
		statesX(web, lineartest.State{ID: "state-todo", Name: "Todo", Type: "unstarted", Team: web}),
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + issueID(1) + `","first":100}`, Body: nodePage()},
	)
	d, err := p.Detail(context.Background(), tracker.IssueID(issueID(1)))
	if err != nil || d.Moves != nil || d.Comments.Next != "" || !d.Fields[1].Value.Empty() {
		t.Errorf("detail %+v %v", d, err)
	}
}

func TestCommentsAndMoves(t *testing.T) {
	t.Parallel()
	c := lineartest.NewComment()
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "IssueComments", Vars: `{"id":"` + issueID(1) + `","first":100,"after":"cc1"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{c.JSON()}, true, "cc2") + `}}`},
		summaryX(issueID(1), issue(1, 1)),
		statesX(web, lineartest.State{ID: "state-todo", Name: "Todo", Type: "unstarted", Team: web},
			lineartest.State{ID: "s-doing", Name: "Doing", Type: "started", Team: web},
			lineartest.State{ID: "s-backlog", Name: "Backlog", Type: "backlog", Team: web}),
	)
	ctx := context.Background()
	pg, err := p.Comments(ctx, tracker.IssueID(issueID(1)), "cc1")
	if err != nil || len(pg.Comments) != 1 || pg.Next != "cc2" || pg.Comments[0].Body != c.Body {
		t.Fatalf("comments %+v %v", pg, err)
	}
	moves, err := p.Moves(ctx, tracker.IssueID(issueID(1)))
	if err != nil || len(moves) != 2 || moves[0].ID != "s-backlog" || moves[1].ID != "s-doing" {
		t.Fatalf("moves %+v %v", moves, err)
	}
	for name, call := range map[string]func() error{
		"comments": func() error { _, err := p.Comments(ctx, "", ""); return err },
		"moves":    func() error { _, err := p.Moves(ctx, ""); return err },
		"detail":   func() error { _, err := p.Detail(ctx, ""); return err },
	} {
		if err := call(); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s with no issue: %v", name, err)
		}
	}
}
