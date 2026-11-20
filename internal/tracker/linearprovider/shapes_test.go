package linearprovider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// fieldShape is one field edit a create or an edit sends: the reads it
// needs, the input it sends (empty when refused before sending) and the
// result.
type fieldShape struct {
	name  string
	edit  tracker.FieldEdit
	reads []lineartest.Exchange
	wire  string
	want  string
}

// Reads the field shapes need.
var (
	shapeMembers  = membersX(web, lineartest.User{ID: "user-1", Name: "User 1", Active: true})
	shapeLabels   = labelsX(web, lineartest.Label{ID: "l-1", Name: "Bug"})
	shapeProjects = lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"` + web + `"}}}}}`,
		Body: page(lineartest.Project{ID: "project-1", Name: "Launch"}.JSON())}
)

func fe(field tracker.FieldID, e tracker.Edit) tracker.FieldEdit {
	return tracker.FieldEdit{Field: field, Value: e}
}

// Every form of every field Linear takes, on a create: what it sends or
// why it is refused. A clear of a member Linear fills when omitted (a
// sub-issue's project, priority and assignee; the due date) is sent as
// its clear; one Linear leaves empty when omitted is omitted.
func TestCreateFieldShapes(t *testing.T) {
	t.Parallel()
	refused := "-:create:refused(invalid)"
	created := "5:create:applied"
	md := tracker.MarkdownEdit
	for _, tc := range []fieldShape{
		{"title as a field", fe(tracker.FieldTitle, tracker.TypedValues("T")), nil, "", refused},
		{"description clear", fe(tracker.FieldDescription, tracker.ClearField()), nil, `{}`, created},
		{"description markdown", fe(tracker.FieldDescription, md("Body", "")), nil, `{"description":"Body"}`, created},
		{"description markdown empty", fe(tracker.FieldDescription, md("", "")), nil, `{}`, created},
		{"description picked", fe(tracker.FieldDescription, tracker.PickValues("x")), nil, "", refused},
		{"description typed", fe(tracker.FieldDescription, tracker.TypedValues("x")), nil, "", refused},
		{"assignee clear", fe(tracker.FieldAssignee, tracker.ClearField()), nil, `{"assigneeId":null}`, created},
		{"assignee picked", fe(tracker.FieldAssignee, tracker.PickValues("user-1")), nil, `{"assigneeId":"user-1"}`, created},
		{"assignee typed", fe(tracker.FieldAssignee, tracker.TypedValues("user 1")), []lineartest.Exchange{shapeMembers}, `{"assigneeId":"user-1"}`, created},
		{"assignee markdown", fe(tracker.FieldAssignee, md("x", "")), nil, "", refused},
		{"priority clear", fe(tracker.FieldPriority, tracker.ClearField()), nil, `{"priority":0}`, created},
		{"priority picked", fe(tracker.FieldPriority, tracker.PickValues("2")), nil, `{"priority":2}`, created},
		{"priority typed", fe(tracker.FieldPriority, tracker.TypedValues("high")), nil, `{"priority":2}`, created},
		{"priority markdown", fe(tracker.FieldPriority, md("x", "")), nil, "", refused},
		{"labels clear", fe(tracker.FieldLabels, tracker.ClearField()), nil, `{}`, created},
		{"labels picked", fe(tracker.FieldLabels, tracker.PickValues("l-1", "l-2")), nil, `{"labelIds":["l-1","l-2"]}`, created},
		{"labels typed", fe(tracker.FieldLabels, tracker.TypedValues("bug")), []lineartest.Exchange{shapeLabels}, `{"labelIds":["l-1"]}`, created},
		{"labels markdown", fe(tracker.FieldLabels, md("x", "")), nil, "", refused},
		{"parent clear", fe(tracker.FieldParent, tracker.ClearField()), nil, `{}`, created},
		{"parent picked", fe(tracker.FieldParent, tracker.PickValues(issueID(2))), nil, `{"parentId":"` + issueID(2) + `"}`, created},
		{"parent typed", fe(tracker.FieldParent, tracker.TypedValues("WEB-2")), nil, `{"parentId":"WEB-2"}`, created},
		{"parent markdown", fe(tracker.FieldParent, md("x", "")), nil, "", refused},
		{"due date clear", fe(FieldDueDate, tracker.ClearField()), nil, `{"dueDate":null}`, created},
		{"due date picked", fe(FieldDueDate, tracker.PickValues("2026-12-24")), nil, "", refused},
		{"due date typed", fe(FieldDueDate, tracker.TypedValues("2026-12-24")), nil, `{"dueDate":"2026-12-24"}`, created},
		{"due date markdown", fe(FieldDueDate, md("x", "")), nil, "", refused},
		{"project clear", fe(FieldProject, tracker.ClearField()), nil, `{"projectId":null}`, created},
		{"project picked", fe(FieldProject, tracker.PickValues("project-1")), nil, `{"projectId":"project-1"}`, created},
		{"project typed", fe(FieldProject, tracker.TypedValues("launch")), []lineartest.Exchange{shapeProjects}, `{"projectId":"project-1"}`, created},
		{"project markdown", fe(FieldProject, md("x", "")), nil, "", refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.reads
			if tc.wire != "" {
				script = append(script, lineartest.Exchange{Op: "CreateIssue", Vars: `{"input":` + withTeamAndTitle(t, tc.wire) + `}`, Body: applied(issue(5, 1).JSON())})
			}
			p, _ := newProvider(t, script...)
			expectResult(t, p.Create(context.Background(), tracker.CreateInput{Project: web, Title: "T", Fields: []tracker.FieldEdit{tc.edit}}), tc.want)
		})
	}
}

// withTeamAndTitle is the create input wire with team web and title T.
func withTeamAndTitle(t *testing.T, wire string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(wire), &m); err != nil {
		t.Fatal(err)
	}
	m["teamId"], m["title"] = web, "T"
	return js(m)
}

// Every form of every field Linear takes, on an edit: what it sends or
// why it is refused. Typed values that name a team's member, label or
// project read the issue for its team; a Markdown edit reads it for the
// text it was written from.
func TestEditFieldShapes(t *testing.T) {
	t.Parallel()
	refused := "1:edit:refused(invalid)"
	edited := "1:edit:applied"
	current := issue(1, 1)
	current.Description = "Steps"
	summary := summaryX(issueID(1), current)
	read := []lineartest.Exchange{summary}
	md := tracker.MarkdownEdit
	for _, tc := range []fieldShape{
		{"title clear", fe(tracker.FieldTitle, tracker.ClearField()), nil, "", refused},
		{"title picked", fe(tracker.FieldTitle, tracker.PickValues("x")), nil, "", refused},
		{"title typed", fe(tracker.FieldTitle, tracker.TypedValues("Renamed")), nil, `{"title":"Renamed"}`, edited},
		{"title typed blank", fe(tracker.FieldTitle, tracker.TypedValues("  ")), nil, "", refused},
		{"title markdown", fe(tracker.FieldTitle, md("x", "")), read, "", refused},
		{"description clear", fe(tracker.FieldDescription, tracker.ClearField()), nil, `{"description":""}`, edited},
		{"description markdown", fe(tracker.FieldDescription, md("New", "Steps")), read, `{"description":"New"}`, edited},
		{"description markdown empty", fe(tracker.FieldDescription, md("", "Steps")), read, `{"description":""}`, edited},
		{"description markdown stale", fe(tracker.FieldDescription, md("New", "Old")), read, "", "1:edit:refused(stale)"},
		{"description picked", fe(tracker.FieldDescription, tracker.PickValues("x")), nil, "", refused},
		{"description typed", fe(tracker.FieldDescription, tracker.TypedValues("x")), nil, "", refused},
		{"assignee clear", fe(tracker.FieldAssignee, tracker.ClearField()), nil, `{"assigneeId":null}`, edited},
		{"assignee picked", fe(tracker.FieldAssignee, tracker.PickValues("user-1")), nil, `{"assigneeId":"user-1"}`, edited},
		{"assignee typed", fe(tracker.FieldAssignee, tracker.TypedValues("User 1")), []lineartest.Exchange{summary, shapeMembers}, `{"assigneeId":"user-1"}`, edited},
		{"assignee markdown", fe(tracker.FieldAssignee, md("x", "")), read, "", refused},
		{"priority clear", fe(tracker.FieldPriority, tracker.ClearField()), nil, `{"priority":0}`, edited},
		{"priority picked", fe(tracker.FieldPriority, tracker.PickValues("4")), nil, `{"priority":4}`, edited},
		{"priority typed", fe(tracker.FieldPriority, tracker.TypedValues("urgent")), nil, `{"priority":1}`, edited},
		{"priority markdown", fe(tracker.FieldPriority, md("x", "")), read, "", refused},
		{"labels clear", fe(tracker.FieldLabels, tracker.ClearField()), nil, `{"labelIds":[]}`, edited},
		{"labels picked", fe(tracker.FieldLabels, tracker.PickValues("l-1")), nil, `{"labelIds":["l-1"]}`, edited},
		{"labels typed", fe(tracker.FieldLabels, tracker.TypedValues("Bug")), []lineartest.Exchange{summary, shapeLabels}, `{"labelIds":["l-1"]}`, edited},
		{"labels markdown", fe(tracker.FieldLabels, md("x", "")), read, "", refused},
		{"parent clear", fe(tracker.FieldParent, tracker.ClearField()), nil, `{"parentId":null}`, edited},
		{"parent picked", fe(tracker.FieldParent, tracker.PickValues(issueID(2))), nil, `{"parentId":"` + issueID(2) + `"}`, edited},
		{"parent typed", fe(tracker.FieldParent, tracker.TypedValues("WEB-2")), nil, `{"parentId":"WEB-2"}`, edited},
		{"parent typed blank", fe(tracker.FieldParent, tracker.TypedValues(" ")), nil, "", "1:edit:refused(no match)"},
		{"parent markdown", fe(tracker.FieldParent, md("x", "")), read, "", refused},
		{"due date clear", fe(FieldDueDate, tracker.ClearField()), nil, `{"dueDate":null}`, edited},
		{"due date picked", fe(FieldDueDate, tracker.PickValues("2026-12-24")), nil, "", refused},
		{"due date typed", fe(FieldDueDate, tracker.TypedValues("2026-12-24")), nil, `{"dueDate":"2026-12-24"}`, edited},
		{"due date markdown", fe(FieldDueDate, md("x", "")), read, "", refused},
		{"project clear", fe(FieldProject, tracker.ClearField()), nil, `{"projectId":null}`, edited},
		{"project picked", fe(FieldProject, tracker.PickValues("project-1")), nil, `{"projectId":"project-1"}`, edited},
		{"project typed", fe(FieldProject, tracker.TypedValues("Launch")), []lineartest.Exchange{summary, shapeProjects}, `{"projectId":"project-1"}`, edited},
		{"project markdown", fe(FieldProject, md("x", "")), read, "", refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.reads
			if tc.wire != "" {
				script = append(script, updateX(issueID(1), tc.wire, applied(current.JSON())))
			}
			p, _ := newProvider(t, script...)
			r := p.Edit(context.Background(), tracker.EditInput{Issue: tracker.IssueID(issueID(1)), Fields: []tracker.FieldEdit{tc.edit}})
			expectResult(t, r, tc.want)
		})
	}
}

// The members of the write inputs Check permits that no other test sets:
// each is sent as Linear can take it or refused before any request.
func TestWriteInputShapes(t *testing.T) {
	t.Parallel()
	c := lineartest.NewComment()
	c.Issue = issueID(1)
	states := statesX(web, lineartest.State{ID: "s-done", Name: "Done", Type: "completed", Team: web})
	ref := func(n int) lineartest.IssueRef { return lineartest.IssueRef{ID: issueID(n), Identifier: "WEB"} }
	p, _ := newProvider(t,
		updateX(issueID(1), `{"title":"T"}`, applied(issue(1, 1).JSON())),
		summaryX(issueID(1), issue(1, 1)), states, updateX(issueID(1), `{"stateId":"s-done"}`, applied(issue(1, 1).JSON())),
		commentX(c),
		commentX(c), lineartest.Exchange{Op: "UpdateComment", Vars: `{"id":"comment-2","input":{"body":"New"}}`, Body: applied(c.JSON())},
		lineartest.Exchange{Op: "CreateRelation", Vars: `{"input":{"issueId":"` + issueID(1) + `","relatedIssueId":"` + issueID(2) + `","type":"duplicate"}}`,
			Body: applied(lineartest.RelationJSON("rel-d", "duplicate", ref(1), ref(2)))},
		lineartest.Exchange{Op: "CreateRelation", Vars: `{"input":{"issueId":"` + issueID(1) + `","relatedIssueId":"` + issueID(2) + `","type":"related"}}`,
			Body: applied(lineartest.RelationJSON("rel-r", "related", ref(1), ref(2)))},
	)
	ctx := context.Background()
	id := tracker.IssueID(issueID(1))
	start, end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	goal := "Ship"
	for _, tc := range []struct {
		name string
		r    tracker.Result
		want string
	}{
		{"edit accepting loss", p.Edit(ctx, tracker.EditInput{Issue: id, AcceptLoss: true,
			Fields: []tracker.FieldEdit{fe(tracker.FieldTitle, tracker.TypedValues("T"))}}), "1:edit:applied"},
		{"edit with images", p.Edit(ctx, tracker.EditInput{Issue: id, ImageDir: "/tmp/x",
			Fields: []tracker.FieldEdit{fe(tracker.FieldTitle, tracker.TypedValues("T"))}}), "1:edit:refused(unsupported)"},
		{"move accepting loss", p.Move(ctx, tracker.MoveInput{Issue: id, Move: "s-done", AcceptLoss: true}), "1:move:applied"},
		{"move with images", p.Move(ctx, tracker.MoveInput{Issue: id, Move: "s-done", ImageDir: "/tmp/x"}), "1:move:refused(unsupported)"},
		{"internal comment", p.AddComment(ctx, tracker.CommentInput{Issue: id, Markdown: "Hi", Audience: tracker.AudienceInternal}), "1:comment:refused(invalid)"},
		{"comment with images", p.AddComment(ctx, tracker.CommentInput{Issue: id, Markdown: "Hi", ImageDir: "/tmp/x"}), "1:comment:refused(unsupported)"},
		{"comment edit with images", p.EditComment(ctx, tracker.CommentEdit{Issue: id, Comment: "comment-2", Markdown: "New", From: c.Body, ImageDir: "/tmp/x"}),
			"1:comment:refused(unsupported)"},
		{"comment edit from empty", p.EditComment(ctx, tracker.CommentEdit{Issue: id, Comment: "comment-2", Markdown: "New"}), "1:comment:refused(stale)"},
		{"comment edit accepting loss", p.EditComment(ctx, tracker.CommentEdit{Issue: id, Comment: "comment-2", Markdown: "New", From: c.Body, AcceptLoss: true}),
			"1:comment:applied"},
		{"duplicate link", p.Link(ctx, tracker.LinkInput{Type: "duplicate", Outward: id, Inward: tracker.IssueID(issueID(2))}), "1:link:applied"},
		{"related link", p.Link(ctx, tracker.LinkInput{Type: "related", Outward: id, Inward: tracker.IssueID(issueID(2))}), "1:link:applied"},
		{"two files", p.Attach(ctx, tracker.AttachInput{Issue: id, Files: []tracker.File{{Name: "a", Content: strings.NewReader("a")}, {Name: "b", Content: strings.NewReader("b")}}}),
			"1:upload:refused(unsupported)"},
		{"sprint without dates", p.CreateSprint(ctx, tracker.SprintInput{Board: web, Name: "Next"}), "-:sprint:refused(unsupported)"},
		{"completion into a sprint", p.CompleteSprint(ctx, tracker.SprintCompletion{Board: web, Sprint: "c1", MoveTo: "c2"}), "-:sprint:refused(unsupported)"},
		{"sprint goal", p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "c1", Goal: &goal}), "-:sprint:refused(unsupported)"},
		{"sprint start", p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "c1", Start: &start}), "-:sprint:refused(unsupported)"},
		{"sprint end", p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "c1", End: &end}), "-:sprint:refused(unsupported)"},
		{"sprint dates", p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "c1", Start: &start, End: &end}), "-:sprint:refused(unsupported)"},
	} {
		if got := describe(tc.r); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
		checkResult(t, tc.r)
	}
}

// A delta with no cursor and no start polls every issue of the team; a
// walk that ends on an empty page after a page that promised more ends
// with More false and a cursor that starts the next walk at the last
// update delivered.
func TestChangedRowsWalkEnds(t *testing.T) {
	t.Parallel()
	all := js(map[string]any{"first": 100, "sort": []any{map[string]any{"updatedAt": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}}}}})
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	last := issue(1, 1)
	last.UpdatedAt = "2026-11-03T08:00:00.000Z"
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Issues", Vars: all, Body: page()},
		lineartest.Exchange{Op: "Issues", Vars: pollVars(start, ""), Body: lineartest.Data(lineartest.PageJSON([]string{last.JSON()}, true, "c1"))},
		lineartest.Exchange{Op: "Issues", Vars: pollVars(start, "c1"), Body: page()},
	)
	ctx := context.Background()
	if pg, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: web}); err != nil || len(pg.Rows) != 0 || pg.More {
		t.Fatalf("a walk from nothing %+v %v", pg, err)
	}
	pg, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: web, Start: start})
	if err != nil || len(pg.Rows) != 1 || !pg.More {
		t.Fatalf("first page %+v %v", pg, err)
	}
	pg, err = p.ChangedRows(ctx, tracker.DeltaQuery{Board: web, From: pg.Next})
	if err != nil || len(pg.Rows) != 0 || pg.More {
		t.Fatalf("empty last page %+v %v", pg, err)
	}
	var cur cursorJSON
	want := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	if err := json.Unmarshal(pg.Next, &cur); err != nil || !cur.Since.Equal(want) || !cur.Floor.Equal(want) || cur.After != "" {
		t.Errorf("cursor after the walk %s %v", pg.Next, err)
	}
}

// A history query that names no board reports no estimate change, since
// an estimate belongs to a board.
func TestHistoryWithoutBoards(t *testing.T) {
	t.Parallel()
	entries := []string{lineartest.HistoryEntry{ID: "h1", CreatedAt: "2026-11-05T09:00:00.000Z", FromEstimate: f64(2), ToEstimate: f64(3), FromState: "a", ToState: "b"}.JSON()}
	p, _ := newProvider(t, lineartest.Exchange{Op: "IssueHistories", Vars: historiesVars(issueID(1)),
		Body: page(lineartest.IssueChangesJSON(issueID(1), web, "2026-11-01T09:00:00.000Z", entries, false, ""))})
	h, err := p.History(context.Background(), tracker.HistoryQuery{Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}})
	if err != nil || len(h.Issues) != 1 || len(h.Issues[0].Estimate) != 0 || len(h.Issues[0].Status) != 1 {
		t.Errorf("history %+v %v", h, err)
	}
}
